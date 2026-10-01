package sandbox

// container_start.go is the container backend's Starter half: a long-lived
// process (a local MCP server, TEN-318) in one `docker run -i --rm` with the
// same hardening as Run. It is Phase 3 groundwork; a profile opts a server in
// with "sandbox": "container".

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Start runs argv as a long-lived server in a hardened container. The container
// lives until the process exits or Close kills it; --rm and the kill-on-close
// release remove it. argv[0] is the program inside the container, run directly
// (no shell), so an MCP server's stdio is exactly its own.
func (b *containerBackend) Start(ctx context.Context, argv []string, pol Policy) (*Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("container: empty command")
	}
	bin, err := b.resolvedBin()
	if err != nil {
		return nil, fmt.Errorf("container: %s CLI not found: %w", b.bin(), err)
	}
	spec, err := b.specFor(Command{Args: argv, Surface: "mcp"}, pol)
	if err != nil {
		return nil, err
	}
	// Build the run argv, then replace the trailing `sh -c <command>` with the
	// server's own argv so its stdio is unwrapped.
	base, err := buildRunArgv(spec)
	if err != nil {
		return nil, err
	}
	base = trimTrailingShell(base, spec.image)
	full := append(base, argv...)

	c := exec.Command(bin, full...)
	if pol.WorkDir != "" {
		c.Dir = "" // the container's -w sets the work dir; the CLI runs from anywhere
	}
	release := func() { _ = b.reap(spec.name) }
	return startProcess(c, pol.Stderr, nil, release)
}

// trimTrailingShell drops the `sh -c <command>` buildRunArgv appended after the
// image, leaving the argv up to and including the image, so Start can append the
// server's own argv instead.
func trimTrailingShell(argv []string, image string) []string {
	for i, a := range argv {
		if a == image {
			return argv[:i+1]
		}
	}
	return argv
}

var _ = strings.TrimSpace
