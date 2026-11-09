package sandbox

// container_image.go builds the sandbox image from a Dockerfile embedded in the
// binary (plan §3.6). No registry is needed: the operator runs
// `tenant sandbox image build`, which builds tenant-sandbox:latest locally from a
// digest-pinned base. The backend runs only a local image and never pulls
// (--pull never), so the build is the one place a registry is contacted.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// baseImageDigest pins the Debian base by its index digest (linux/amd64 verified
// on the target box). trixie is the current stable line; bookworm left regular
// security support on 2026-07-12 (H24 recommends the move, tracked there). The
// pin is what makes the build reproducible; verify checks the built image id.
const baseImageRef = "debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251"

// imageDockerfile is the sandbox image: the pinned base, a non-root uid 65532
// user, the common tools an agent command expects, and a final pass that strips
// every setuid and setgid bit so no in-image binary can raise privileges (the
// probe showed setuid still reaches root without no-new-privileges; the image
// carries none regardless, defence in depth).
const imageDockerfile = "FROM " + baseImageRef + `
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        bash coreutils ca-certificates curl git jq python3; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd -g 65532 sandbox; \
    useradd -u 65532 -g 65532 -M -d /home/sandbox -s /bin/bash sandbox; \
    find / -xdev -perm /6000 -type f -exec chmod a-s {} + || true
USER 65532:65532
`

// BuildImage builds tag from the embedded Dockerfile using the given CLI and
// returns the built image's id. It streams the build output to progress when
// non-nil. The build contacts a registry for the pinned base only.
func BuildImage(ctx context.Context, cli, tag string, progress func(string)) (string, error) {
	if cli == "" {
		cli = "docker"
	}
	if tag == "" {
		tag = defaultImage
	}
	c := exec.CommandContext(ctx, cli, "build", "-t", tag, "-")
	c.Stdin = strings.NewReader(imageDockerfile)
	var out strings.Builder
	if progress != nil {
		c.Stdout = lineWriter(progress)
		c.Stderr = lineWriter(progress)
	} else {
		c.Stdout = &out
		c.Stderr = &out
	}
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("build %s: %w: %s", tag, err, oneLine(out.String()))
	}
	return imageID(ctx, cli, tag)
}

// VerifyImage checks that tag is present and reports its id and the base it was
// built from, so an operator can confirm the pin.
func VerifyImage(ctx context.Context, cli, tag string) (id string, err error) {
	if cli == "" {
		cli = "docker"
	}
	if tag == "" {
		tag = defaultImage
	}
	// The operator's own `image verify`: the bare CLI, as `image build` uses it
	// (the backend's runs go through containerBackend.command with -H).
	if err := imagePresent(exec.CommandContext(ctx, cli, "image", "inspect", tag)); err != nil {
		return "", fmt.Errorf("image %s is not built: run `tenant sandbox image build`", tag)
	}
	return imageID(ctx, cli, tag)
}

func imageID(ctx context.Context, cli, tag string) (string, error) {
	out, err := exec.CommandContext(ctx, cli, "image", "inspect", "--format", "{{.Id}}", tag).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// BaseImageRef is the pinned base the sandbox image is built from, for the
// operator CLI to display.
func BaseImageRef() string { return baseImageRef }

// DefaultImageTag is the image the backend runs when a profile names none.
func DefaultImageTag() string { return defaultImage }

// lineWriter adapts a func(string) to an io.Writer that calls it per write.
type lineWriter func(string)

func (w lineWriter) Write(p []byte) (int, error) {
	w(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
