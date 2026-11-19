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
	// TEN-531: the same for the fleet child a machine runs: `tenant child
	// pause`, `resume` and `uninstall` are the operator's on that machine.
	{regexp.MustCompile(`(?i)^(\S*[\\/])?tenant(\.exe)?\s+(service|update|child)\b`), "tenant service/update/child (changes how Tenant itself runs)"},
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

// readOnlyWords are the commands that only look (TEN-491): run without a
// prompt when every segment of a command is one of them and nothing
// redirects. Case-insensitive, so PowerShell's verbs are listed once.
// ponytail: a flat set; `git` and `brew` get their read subcommands below,
// `find` loses its acting flags. Add a word when a model keeps asking for it.
var readOnlyWords = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "file": true, "stat": true, "find": true,
	"rg": true, "grep": true, "egrep": true, "fgrep": true, "which": true, "pwd": true, "echo": true, "date": true,
	"env": true, "printenv": true, "uname": true, "whoami": true, "id": true, "hostname": true, "df": true, "du": true,
	"ps": true, "tree": true, "realpath": true, "basename": true, "dirname": true, "type": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "true": true, "git": true, "brew": true,
	"dir": true, "gci": true, "get-childitem": true, "get-content": true, "get-location": true, "get-process": true,
	"get-item": true, "get-date": true, "select-string": true, "write-output": true,
}

var gitReadVerbs = map[string]bool{"status": true, "log": true, "diff": true, "show": true, "rev-parse": true,
	"ls-files": true, "blame": true, "describe": true, "branch": true, "remote": true}

// ReadOnly reports whether a command only looks at the machine: every segment
// of it (split at && || ; | and subshells) starts with a read-only word, no
// segment redirects its output, and nothing escalates (sudo). `git` and `brew`
// count only with a read subcommand; `find` not when it acts (-delete, -exec).
// The brokers skip the prompt for such a command (TEN-491, D4); iMessage does
// not use it. It is a convenience judgement, not the safety boundary.
func ReadOnly(command string) bool {
	c := strings.TrimSpace(command)
	if c == "" || strings.ContainsAny(c, ">") {
		return false
	}
	segments := segmentSplit.Split(c, -1)
	judged := 0
	for _, seg := range segments {
		f := strings.Fields(strings.TrimRight(seg, ")"))
		for len(f) > 0 && strings.Contains(f[0], "=") && !strings.HasPrefix(f[0], "=") { // VAR=x prefixes
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		name := strings.ToLower(f[0][strings.LastIndex(f[0], "/")+1:])
		if !readOnlyWords[name] {
			return false
		}
		args := f[1:]
		switch name {
		case "git":
			if len(args) == 0 || !gitReadVerbs[args[0]] || (args[0] == "branch" && hasActingFlag(args[1:], "-d", "-D", "-m", "-M", "--delete", "--move", "-c", "-C", "--copy", "-u", "--set-upstream-to", "--unset-upstream")) ||
				(args[0] == "remote" && len(args) > 1 && args[1] != "-v" && args[1] != "show" && args[1] != "get-url") {
				return false
			}
		case "brew":
			if len(args) == 0 || (args[0] != "list" && args[0] != "info" && args[0] != "--version" && args[0] != "deps" && args[0] != "outdated") {
				return false
			}
		case "find":
			if hasActingFlag(args, "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls") {
				return false
			}
		}
		judged++
	}
	return judged > 0
}

func hasActingFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

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
