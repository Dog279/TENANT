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
	"slices"
	"strings"
	"unicode"
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

// readOnlyWords are the commands that only look (TEN-491): a line runs without
// a prompt when every command in it is one of them. The value is the word's
// argument rule (TEN-534): nil for a word that only looks whatever it is
// given, otherwise a function that reports whether these arguments only look.
// Case-insensitive, so PowerShell's verbs are listed once.
// ponytail: one table. Add a word when a model keeps asking for it, with a
// rule if any option or operand of it, in any implementation (GNU, BSD,
// PowerShell's alias), writes a file, changes a setting or runs a program.
var readOnlyWords = map[string]func(args []string) bool{
	"ls": nil, "cat": nil, "head": nil, "tail": nil, "wc": nil, "stat": nil, "grep": nil, "egrep": nil, "fgrep": nil,
	"which": nil, "pwd": nil, "echo": nil, "printenv": nil, "uname": nil, "whoami": nil, "id": nil, "df": nil,
	"du": nil, "ps": nil, "realpath": nil, "basename": nil, "dirname": nil, "type": nil, "cut": nil, "tr": nil,
	"true": nil,
	"dir":  nil, "gci": nil, "get-childitem": nil, "get-content": nil, "get-location": nil, "get-process": nil,
	"get-item": nil, "get-date": nil, "select-string": nil, "write-output": nil,

	// env runs the command it is given; VAR=value alone only prints.
	"env": only(envAssignment.MatchString),
	// date -s, --set and MMDDhhmm set the clock; only +FORMAT is the same everywhere.
	"date": only(func(a string) bool { return strings.HasPrefix(a, "+") }),
	// hostname NAME, -F and -b set the name.
	"hostname": only(func(string) bool { return false }),
	// find acts with -delete, -exec, -fprint and their kin: only the tests and
	// printers of findLooks, and the numbers they take (-mtime -7).
	"find": only(func(a string) bool {
		return !strings.HasPrefix(a, "-") || findLooks[a] || (len(a) > 1 && a[1] >= '0' && a[1] <= '9')
	}),
	"sort": without("o", "output", "compress-program"), // write FILE; run PROG
	"tree": without("oR", "output"),                    // write FILE; -R writes 00Tree.html files
	"file": without("C", "compile"),                    // write a .mgc file
	"rg":   without("", "pre", "hostname-bin"),         // run a program
	"uniq": uniqLooks,
	"git":  gitLooks,
	"brew": func(args []string) bool { return len(args) > 0 && brewLooks[args[0]] },
}

var (
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	gitRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

	brewLooks = map[string]bool{"list": true, "info": true, "--version": true, "deps": true, "outdated": true}

	findLooks = map[string]bool{
		"-name": true, "-iname": true, "-path": true, "-ipath": true, "-wholename": true, "-iwholename": true,
		"-regex": true, "-iregex": true, "-regextype": true, "-lname": true, "-ilname": true,
		"-type": true, "-xtype": true, "-size": true, "-empty": true, "-links": true, "-inum": true, "-samefile": true,
		"-perm": true, "-user": true, "-group": true, "-uid": true, "-gid": true, "-nouser": true, "-nogroup": true,
		"-readable": true, "-writable": true, "-executable": true, "-fstype": true,
		"-newer": true, "-anewer": true, "-cnewer": true, "-newermt": true, "-mtime": true, "-atime": true,
		"-ctime": true, "-mmin": true, "-amin": true, "-cmin": true, "-daystart": true, "-used": true,
		"-maxdepth": true, "-mindepth": true, "-depth": true, "-mount": true, "-xdev": true, "-prune": true,
		"-follow": true, "-noleaf": true, "-print": true, "-print0": true, "-printf": true, "-ls": true, "-quit": true,
		"-not": true, "-a": true, "-and": true, "-o": true, "-or": true, "-true": true, "-false": true,
		"-H": true, "-L": true, "-P": true, "-E": true,
	}

	// gitBranchOpts are the options of `git branch` that only list, an option
	// with a value spelled up to its "=". true marks those that make a name a
	// pattern to list; without one, a name is a branch to create. None of the
	// others takes the next argument as its value: `--sort --list NAME` would
	// sort by "--list" and create NAME, so --sort and --format are here only
	// as --sort= and --format=.
	gitBranchOpts = map[string]bool{
		"--list": true, "--contains": true, "--no-contains": true, "--merged": true, "--no-merged": true, "--points-at": true,
		"-a": false, "--all": false, "-r": false, "--remotes": false, "-v": false, "-vv": false, "--verbose": false,
		"-l": false, "--show-current": false, "--sort=": false, "--format=": false, "--color": false, "--color=": false,
		"--no-color": false, "--column": false, "--column=": false, "--no-column": false, "--abbrev": false,
		"--abbrev=": false, "--no-abbrev": false, "-i": false, "--ignore-case": false,
	}
)

// only is the rule of a word whose every argument must pass ok.
func only(ok func(arg string) bool) func([]string) bool {
	return func(args []string) bool {
		return !slices.ContainsFunc(args, func(a string) bool { return !ok(a) })
	}
}

// without is the rule of a word that only looks unless it is given one of its
// acting options: a letter of short in a -xyz cluster, or one of long as
// --name, --name=value or a shorter spelling of it (GNU tools and git take an
// unambiguous abbreviation).
func without(short string, long ...string) func([]string) bool {
	return only(func(a string) bool {
		name, isLong := strings.CutPrefix(a, "--")
		if !isLong {
			return !strings.HasPrefix(a, "-") || !strings.ContainsAny(a, short)
		}
		name, _, _ = strings.Cut(name, "=")
		return name == "" || !slices.ContainsFunc(long, func(l string) bool { return strings.HasPrefix(l, name) })
	})
}

// uniqLooks is uniq's rule. `uniq IN OUT` writes OUT, so there is at most one
// operand, and it comes last: BSD uniq reads an option after IN as OUT.
func uniqLooks(args []string) bool {
	for i, a := range args {
		if a == "--" {
			return len(args)-i-1 <= 1
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			return i == len(args)-1
		}
	}
	return true
}

// gitLooks is git's rule: a subcommand that reads, without what makes it
// write. A global option before the subcommand (`git -c k=v status`) is
// refused with everything else.
func gitLooks(args []string) bool {
	if len(args) == 0 {
		return false
	}
	rest := args[1:]
	switch args[0] {
	case "status", "log", "diff", "show", "rev-parse", "ls-files", "blame", "describe":
		return without("", "output")(rest) // --output=FILE writes the diff to FILE
	case "branch":
		option := func(a string) string { // --sort=x is looked up as --sort=
			if i := strings.Index(a, "="); i >= 0 {
				return a[:i+1]
			}
			return a
		}
		lists := slices.ContainsFunc(rest, func(a string) bool { return gitBranchOpts[option(a)] })
		return only(func(a string) bool {
			if !strings.HasPrefix(a, "-") {
				return lists
			}
			_, known := gitBranchOpts[option(a)]
			return known
		})(rest)
	case "remote":
		if len(rest) == 0 || (len(rest) == 1 && (rest[0] == "-v" || rest[0] == "--verbose")) {
			return true
		}
		// show and get-url, of a remote in the config by its name: given a URL
		// or host:path, show connects to it.
		return (rest[0] == "show" || rest[0] == "get-url") && only(func(a string) bool {
			return a == "-n" || a == "--push" || a == "--all" || gitRemoteName.MatchString(a)
		})(rest[1:])
	}
	return false
}

// readOnlySeps is where one command of a line ends and the next begins: the
// operators, a newline, and the brackets of a substitution or a subshell.
var readOnlySeps = regexp.MustCompile(`&&|\|\||\$\(|[;&|\n()]`)

// ReadOnly reports whether a command line only looks at the machine. The
// brokers run such a line without a prompt (TEN-491, D4; iMessage does not use
// it), so with exec on ask this is the one check between the model and a
// command nobody approved (TEN-534). It reads the line as sh and as PowerShell
// at once, and says yes only when:
//
//   - every command in it is a read-only word standing bare: no path before
//     it, no VAR=value before it, no quotes in it;
//   - a word with a rule (readOnlyWords) is given only arguments whose value
//     is the text itself and that the rule passes;
//   - a bracket appears only as an argument of a word that has no rule, and
//     what it holds is read-only too (`echo $(whoami)`);
//   - nothing in it is read differently by the two shells, or is something no
//     read-only command needs (readOnlyText).
//
// Anything it cannot tell is not read-only: that costs one approval card, and
// the opposite mistake runs a command unasked. It judges what the line asks
// the shell to do, not what a program then does of its own accord: git runs
// the fsmonitor, external diff and textconv programs a repository's config
// names, and a read reaches wherever its path points (a network share).
func ReadOnly(command string) bool {
	// Nothing is trimmed: sh makes a space outside ASCII part of a word, and a
	// backslash before the last newline still continues the line.
	c := strings.ReplaceAll(command, "\r\n", "\n")
	if !readOnlyText(c) {
		return false
	}
	// One frame per open bracket. cmd: a command has begun in it. free: that
	// command takes any argument, so a bracket may follow. closed: a bracket
	// has closed inside it, so only blanks may follow. n: commands judged.
	type frame struct {
		cmd, free, closed bool
		n                 int
	}
	stack := []frame{{}}
	pos := 0
	for _, m := range append(readOnlySeps.FindAllStringIndex(c, -1), []int{len(c), len(c)}) {
		text, sep := c[pos:m[0]], c[m[0]:m[1]]
		pos = m[1]
		top := &stack[len(stack)-1]
		if strings.Trim(text, " \t") != "" {
			ok, free := readOnlyCommand(text)
			if !ok || top.closed {
				return false
			}
			top.cmd, top.free = true, free
			top.n++
		}
		// sh reads a separator after a backslash as text; PowerShell does not.
		if sep != "" && strings.HasSuffix(text, `\`) {
			return false
		}
		switch sep {
		case "(", "$(":
			// In the command position a bracket runs what it prints
			// (`$(echo rm x)`, `& (…)`); after a word with a rule it can print
			// the option the rule forbids; glued to a word it is a call
			// (`$f.Delete()`, `f()`).
			if !top.cmd || !top.free || (sep == "(" && !strings.HasSuffix(text, " ") && !strings.HasSuffix(text, "\t")) {
				return false
			}
			stack = append(stack, frame{})
		case ")":
			if len(stack) == 1 || top.n == 0 { // unbalanced; or empty, and `f ()` begins a function in sh
				return false
			}
			stack = stack[:len(stack)-1]
			stack[len(stack)-1].closed = true
		default: // an operator, a newline or the end: the next command begins
			*top = frame{n: top.n}
		}
	}
	return len(stack) == 1 && stack[0].n > 0
}

// readOnlyText refuses what no read-only command needs and the reading above
// cannot follow: redirection (>), script blocks and brace expansion ({ }),
// backticks, PowerShell's static call (::), control characters (PowerShell
// ends a statement at a lone carriage return, sh does not), and anything
// outside ASCII but letters, marks and digits (PowerShell reads typographic
// quotes, dashes and spaces as their ASCII twins).
func readOnlyText(c string) bool {
	if c == "" || strings.ContainsAny(c, ">{}`") || strings.Contains(c, "::") {
		return false
	}
	for _, r := range c {
		switch {
		case r == '\n' || r == '\t':
		case r < ' ' || r == 0x7f:
			return false
		case r > unicode.MaxASCII && !unicode.In(r, unicode.L, unicode.M, unicode.Nd):
			return false // an invalid byte arrives here as U+FFFD, a symbol
		}
	}
	return true
}

// readOnlyCommand judges one command of the line: its word must be a
// read-only word, and its arguments must pass the word's rule. free reports a
// word without a rule.
func readOnlyCommand(text string) (ok, free bool) {
	text = strings.TrimLeft(text, " \t")
	word, rest := text, ""
	if i := strings.IndexAny(text, " \t"); i >= 0 {
		word, rest = text[:i], text[i:]
	}
	rule, known := readOnlyWords[strings.ToLower(word)]
	args, fixed, agreed := readOnlyArgs(rest)
	// ToLower turns two letters outside ASCII (İ, the Kelvin sign) into i and k.
	if !known || !agreed || strings.IndexFunc(word, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		return false, false
	}
	if rule == nil {
		return true, true
	}
	return fixed && rule(args), false
}

// readOnlyArgs splits a command's arguments and returns their values, quotes
// removed. agreed is false where sh and PowerShell would not pair the quotes
// alike: an open quote, or a backslash before a quote. fixed is false when
// the arguments a program receives are not, one for one, the values returned,
// so no rule can vet them: a variable, a glob, a comment, a redirection, or
// one of the places where the two shells split or spell an argument
// differently (each seen on dash and on PowerShell 5.1).
func readOnlyArgs(s string) (args []string, fixed, agreed bool) {
	var b strings.Builder
	fixed = true
	quote, in := byte(0), false // the open quote; inside an argument
	end := func() {
		if in {
			// PowerShell hands what follows --% to the program unparsed, %VAR% expanded.
			fixed = fixed && b.String() != "--%"
			args = append(args, b.String())
		}
		b.Reset()
		in = false
	}
	for i := 0; i < len(s); i++ {
		ch, next := s[i], byte(0)
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case quote != 0 && ch == quote:
			// PowerShell ends the argument at the quote that began it ('x'-o is
			// x and -o); sh goes on. Alike only when the argument ends here.
			fixed = fixed && (next == 0 || next == ' ' || next == '\t')
			quote = 0
		case quote == '\'':
			b.WriteByte(ch)
		case quote == '"':
			if ch == '\\' && next == '"' { // sh: a quote character; PowerShell: the closing quote
				return nil, false, false
			}
			fixed = fixed && ch != '$'
			b.WriteByte(ch)
		case ch == '\'' || ch == '"':
			quote, in = ch, true
		case ch == ' ' || ch == '\t':
			end()
		case ch == '\\':
			if next == '\'' || next == '"' { // sh: a quote character; PowerShell: an opening quote
				return nil, false, false
			}
			// sh drops the backslash and PowerShell keeps it. That changes what
			// the argument is when it leads (\-o) or ends it (a\ b).
			fixed = fixed && b.Len() > 0 && next != 0 && next != ' ' && next != '\t'
			in = true
		default:
			switch ch {
			case '$', '<': // a variable; a redirection
				fixed = false
			case ',':
				// Inside an argument both shells keep a comma. Beside a blank,
				// PowerShell makes `a ,b` the arguments a and b, and sh passes
				// the comma itself (`uniq in ,` writes a file named ",").
				fixed = fixed && in && next != 0 && next != ' ' && next != '\t' && next != ','
			case '*', '?', '[': // sh turns a glob into file names: an option, or two operands
				fixed = false
			case '#', '@': // leading: a comment; PowerShell's splat
				fixed = fixed && in
			case '.': // PowerShell splits -c.a into -c and .a
				fixed = fixed && (b.Len() == 0 || b.String()[0] != '-' || strings.HasPrefix(b.String(), "--"))
			}
			b.WriteByte(ch)
			in = true
		}
	}
	if quote != 0 {
		return nil, false, false
	}
	end()
	return args, fixed, true
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
