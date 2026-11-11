package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// container_network.go: the network a `full` command runs on (W2-4, H18).
//
// `full` used to mean Docker's default bridge. Every container started
// without a network of its own is on that bridge, and containers on it reach
// each other: on a real engine a sandboxed command fetched a page from
// another container there. So a `full` command could talk to whatever else
// the operator runs in Docker (a database, a cache) and to other sandboxed
// commands.
//
// It now runs on a bridge Tenant makes for itself, with traffic between its
// containers turned off. Docker keeps separate bridges apart, so a command
// there reaches neither the default bridge's containers nor another sandbox,
// and still reaches the internet. This does not close the path from `full`
// to the host's own loopback services (host.docker.internal): that is the
// network scope per run (W2-9).

// sandboxNetwork is the bridge's name on the engine.
const sandboxNetwork = "tenant-sbx"

// iccOption is the bridge driver's switch for traffic between a network's
// containers; iccTemplate reads it back from `network inspect`.
const (
	iccOption   = "com.docker.network.bridge.enable_icc"
	iccTemplate = `{{index .Options "` + iccOption + `"}}`
)

// networkCheckTTL is how long a network seen to be right is trusted before it
// is looked at again. An operator who removes it in between gets one failed
// command ("network not found"), and the next look recreates it.
const networkCheckTTL = 10 * time.Minute

const fixNetwork = "remove the network (`docker network rm " + sandboxNetwork + "`); Tenant makes it again, with traffic between its containers off, on the next command that uses the network"

// ensureNetwork makes sure the sandbox bridge exists as Tenant makes it, and
// creates it when it is missing. A network of that name that lets its
// containers reach each other is not used: the run is refused with the fix,
// because Tenant will not reconfigure or remove a network it did not verify
// as its own.
func (b *containerBackend) ensureNetwork(ctx context.Context) error {
	if b.cache.networkFresh() {
		return nil
	}
	icc, found, err := b.networkICC(ctx)
	if err != nil {
		return err
	}
	if !found {
		c, done, err := b.control(ctx, "creating the sandbox network", "network", "create",
			"--driver", "bridge", "-o", iccOption+"=false", "--label", labelSandbox+"=1", sandboxNetwork)
		if err != nil {
			return unavailable(err, fixEndpoint)
		}
		out, runErr := c.CombinedOutput()
		createErr := done(runErr)
		// Created, or another process made it in the same instant: either
		// way, what counts is what is there now.
		icc, found, err = b.networkICC(ctx)
		if err != nil {
			return err
		}
		if !found {
			return unavailable(fmt.Errorf("could not create the sandbox network %s: %v: %s", sandboxNetwork, createErr, oneLine(string(out))), fixStartEngine)
		}
	}
	if icc != "false" {
		return unavailable(fmt.Errorf("a network named %s exists, but its containers can reach each other (%s=%q): it was not made by Tenant, or was changed", sandboxNetwork, iccOption, icc), fixNetwork)
	}
	b.cache.recordNetwork()
	return nil
}

// networkICC reads the sandbox network's inter-container setting. found is
// false when the engine has no such network.
func (b *containerBackend) networkICC(ctx context.Context) (icc string, found bool, err error) {
	c, done, err := b.control(ctx, "a network check", "network", "inspect", "--format", iccTemplate, sandboxNetwork)
	if err != nil {
		return "", false, unavailable(err, fixEndpoint)
	}
	out, runErr := c.Output()
	if err := done(runErr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", false, unavailable(err, fixStuckEngine)
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", false, nil // the CLI ran and said no: there is no such network
		}
		return "", false, unavailable(err, fixInstallEngine)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// networkFresh reports a network check passed within networkCheckTTL.
func (c *probeCache) networkFresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.networkAt.IsZero() && c.clock().Sub(c.networkAt) < networkCheckTTL
}

func (c *probeCache) recordNetwork() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.networkAt = c.clock()
}
