// Package crm wraps the operator's external `crm-tool` binary as a set of
// gated Tenant tools. The binary (subcommands: search, lookup, ask, align,
// history, show, commitments-list) reads the operator's ~/.assistant DB; it
// lives on the OPERATOR's machine, not in this repo. The agent must route
// through crm-tool ONLY — never hand-write SQL — so this wrapper exposes the
// binary's subcommands and nothing else.
package crm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second
	maxOutputBytes = 16000 // cap stdout fed back to the model
	maxErrPreview  = 2000  // cap stderr surfaced in an error
)

// allowedSubcommands is the deny-by-default execution allowlist. ONLY these
// subcommands may ever reach the binary; anything else is rejected BEFORE
// exec. This is a hard boundary in Go — it does not depend on the model's
// intent or on the gating Policy (which decides read-vs-mutate separately).
var allowedSubcommands = map[string]bool{
	"search":           true,
	"lookup":           true,
	"ask":              true,
	"align":            true,
	"history":          true,
	"show":             true,
	"commitments-list": true,
}

// runner executes the configured binary with a subcommand + positional args
// and returns combined output. Injectable so tests use a fake and never spawn
// the real (operator-only) binary. The name is the subcommand; args are the
// trailing positional argv — there is NO shell, so nothing is interpolated.
type runner func(ctx context.Context, name string, args []string) ([]byte, error)

// Service is the opened crm-tool wrapper.
type Service struct {
	path    string
	timeout time.Duration
	run     runner
}

// realRunner runs `<path> <subcommand> <args...>` with positional argv and a
// timeout. No shell: exec.CommandContext takes argv directly, so there is no
// interpolation, globbing, or word-splitting. stdout and stderr are captured
// separately — stderr becomes the error preview, stdout the result.
func (s *Service) realRunner(ctx context.Context, name string, args []string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	argv := append([]string{name}, args...)
	cmd := exec.CommandContext(cctx, s.path, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return stdout.Bytes(), fmt.Errorf("crm-tool %s timed out after %s", name, s.timeout)
	}
	if err != nil {
		// A non-zero exit is signal, not a transport failure — surface the
		// stderr preview so the model can react (e.g. bad args).
		preview := clip(strings.TrimSpace(stderr.String()), maxErrPreview)
		if preview == "" {
			preview = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("crm-tool %s failed: %s", name, preview)
	}
	return stdout.Bytes(), nil
}

// Exec validates the subcommand against the allowlist, then runs it. The
// allowlist check happens BEFORE exec, so a disallowed subcommand never
// touches the binary. Returns the (capped) stdout.
func (s *Service) Exec(ctx context.Context, subcommand string, args ...string) (string, error) {
	sub := strings.TrimSpace(subcommand)
	if sub == "" {
		return "", errors.New("crm: subcommand is required")
	}
	if !allowedSubcommands[sub] {
		return "", fmt.Errorf("crm: subcommand %q is not allowed (allowed: %s)", sub, allowedList())
	}
	out, err := s.run(ctx, sub, args)
	res := clip(string(out), maxOutputBytes)
	if err != nil {
		return res, err
	}
	if strings.TrimSpace(res) == "" {
		res = "(no output)"
	}
	return res, nil
}

func allowedList() string {
	// Stable, readable order for the error message.
	order := []string{"search", "lookup", "ask", "align", "history", "show", "commitments-list"}
	return strings.Join(order, ", ")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[output truncated]"
}
