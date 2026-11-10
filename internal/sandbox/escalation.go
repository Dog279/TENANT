package sandbox

// escalation.go is the escalation half of W1-6 (TEN-460; hardening plan §4,
// decision D3): what happens after a backend refuses something and a person
// approves the command anyway.
//
//   - Within the tier first. A backend that can widen its own boundary by one
//     grant and re-run (TierEscalator) does that: the same `sh` line, in a
//     fresh container, with the one refused path granted or with the network
//     for this one command. The host is reached only by a program refusal
//     (a Windows program a Linux container cannot run), run verbatim.
//   - The language is kept. A line written for the backend's shell never runs
//     under another: Execute compares the backend's CommandShell with the
//     host's and refuses the re-run rather than hand an sh line to PowerShell.
//   - The prompt is honest and the approval is bound. The approval prompt shows
//     the exact interpreter and command, untruncated, with control characters
//     escaped (EscapeControl), and a risk summary Tenant writes, never the
//     model (RiskSummary). An approval is bound to a hash of the interpreter,
//     the argv, the mounts, the network and the image digest (EscalationHash),
//     and it expires (EscalationTicket): a changed command, mount or image
//     cannot ride on an earlier yes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
)

// TierEscalator is a backend that can re-run a refused command with exactly
// one more grant, inside its own boundary. Execute prefers it to the host for
// filesystem and network refusals (D3).
type TierEscalator interface {
	RunGranted(ctx context.Context, cmd Command, pol Policy, refused Result) (Result, error)
}

// EscalationBinder is a TierEscalator that can name, as one digest, exactly
// what an approved re-run would be: the interpreter, the command, every mount,
// the network and the image (EscalationHash). Execute takes the digest before
// it asks and again immediately before the run, and runs nothing when the two
// differ or the approval is older than EscalationTTL: an approval covers the
// run the operator was shown, not whatever the same request has become while
// the prompt waited (an image tag moved, a mount source re-pointed).
type EscalationBinder interface {
	GrantedRunHash(ctx context.Context, cmd Command, pol Policy, refused Result) (string, error)
}

// PinnedEscalator is an EscalationBinder that starts the very run it
// describes. RunGrantedAs builds the re-run once, takes its digest from
// exactly what it is about to start, refuses when that is not approved, and
// starts that same description: the mounts as they were hashed, and the image
// by the id that was hashed, never by its tag. So nothing is resolved again
// between the last check and the run, which GrantedRunHash followed by
// RunGranted cannot promise.
type PinnedEscalator interface {
	RunGrantedAs(ctx context.Context, cmd Command, pol Policy, refused Result, approved string) (Result, error)
}

// errApprovalStale is why a pinned re-run did not start.
var errApprovalStale = errors.New("what would run is no longer what was approved (the command, a mount, the network or the image changed after the approval); nothing was run. Ask again to approve the run as it is now")

// escalationNow is the clock a ticket is issued and checked on; tests move it.
var escalationNow = time.Now

// shellReporter is a backend that runs Command.Shell under an interpreter other
// than the host's own (a Linux container on a Windows host runs sh).
type shellReporter interface{ CommandShell() string }

// commandShellOf is the interpreter b runs a Command.Shell line under: what it
// reports, else the host's native shell.
func commandShellOf(b Backend) string {
	if r, ok := b.(shellReporter); ok {
		return r.CommandShell()
	}
	return shellName(runtime.GOOS)
}

// EscalationTTL is how long an approval stays valid after it is given.
const EscalationTTL = 2 * time.Minute

// EscalationTicket binds one approval to one exact run.
type EscalationTicket struct {
	Hash    string    // EscalationHash of what was approved
	Expires time.Time // after this, the approval is void
}

// NewEscalationTicket issues a ticket for hash, valid for EscalationTTL from now.
func NewEscalationTicket(hash string, now time.Time) EscalationTicket {
	return EscalationTicket{Hash: hash, Expires: now.Add(EscalationTTL)}
}

// Valid reports whether the ticket covers hash at now: the same hash, and not
// expired. A ticket never matches an empty hash.
func (t EscalationTicket) Valid(hash string, now time.Time) bool {
	return hash != "" && t.Hash == hash && now.Before(t.Expires)
}

// EscalationHash is the digest an approval is bound to: the interpreter, every
// argv element, every mount (source, destination, mode), the network value and
// the image digest. Any change to any of them is a different hash. Elements are
// length-prefixed so no two inputs collide by concatenation.
func EscalationHash(interpreter string, argv []string, mounts []string, network, imageDigest string) string {
	h := sha256.New()
	write := func(tag, s string) {
		fmt.Fprintf(h, "%s:%d:", tag, len(s))
		h.Write([]byte(s))
	}
	write("interp", interpreter)
	for _, a := range argv {
		write("argv", a)
	}
	for _, m := range mounts {
		write("mount", m)
	}
	write("net", network)
	write("image", imageDigest)
	return hex.EncodeToString(h.Sum(nil))
}

// EscapeControl renders s for an approval prompt: every control character (a
// newline, a tab, an escape, a carriage return) is shown as its Go escape, so
// a command that hides a second line behind a terminal escape sequence or a
// carriage return is seen whole. Printable text, non-ASCII included, is kept.
func EscapeControl(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == 0x1b:
			b.WriteString(`\x1b`)
		case r == 0x7f || unicode.IsControl(r) || r == '\uFEFF' || r == '\u200b' || r == '\u200e' || r == '\u200f' || r == '\u202e':
			if r <= 0xff {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RiskSummary is the line Tenant itself writes into an escalation prompt: what
// the boundary refused and exactly what an approval opens, so the person reads
// Tenant's account of the risk and never the model's. b is the backend that
// refused; the summary says where the re-run would go.
func RiskSummary(b Backend, cmd Command, refused Result) string {
	kinds := map[string]Violation{}
	for _, v := range refused.Violations {
		if _, seen := kinds[v.Kind]; !seen {
			kinds[v.Kind] = v
		}
	}
	_, tier := b.(TierEscalator)
	var parts []string
	if v, ok := kinds["program"]; ok {
		parts = append(parts, "program: "+v.Detail+"; approving runs the command on the HOST, unconfined, under "+shellName(runtime.GOOS)+", exactly as shown")
	}
	if v, ok := kinds["filesystem"]; ok {
		switch {
		case tier && v.Path != "":
			parts = append(parts, "filesystem: "+v.Detail+"; approving grants this one command write access to "+grantScope(v.Path)+", still inside the sandbox")
		case tier:
			parts = append(parts, "filesystem: "+v.Detail+"; the refusal names no path the sandbox can grant, so there is nothing to approve")
		default:
			parts = append(parts, "filesystem: "+v.Detail+"; approving re-runs the command on the host with no filesystem boundary")
		}
	}
	if v, ok := kinds["network"]; ok {
		if tier {
			parts = append(parts, "network: "+v.Detail+"; approving gives this one command the network (full: the host's loopback, LAN and tailnet are reachable), still inside the sandbox")
		} else {
			parts = append(parts, "network: "+v.Detail+"; approving re-runs the command on the host with its full network")
		}
	}
	if len(parts) == 0 {
		return "nothing to approve: the refusal is a limit (timeout, memory, output cap), which re-running cannot change"
	}
	return strings.Join(parts, " | ")
}

// grantScope is the folder a filesystem grant would open for path: the path
// when it is a folder, else its folder.
func grantScope(path string) string {
	if strings.HasSuffix(path, string(filepath.Separator)) {
		return path
	}
	return filepath.Dir(path) + string(filepath.Separator) + "…"
}

// refusedPathIn finds the first absolute Linux path a container's error output
// names ("sh: 1: cannot create /mnt/c/x/y: Read-only file system",
// "cat: can't open '/mnt/c/x': No such file or directory"), trimmed of the
// quotes and punctuation shells wrap it in. "" when none.
var containerPathRe = regexp.MustCompile(`(^|[\s'"(\[:])(/[^\s'"():\[\]]+)`)

func refusedPathIn(output string) string {
	m := containerPathRe.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	p := strings.TrimRight(m[2], ".,;")
	if p == "/" || strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/proc/") {
		return ""
	}
	return p
}

// hostPathFor maps a container path back to the host path it mounts (the
// inverse of containerDest): /mnt/c/x -> C:\x on Windows, identity elsewhere.
// "" for a container-only path (/etc, /tmp) on Windows, which has no host
// counterpart.
func hostPathFor(ctr, goos string) string {
	if goos != "windows" {
		return ctr
	}
	rest, ok := strings.CutPrefix(ctr, "/mnt/")
	if !ok || rest == "" {
		return ""
	}
	drive := rest[0]
	if !isDriveLetter(drive) || (len(rest) > 1 && rest[1] != '/') {
		return ""
	}
	tail := strings.TrimPrefix(rest[1:], "/")
	return strings.ToUpper(string(drive)) + `:\` + strings.ReplaceAll(tail, "/", `\`)
}

// hostPathOf maps a container path the command named back to the host, or ""
// when it has no host counterpart.
func (s containerSpec) hostPathOf(ctr string) string { return hostPathFor(ctr, runtime.GOOS) }

// mounted reports whether host lies inside the work dir or any mount, i.e. the
// container could see it.
func (s containerSpec) mounted(host string) bool {
	if withinHost(host, s.work.host) {
		return true
	}
	for _, m := range s.mounts {
		if withinHost(host, m.host) {
			return true
		}
	}
	return false
}
