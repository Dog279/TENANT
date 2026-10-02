// Package osys is Tenant's operating-system plugin: inspect the machine
// and (gated) run shell commands, cross-platform (Windows / Linux /
// macOS). This is the highest-blast-radius plugin in Tenant — an agent
// with a shell can destroy a machine — so command execution is OFF by
// default and a danger classifier hard-blocks catastrophic commands.
//
// HONEST LIMIT: the classifier is a guardrail against accidents and the
// obvious destructive cases, NOT a security sandbox. A determined or
// obfuscated command (base64, unusual flag order, indirection) can slip
// past it. The real containment is: keep exec OFF unless you need it,
// run Tenant as a least-privilege OS user, and prefer a container/VM.
package osys

import (
	"regexp"
	"strings"
)

// dangerPattern pairs a matcher with why it's dangerous.
type dangerPattern struct {
	re     *regexp.Regexp
	reason string
}

// dangerPatterns flags commands that can irreversibly damage the
// machine. Case-insensitive; matched against the whole command string
// so chaining (`ls && rm -rf /`) doesn't hide the dangerous part.
// Command words (shutdown, init, format) only count at the start of a
// command, so `git log --grep=reboot` or `--format=%H` don't (TEN-489).
// Recursive deletes and permission changes are judged by their target in
// destructiveTarget below, not here.
var dangerPatterns = []dangerPattern{
	{regexp.MustCompile(`(?i)\bremove-item\b.*-(recurse|force)`), "PowerShell Remove-Item -Recurse/-Force"},
	{regexp.MustCompile(`(?i)\b(rd|rmdir)\b.*\s/s`), "recursive directory removal (rd /s)"},
	{regexp.MustCompile(`(?i)\bdel\b.*\s/s\b`), "recursive delete (del /s)"},
	{regexp.MustCompile(`(?i)\bmkfs\b`), "filesystem creation (mkfs)"},
	{regexp.MustCompile(`(?i)\bdd\b[^;&|\n]*\bof=/dev/`), "raw disk write (dd of=/dev/…)"},
	{regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|hd|disk|rdisk)`), "writing to a raw disk device"},
	{regexp.MustCompile(`(?i)\bformat-volume\b|` + cmdStart + `format\s+[a-z]:`), "disk format"},
	{regexp.MustCompile(`(?i)\bdiskutil\s+(erase\w*|partitionDisk|zeroDisk|randomDisk|secureErase|reformat)\b`), "disk erase (diskutil)"},
	{regexp.MustCompile(`(?i)\bdiskpart\b`), "diskpart (disk partitioning)"},
	{regexp.MustCompile(`(?i)\bcipher\b.*\s/w`), "secure-wipe free space (cipher /w)"},
	{regexp.MustCompile(`(?i)` + cmdStart + `(shutdown|reboot|halt|poweroff)\b`), "power state change (shutdown/reboot)"},
	{regexp.MustCompile(`(?i)\b(stop|restart)-computer\b`), "PowerShell Stop/Restart-Computer"},
	{regexp.MustCompile(`(?i)` + cmdStart + `init\s+[06]\b`), "runlevel change (init 0/6)"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{`), "fork bomb"},
	{regexp.MustCompile(`(?i)\b(curl|wget|iwr|invoke-webrequest)\b.*\|\s*(sudo\s+)?(sh|bash|zsh|pwsh|powershell|iex)\b`), "pipe remote content to a shell"},
	{regexp.MustCompile(`(?i)\b(sh|bash|zsh)\s+-c\s+["']?\$\(\s*(curl|wget)\b`), "run remote content in a shell"},
	{regexp.MustCompile(`(?i)\biex\b.*\b(downloadstring|invoke-webrequest|iwr)\b`), "execute downloaded content (IEX)"},
	{regexp.MustCompile(`(?i)\bchmod\s+-R\s+0*[0-7]{0,3}\s+/`), "recursive chmod on a root path"},
	{regexp.MustCompile(`(?i)\bfind\s+/(\s|$)[^;&|\n]*-delete`), "find / -delete"},
	// TEN-498: an agent must not stop, remove or replace the Tenant it runs
	// in without the operator's say. Anchored: `grep tenant service.log` is
	// not this; a path or .exe before the name is.
	{regexp.MustCompile(`(?i)^(\S*[\\/])?tenant(\.exe)?\s+(service|update)\b`), "tenant service/update (changes how Tenant itself runs)"},
}

// cmdStart anchors a command word at the start of a command: the string's
// start, after a separator, or after sudo.
const cmdStart = `(?:^|[;&|(\n]\s*|\bsudo\s+)`

// Classify reports whether a command looks destructive, with a reason.
// Empty command is treated as safe (the dispatcher rejects it earlier).
func Classify(command string) (dangerous bool, reason string) {
	c := strings.TrimSpace(command)
	if c == "" {
		return false, ""
	}
	for _, p := range dangerPatterns {
		if p.re.MatchString(c) {
			return true, p.reason
		}
	}
	return destructiveTarget(c)
}

// segmentSplit separates the commands of a compound line.
var segmentSplit = regexp.MustCompile("&&|\\|\\||[;&|\n`(]|\\$\\(")

// destructiveTarget flags a recursive rm, chmod or chown whose target is the
// machine itself: /, a top-level folder, a home folder or one of its direct
// children, or ., .., *. A recursive delete of anything deeper (node_modules,
// ./build, ~/code/app/dist) is an ordinary command: it still asks unless the
// operator allowed exec, but it isn't flagged destructive (TEN-489).
// ponytail: splits on whitespace, so a quoted path with spaces is judged
// piece by piece; parse shell quoting if that ever matters.
func destructiveTarget(c string) (bool, string) {
	for _, seg := range segmentSplit.Split(c, -1) {
		f := strings.Fields(seg)
		for len(f) > 0 && (f[0] == "sudo" || f[0] == "command" || f[0] == "exec" || strings.Contains(f[0], "=")) {
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		name := f[0][strings.LastIndex(f[0], "/")+1:]
		if name != "rm" && name != "chmod" && name != "chown" {
			continue
		}
		recursive := false
		var targets []string
		for _, a := range f[1:] {
			switch {
			case a == "--recursive":
				recursive = true
			case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
				recursive = recursive || strings.ContainsAny(a, "rR")
			default:
				targets = append(targets, a)
			}
		}
		if !recursive {
			continue
		}
		for _, t := range targets {
			if catastrophicPath(t) {
				if name == "rm" {
					return true, "recursive delete of " + t
				}
				return true, "recursive " + name + " of " + t
			}
		}
	}
	return false, ""
}

// catastrophicPath reports whether p is the machine itself rather than
// something in it: /, a top-level folder, a home folder or its direct
// child, ., .., or a bare *.
func catastrophicPath(p string) bool {
	p = strings.Trim(p, `"'`)
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/*"), "/")
	switch p {
	case "", "~", "$HOME", "${HOME}", ".", "..", "*":
		return true
	}
	for _, home := range []string{"~/", "$HOME/", "${HOME}/"} {
		if rest, ok := strings.CutPrefix(p, home); ok {
			return !strings.Contains(rest, "/")
		}
	}
	if rest, ok := strings.CutPrefix(p, "/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) == 1 {
			return true
		}
		return (parts[0] == "Users" || parts[0] == "home") && len(parts) <= 3
	}
	return false
}
