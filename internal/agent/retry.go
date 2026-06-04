package agent

// RetryDecorator wraps a ToolDispatcher and silently retries a small
// allowlist of transient transport failures (web/embedder/sql timeouts,
// connection refused, SQLite BUSY) ONCE before surfacing them to the
// model. The goal: stop burning model turns on infra noise the model
// has no useful judgment about.
//
// Design constraints (earned from the PRO/CON debate + tertiary review):
//
//   - Bounded compute, never unbounded. ONE retry max per call, bounded
//     by ctx cancellation, deterministic backoff.
//
//   - Plugins in tenant wrap transport failures into (string, true, nil)
//     so the model can act on them (see internal/plugins/web/dispatch.go:158
//     comment + docs/PLUGINS.md §1). That means Eligible MUST inspect
//     (result, isErr, err), not just err — otherwise the decorator is a
//     no-op for the very transports it's meant to retry. The
//     DefaultEligibleTransient policy does substring matches on the
//     result string when err==nil && isErr.
//
//   - Hard deny list for side-effectful tools (sql_exec, web_click,
//     web_fill, web_select, x_post, imessage_send, gsuite_send). Retrying
//     a failed INSERT or form-submit could silently corrupt state. Read
//     tools (web_navigate, web_text, sql_query, embeddings) are the only
//     eligible class.
//
//   - Retries surface to the activity feed via EventRetry, NOT via
//     *slog.Logger — the operator-facing canary that "frequent retries
//     on X" mitigates the masking-failures concern lives in the TUI,
//     not in server logs.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"tenant/internal/model"
)

// EligibilityFunc decides whether to retry a tool call given its
// (result, isErr, err) outcome. Returning true triggers ONE retry
// after backoff; returning false short-circuits to today's behavior.
type EligibilityFunc func(toolName, result string, isErr bool, err error) bool

// RetryDecorator wraps any ToolDispatcher with bounded retry semantics.
// Zero-value safe: nil Eligible disables retry (pure passthrough);
// MaxRetries=0 also disables. So this is opt-in by construction.
type RetryDecorator struct {
	Inner      ToolDispatcher
	Eligible   EligibilityFunc
	Backoff    time.Duration // first retry waits this long; doubled each subsequent
	MaxRetries int           // hard cap; 0 = passthrough
	Observer   func(Event)   // optional: receives EventRetry for the activity feed
}

// Dispatch implements ToolDispatcher.
func (r *RetryDecorator) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if r.Inner == nil {
		return "", true, errors.New("retry decorator: nil Inner dispatcher")
	}
	// Decorator opt-out paths: keep today's behavior exactly when retry
	// is unconfigured. Zero-value RetryDecorator{Inner: x} is a passthrough.
	if r.Eligible == nil || r.MaxRetries <= 0 {
		return r.Inner.Dispatch(ctx, call)
	}
	backoff := r.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for attempt := 0; attempt <= r.MaxRetries; attempt++ {
		result, isErr, err := r.Inner.Dispatch(ctx, call)
		// Clean success → return immediately.
		if err == nil && !isErr {
			return result, false, nil
		}
		// Not eligible OR out of attempts → surface today's behavior.
		if !r.Eligible(call.Name, result, isErr, err) || attempt == r.MaxRetries {
			return result, isErr, err
		}
		// Eligible transient. Emit so the operator sees it in the feed
		// (the doctor canary depends on this).
		if r.Observer != nil {
			reason := ""
			switch {
			case err != nil:
				reason = err.Error()
			case isErr:
				reason = result
			}
			r.Observer(Event{
				Kind:  EventRetry,
				Tool:  call.Name,
				Text:  fmt.Sprintf("attempt %d/%d: %s", attempt+1, r.MaxRetries, reason),
				IsErr: true,
			})
		}
		// Backoff with ctx-cancel guard. Doubling is bounded by MaxRetries.
		select {
		case <-time.After(backoff << attempt):
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	// Unreachable given the loop terminates on attempt==MaxRetries above,
	// but the compiler can't prove it. Return a safe zero.
	return "", false, nil
}

// DefaultEligibleTransient is the shipped eligibility policy. Inspects
// both typed err and the wrapped-string result. Hard-denies any tool
// that mutates state (retrying a failed INSERT would corrupt). The
// transient set is intentionally narrow — only errors the model has
// no useful judgment about. Anything else falls through to today's
// behavior (the model sees it, re-plans).
func DefaultEligibleTransient(toolName, result string, isErr bool, err error) bool {
	// 1. Hard deny: never retry side-effectful tools. Retrying a partial
	//    write is worse than surfacing the error.
	if mutatingTools[toolName] {
		return false
	}
	// 2. Typed-err path. ctx.Canceled means the caller asked us to stop;
	//    DeadlineExceeded means the underlying I/O timed out and may
	//    succeed on retry.
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return false
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		// Any other typed err: surface as-is. The model can decide.
		return false
	}
	// 3. err==nil + !isErr means success; we shouldn't be here, but
	//    defensive: pass through.
	if !isErr {
		return false
	}
	// 4. err==nil + isErr=true: the dispatcher wrapped a transport
	//    failure into a string. Substring-match the known transient set.
	lower := strings.ToLower(result)
	for _, transient := range transientResultSubstrings {
		if strings.Contains(lower, transient) {
			return true
		}
	}
	return false
}

// mutatingTools — explicit deny list. Edit when adding new mutation tools.
// Anything that performs a write, post, send, or other side effect MUST
// be here, or a flaky network could double-fire it on retry.
var mutatingTools = map[string]bool{
	"sql_exec":             true,
	"web_click":            true,
	"web_fill":             true,
	"web_select":           true,
	"x_post":               true,
	"imessage_send":        true,
	"gsuite_send":          true,
	"gsuite_send_email":    true, // alias safety; harmless if name doesn't exist
	"memory_remember":      true, // any memory mutation
	"discord_send_message": true, // posts publicly as the bot — never silently retry
	"discord_react":        true, // adds a reaction — never silently retry
}

// transientResultSubstrings — lower-cased substrings that flag a result
// string as a transient transport failure. Substring-based because plugins
// wrap errors as "navigation failed: context deadline exceeded" etc.
// Note: "database is locked" matches modernc.org/sqlite SQLITE_BUSY shape;
// validated against the only driver shipped (internal/plugins/sql/sql.go).
var transientResultSubstrings = []string{
	"context deadline exceeded",
	"connection refused",
	"i/o timeout",
	"no such host",
	"connection reset",
	"database is locked", // SQLITE_BUSY
	"timeout exceeded",
}
