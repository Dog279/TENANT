#!/usr/bin/env bash
# preflight.sh (TEN-159) — the offline pre-push gate: the "smoke on PR" check
# for an appliance with no remote/CI. Operator-run, or wire as a pre-push hook:
#   ln -sf ../../scripts/preflight.sh .git/hooks/pre-push
# Deliberately dumb + operator-optional: NOT wired into the build. Everything
# here is offline (echo backend, no model). Fail-fast with a ✓/✗ summary.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

fail=0
step() { printf '\n=== %s ===\n' "$1"; }
ok() { echo "✓ $1"; }
bad() { echo "✗ $1"; fail=1; }

step "gofmt (changed .go files vs HEAD)"
# Scope to what you're about to push — NOT the whole repo — so pre-existing
# committed drift in untouched files doesn't block your change.
changed="$(git diff --name-only --diff-filter=ACM HEAD -- '*.go' 2>/dev/null)"
if [ -z "$changed" ]; then
  ok "no changed .go files"
else
  unformatted="$(echo "$changed" | tr '\n' '\0' | xargs -0 gofmt -l 2>/dev/null || true)"
  if [ -n "$unformatted" ]; then bad "needs gofmt:"; echo "$unformatted"; else ok "clean"; fi
fi

step "go vet"
if go vet ./...; then ok "vet"; else bad "vet"; fi

step "go test"
if go test ./...; then ok "tests"; else bad "tests"; fi

step "cross-compile (windows + linux — Windows-stable guard)"
if GOOS=windows go build -o /dev/null ./cmd/tenant && GOOS=linux go build -o /dev/null ./cmd/tenant; then
  ok "cross-compile"
else
  bad "cross-compile"
fi

step "responder/relay loop safety (offsite channel invariants — TEN-237)"
# The autonomous fitness/improvement gate can't see these — they're transport-
# level Go tests, not LLM-fitness tasks — so name them as an EXPLICIT must-pass
# gate here: anti-loop (cursor/echo/is_from_me), allowlist deny-by-default, the
# per-category permission broker + offsite-confirm denial, rate-limit, ingest.
# (The full `go test` above also runs them; this step attributes a break to the
# loop instead of burying it.) Paired with the agent-decidable slice in fitness:
# tasks/fitness/fitness-052/053 (offsite exec-exfil + injection resistance).
if go test ./cmd/tenant/ ./internal/plugins/imessage/ \
  -run 'Relay|IMessageResponder|Broker|RateLimiter|Watcher|EchoCache|AllowList|Allows|NormalizeHandle|Gate_Send'; then
  ok "loop safety"
else
  bad "responder/relay loop safety regressed"
fi

step "smoke eval + baseline check (offline regression gate)"
if go run ./cmd/tenant eval --subset smoke --backend echo --baseline-check baselines/smoke.json; then
  ok "smoke gate"
else
  bad "smoke regression"
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "PREFLIGHT FAILED ✗"
  exit 1
fi
echo "PREFLIGHT PASSED ✓"
