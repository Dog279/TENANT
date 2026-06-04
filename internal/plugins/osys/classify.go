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
var dangerPatterns = []dangerPattern{
	{regexp.MustCompile(`(?i)\brm\s+(-\S*\s+)*-\S*[rf]`), "recursive/forced delete (rm -r/-f)"},
	{regexp.MustCompile(`(?i)\bremove-item\b.*-(recurse|force)`), "PowerShell Remove-Item -Recurse/-Force"},
	{regexp.MustCompile(`(?i)\b(rd|rmdir)\b.*\s/s`), "recursive directory removal (rd /s)"},
	{regexp.MustCompile(`(?i)\bdel\b.*\s/[sq]`), "recursive/quiet delete (del /s|/q)"},
	{regexp.MustCompile(`(?i)\bmkfs\b`), "filesystem creation (mkfs)"},
	{regexp.MustCompile(`(?i)\bdd\b.*\bof=`), "raw disk write (dd of=)"},
	{regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|hd|disk)`), "writing to a raw disk device"},
	{regexp.MustCompile(`(?i)\bformat(-volume)?\b(\s+[a-z]:|\s+/|\b.*-)`), "disk format"},
	{regexp.MustCompile(`(?i)\bdiskpart\b`), "diskpart (disk partitioning)"},
	{regexp.MustCompile(`(?i)\bcipher\b.*\s/w`), "secure-wipe free space (cipher /w)"},
	{regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff)\b`), "power state change (shutdown/reboot)"},
	{regexp.MustCompile(`(?i)\b(stop|restart)-computer\b`), "PowerShell Stop/Restart-Computer"},
	{regexp.MustCompile(`(?i)\binit\s+[06]\b`), "runlevel change (init 0/6)"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{`), "fork bomb"},
	{regexp.MustCompile(`(?i)\b(curl|wget|iwr|invoke-webrequest)\b.*\|\s*(sh|bash|zsh|pwsh|powershell|iex)\b`), "pipe remote content to a shell"},
	{regexp.MustCompile(`(?i)\biex\b.*\b(downloadstring|invoke-webrequest|iwr)\b`), "execute downloaded content (IEX)"},
	{regexp.MustCompile(`(?i)\bchmod\s+-R\s+0*[0-7]{0,3}\s+/`), "recursive chmod on a root path"},
	{regexp.MustCompile(`(?i)\bfind\s+/\b.*-delete`), "find / -delete"},
}

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
	return false, ""
}
