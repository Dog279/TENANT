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
// A word is taken as written here, in lower case: sh runs HEAD, a different
// program from head. Only the names PowerShell alone has (its cmdlets, gci)
// are taken in any case, as PowerShell takes them.
// ponytail: one table for every system. Add a word when a model keeps asking
// for it, with a rule if any option or operand of it, in any implementation
// (GNU, BSD, Git for Windows, the PowerShell cmdlet behind an alias), writes
// anything, changes a setting, starts another program or opens a connection.
var readOnlyWords = map[string]func(args []string) bool{
	"ls": nil, "cat": nil, "head": nil, "tail": nil, "wc": nil, "stat": nil, "grep": nil, "egrep": nil, "fgrep": nil,
	"which": nil, "pwd": nil, "echo": nil, "printenv": nil, "uname": nil, "whoami": nil, "id": nil, "df": nil,
	"du": nil, "ps": nil, "realpath": nil, "basename": nil, "dirname": nil, "type": nil, "cut": nil, "tr": nil,
	"true": nil,
	"dir":  nil, "gci": nil, "get-childitem": nil, "get-content": nil, "get-location": nil, "get-process": nil,
	"get-item": nil, "get-date": nil, "select-string": nil, "write-output": nil,

	// env runs the command it is given; VAR=value alone only prints.
	"env":  only(envAssignment.MatchString),
	"date": dateLooks,
	// hostname NAME, -F FILE and -b set the name. The options of hostnameShows
	// print, or are an error, in every hostname of Linux, macOS and Windows.
	"hostname": only(func(a string) bool { return hostnameShows[a] }),
	// find acts with -delete, -exec, -fprint and their kin: only the tests and
	// printers of findLooks, and the numbers they take (-mtime -7).
	"find": only(func(a string) bool {
		return !strings.HasPrefix(a, "-") || findLooks[a] || (len(a) > 1 && a[1] >= '0' && a[1] <= '9')
	}),
	"sort": sortLooks,
	// tree -o FILE writes FILE; -R writes a 00Tree.html into every directory.
	"tree": without("oR", "output"),
	// file -C writes a .mgc file, -p writes the file's times back, -z and -Z
	// start the decompression programs.
	"file": without("CpzZ", "compile", "preserve-date", "uncompress", "uncompress-noreport"),
	// rg --pre and --hostname-bin run a program of the caller's choosing, -z
	// the decompression programs.
	"rg":   without("z", "pre", "hostname-bin", "search-zip"),
	"uniq": uniqLooks,
	"git":  gitLooks,
	// brew info --github opens a browser; outdated --fetch-HEAD fetches.
	"brew": func(args []string) bool {
		return len(args) > 0 && brewLooks[args[0]] && without("", "github", "fetch-HEAD")(args[1:])
	},
}

var (
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	gitRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	uniqCount     = regexp.MustCompile(`^[0-9]+$`)

	brewLooks = map[string]bool{"list": true, "info": true, "--version": true, "deps": true, "outdated": true}

	// dateShows are the options of date that choose what it prints, in GNU,
	// BSD and busybox date alike (or are an error there): UTC, RFC 5322 and
	// ISO 8601 output. -I and --iso-8601 also come with a precision attached.
	dateShows = map[string]bool{"-u": true, "--utc": true, "--universal": true, "-R": true, "--rfc-email": true,
		"--rfc-2822": true, "--iso-8601": true, "-I": true, "-Idate": true, "-Ihours": true, "-Iminutes": true,
		"-Iseconds": true, "-Ins": true}

	hostnameShows = map[string]bool{"-f": true, "-s": true, "-i": true, "-I": true, "-A": true, "-d": true,
		"--fqdn": true, "--long": true, "--short": true, "--ip-address": true, "--all-ip-addresses": true,
		"--all-fqdns": true, "--domain": true}

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

// dateLooks is date's rule. An operand is a time to set, and so are -s and
// --set; -d sets the kernel's daylight-saving flag on some BSDs. What only
// prints, in GNU, BSD and busybox date alike: +FORMAT, the options of
// dateShows, and the time to print instead of now, as -r FILE (or seconds) or
// --reference=FILE.
func dateLooks(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "+"), dateShows[a], strings.HasPrefix(a, "--iso-8601="),
			strings.HasPrefix(a, "--rfc-3339="), strings.HasPrefix(a, "--reference="):
		case a == "-r" && i+1 < len(args):
			i++ // its value, whatever it looks like
		default:
			return false
		}
	}
	return true
}

// sortLooks is sort's rule: -o FILE and --output write FILE, -T DIR and
// --temporary-directory choose where its temporary files go (and stay, when
// it is stopped), --compress-program runs PROG. -Property is Sort-Object's
// parameter, which PowerShell's sort alias takes; a sort program stops at it,
// because no sort has an option P or p.
func sortLooks(args []string) bool {
	return without("oT", "output", "compress-program", "temporary-directory")(
		slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.EqualFold(a, "-Property") }))
}

// uniqLooks is uniq's rule. `uniq IN OUT` writes OUT, so there is at most one
// operand, and it comes last: BSD uniq reads an option after IN as OUT. The
// operand holds no glob, even quoted: Git for Windows' uniq expands one
// itself, into IN and OUT. -f and -s (POSIX) take the number after them.
func uniqLooks(args []string) bool {
	one := func(operands []string) bool {
		return len(operands) == 0 || (len(operands) == 1 && !strings.ContainsAny(operands[0], "*?["))
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "-f" || a == "-s") && i+1 < len(args) && uniqCount.MatchString(args[i+1]):
			i++
		case a == "--":
			return one(args[i+1:])
		case a == "-" || !strings.HasPrefix(a, "-"):
			return one(args[i:])
		}
	}
	return true
}

// gitLooks is git's rule: a subcommand that reads, without what makes it
// write, start another program or connect. A global option before the
// subcommand (`git -c k=v status`) is refused with everything else.
func gitLooks(args []string) bool {
	if len(args) == 0 {
		return false
	}
	rest := args[1:]
	switch args[0] {
	case "status", "log", "diff", "show", "rev-parse", "ls-files", "blame", "describe":
		// --output=FILE writes the diff to FILE; --help starts man, or a browser
		// on Windows; --show-signature and a %G placeholder of --format or
		// --pretty start gpg.
		return without("", "output", "help", "show-signature")(rest) &&
			!slices.ContainsFunc(rest, func(a string) bool { return strings.Contains(a, "%G") })
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
		// get-url, and show -n, of a remote in the config by its name. Without
		// -n, show connects to the remote (ssh, a credential helper), and given
		// a URL or host:path it connects to that.
		names := only(func(a string) bool {
			return a == "-n" || a == "--push" || a == "--all" || gitRemoteName.MatchString(a)
		})(rest[1:])
		return names && (rest[0] == "get-url" || (rest[0] == "show" && slices.Contains(rest[1:], "-n")))
	}
	return false
}

// readOnlySeps is where one command of a line ends and the next begins: the
// operators, a newline, and the brackets of a substitution or a subshell.
var readOnlySeps = regexp.MustCompile(`&&|\|\||\$\(|[;&|\n()]`)

// ReadOnly reports whether a command line only looks at the machine. The
// brokers run such a line without a prompt (TEN-491, D4; iMessage does not use
// it), so with exec on ask this is the one check between the model and a
// command nobody approved (TEN-534). It reads the line as sh and as Windows
// PowerShell 5.1 at once, the two shells os_exec uses, gives the same answer
// on every system, and says yes only when:
//
//   - every command in it is a read-only word standing bare: no path before
//     it, no VAR=value before it, no quotes in it;
//   - a word with a rule (readOnlyWords) is given only arguments whose value
//     is the text itself, and the rule passes on them as each shell hands
//     them to the program;
//   - no argument is a network path (readOnlyReach), and what one command
//     prints is no other command's argument or path: a bracket is an argument
//     of echo and Write-Output only (`echo $(whoami)`), no cmdlet sets a
//     variable, and the cmdlets that read a piped string as a path take no
//     pipe;
//   - nothing in it is read differently by the two shells, or is something no
//     read-only command needs (readOnlyText).
//
// Anything it cannot tell is not read-only: that costs one approval card, and
// the opposite mistake runs a command unasked.
//
// What still runs unasked, because the line does not show it:
//
//   - What a program does of its own accord. git runs the programs that a
//     repository's config, attributes and hooks name (fsmonitor, an external
//     diff, textconv, a signature check, post-index-change), in whatever
//     directory the model gives as workdir; it rewrites .git/index, and in a
//     partial clone it fetches what is missing from the configured remote.
//     brew reads the network and writes its cache. sort leaves temporary
//     files in the system's temporary directory when it is stopped. hostname
//     -f, id and ls -l ask the name services the machine is set up with.
//   - Where a local path leads: a device, a pipe, a mounted or mapped network
//     folder, a path an environment variable already holds. A host with no
//     share (//name) stays the pattern of grep, egrep, fgrep and rg when it is
//     their first operand.
//   - What a read tells the model: `env`, `cat ~/.ssh/id_ed25519`.
//   - Which program a word names. The shell finds it on PATH, so on Windows a
//     word that is no Windows command (true, head, grep, env, uniq, file …)
//     runs a file of that name someone put in a PATH folder, or Git for
//     Windows' Unix tool of that name, which expands a quoted glob itself
//     (`find . -name '-d*'` beside a file named -delete).
//   - A command that does not end (`tail -f`), until its timeout moves it to
//     the background.
func ReadOnly(command string) bool {
	// Nothing is trimmed: sh makes a space outside ASCII part of a word, and a
	// backslash before the last newline still continues the line.
	c := strings.ReplaceAll(command, "\r\n", "\n")
	if !readOnlyText(c) {
		return false
	}
	// One frame per open bracket. cmd: a command has begun in it. prints: that
	// command only prints its arguments, so a bracket may be one. closed: a
	// bracket has closed inside it, so only blanks may follow. piped: a pipe
	// feeds the command that comes next. n: commands judged.
	type frame struct {
		cmd, prints, closed, piped bool
		n                          int
	}
	stack := []frame{{}}
	pos := 0
	for _, m := range append(readOnlySeps.FindAllStringIndex(c, -1), []int{len(c), len(c)}) {
		text, sep := c[pos:m[0]], c[m[0]:m[1]]
		pos = m[1]
		top := &stack[len(stack)-1]
		if strings.Trim(text, " \t") != "" {
			ok, prints := readOnlyCommand(text, top.piped)
			if !ok || top.closed {
				return false
			}
			top.cmd, top.prints = true, prints
			top.n++
		}
		// sh reads a separator after a backslash as text; PowerShell does not.
		if sep != "" && strings.HasSuffix(text, `\`) {
			return false
		}
		switch sep {
		case "(", "$(":
			// In the command position a bracket runs what it prints
			// (`$(echo rm x)`, `& (…)`). As an argument, what it prints becomes
			// an option, or a path nobody wrote in the line (a network share),
			// of any word but the two that only print. Glued to a word it is a
			// call (`$f.Delete()`, `f()`).
			if !top.cmd || !top.prints || (sep == "(" && !strings.HasSuffix(text, " ") && !strings.HasSuffix(text, "\t")) {
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
			// A pipe feeds the next command, also across the end of the line.
			*top = frame{n: top.n, piped: sep == "|" || (sep == "\n" && top.piped && !top.cmd)}
		}
	}
	return len(stack) == 1 && stack[0].n > 0
}

// readOnlyText refuses what no read-only command needs and the reading above
// cannot follow: redirection (>), script blocks and brace expansion ({ }),
// backticks, PowerShell's static call (::), bash's network paths (a
// redirection from /dev/tcp/HOST/PORT opens a connection), control characters
// (PowerShell ends a statement at a lone carriage return, sh does not), and
// anything outside ASCII but letters, marks and digits (PowerShell reads
// typographic quotes, dashes and spaces as their ASCII twins).
func readOnlyText(c string) bool {
	if c == "" || strings.ContainsAny(c, ">{}`") || strings.Contains(c, "::") ||
		strings.Contains(c, "/dev/tcp/") || strings.Contains(c, "/dev/udp/") {
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

// psWords are the words PowerShell resolves to a cmdlet, by name or by alias.
// true marks those that take a string from a pipe as the path to read.
var psWords = map[string]bool{
	"get-childitem": true, "gci": true, "dir": true, "ls": true, "get-item": true,
	"get-content": false, "cat": false, "type": false, "get-location": false, "pwd": false, "get-process": false,
	"ps": false, "get-date": false, "select-string": false, "write-output": false, "echo": false, "sort": false,
}

// psVariables are the common parameters that make a cmdlet keep what it
// prints, or its errors, in a variable, each with the shortest start of its
// name that PowerShell takes for it.
var psVariables = map[string]string{"-outvariable": "-outv", "-errorvariable": "-errorv",
	"-warningvariable": "-warningv", "-informationvariable": "-informationv", "-pipelinevariable": "-pi"}

// psVariable reports one of psVariables, by its alias or by any start of its
// name that PowerShell takes for it, in any case, the value after a blank or
// a colon.
func psVariable(a string) bool {
	p, _, _ := strings.Cut(strings.ToLower(a), ":")
	for full, least := range psVariables {
		if strings.HasPrefix(p, least) && strings.HasPrefix(full, p) {
			return true
		}
	}
	return p == "-ov" || p == "-ev" || p == "-wv" || p == "-iv" || p == "-pv"
}

// readOnlyCommand judges one command of the line: its word must be a
// read-only word, and its arguments must pass the word's rule. piped says a
// pipe feeds the command. prints reports a word that only prints its
// arguments.
func readOnlyCommand(text string, piped bool) (ok, prints bool) {
	text = strings.TrimLeft(text, " \t")
	word, rest := text, ""
	if i := strings.IndexAny(text, " \t"); i >= 0 {
		word, rest = text[:i], text[i:]
	}
	// sh runs a word as it is written, so the table is matched as written. Only
	// the names PowerShell alone has are folded: PowerShell takes them in any
	// case, and no POSIX system has a program by such a name.
	name := word
	if lower := strings.ToLower(word); strings.Contains(lower, "-") || lower == "gci" {
		name = lower
	}
	rule, known := readOnlyWords[name]
	sh, ps, fixed, agreed := readOnlyArgs(rest)
	// ToLower turns two letters outside ASCII (İ, the Kelvin sign) into i and k.
	if !known || !agreed || strings.IndexFunc(word, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 ||
		readOnlyReach(name, sh) || readOnlyReach(name, ps) {
		return false, false
	}
	// What one command prints must not become another's path: PowerShell hands
	// a piped string to these cmdlets as the path to read, and a variable a
	// cmdlet sets is an argument further on. Neither is in the line to judge
	// (`git log --format=%x2f%x2fhost%x2fshare | dir` reads a network share).
	if takesPipe, isCmdlet := psWords[name]; (piped && takesPipe) ||
		(isCmdlet && (slices.ContainsFunc(sh, psVariable) || slices.ContainsFunc(ps, psVariable))) {
		return false, false
	}
	if rule == nil {
		return true, name == "echo" || name == "write-output"
	}
	// The rule must pass on what each shell hands the program.
	return fixed && rule(sh) && rule(ps), false
}

// uncHost finds a network path in an argument, where a path can begin: at its
// start, after an option's name (-Path:\\h, -O//h, --file=//h) and after a
// comma or "=". The path is two or more separators (or the \??\ of the NT
// namespace) and a host; the group is the separator after the host.
// C:\\Users\\me, http://h/x and src//pkg are not such paths.
var uncHost = regexp.MustCompile(`(?:^|[,=]|^-[A-Za-z-]*:?)(?:[\\/]{2,}|[\\/]\?\?[\\/])[^\\/\s]+([\\/])?`)

// readOnlyReach reports an argument that takes the command to another
// machine. A read of \\host\share (Windows also takes //host/share) opens a
// session with that host and offers it the user's credentials, and Get-Process
// -ComputerName asks another machine. The host alone (\\host, //host) is
// refused too, except as the first operand of the words that search for a
// pattern (//go:build), where only a host with a share is.
func readOnlyReach(word string, args []string) bool {
	pattern := word == "grep" || word == "egrep" || word == "fgrep" || word == "rg" // the next operand is the pattern
	for _, a := range args {
		operand := !strings.HasPrefix(a, "-")
		for _, m := range uncHost.FindAllStringSubmatchIndex(a, -1) {
			if m[2] >= 0 || !pattern || !operand {
				return true
			}
		}
		pattern = pattern && !operand
		if word == "ps" || word == "get-process" {
			// PowerShell takes any unambiguous start of a parameter's name, in
			// any case, with the value after a colon; -c is one for -ComputerName.
			p, _, _ := strings.Cut(strings.ToLower(a), ":")
			if p == "-cn" || (len(p) > 1 && strings.HasPrefix("-computername", p)) {
				return true
			}
		}
	}
	return false
}

// readOnlyArgs splits a command's arguments and returns them twice, quotes
// removed: as sh hands them to a program, and as PowerShell 5.1 does. The two
// differ where a rule can vet both: sh drops a backslash outside quotes and
// PowerShell keeps it; PowerShell drops a comma beside a blank (`a, b` is a
// and b) and an empty argument, and sh keeps them.
//
// agreed is false where the two would not pair the quotes alike: an open
// quote, or a backslash before a quote. fixed is false when what a program
// receives is not, one for one, these values, so no rule can vet it: a
// variable, a glob, a comment, a redirection, or a place where a shell, or
// the program's own reading of the command line PowerShell builds for it,
// splits or spells an argument another way. Each was seen on dash and on
// PowerShell 5.1 with git.exe and with Git for Windows' Unix tools.
func readOnlyArgs(s string) (sh, ps []string, fixed, agreed bool) {
	var a, p strings.Builder // one argument, for sh and for PowerShell
	fixed = true
	quote, in := byte(0), false // the open quote; inside an argument
	commas := 0                 // commas outside quotes that end p so far
	put := func(ch byte) {
		a.WriteByte(ch)
		p.WriteByte(ch)
		commas = 0
	}
	end := func() {
		if in {
			v := p.String()[:p.Len()-commas]
			// --% : PowerShell hands the rest to the program unparsed, %VAR%
			// expanded. @x : PowerShell's splat, and a file of arguments to
			// Git for Windows' Unix tools, quoted or not.
			fixed = fixed && v != "--%" && !strings.HasPrefix(v, "@")
			sh = append(sh, a.String())
			if v != "" {
				ps = append(ps, v)
			}
		}
		a.Reset()
		p.Reset()
		in, commas = false, 0
	}
	for i := 0; i < len(s); i++ {
		ch, next := s[i], byte(0)
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case quote != 0 && ch == quote:
			// PowerShell ends the argument at the quote that began it ('x'-o is
			// x and -o) and sh goes on: alike only when the argument ends here.
			// And PowerShell wraps an argument with a blank in double quotes
			// without escaping a backslash at its end, which then escapes the
			// closing quote for the program and swallows the next argument.
			fixed = fixed && (next == 0 || next == ' ' || next == '\t') && s[i-1] != '\\'
			quote = 0
		case quote == '"' && ch == '\\' && next == '"': // sh: a quote character; PowerShell: the closing quote
			return nil, nil, false, false
		case quote != 0:
			// PowerShell passes a double quote inside single quotes on
			// unescaped, and the program's parser drops it or splits there
			// (git log '--out"put=x' writes x); Git for Windows' Unix tools
			// read a single quote as a quote too. In double quotes a variable
			// is not text.
			fixed = fixed && ch != '\'' && ch != '"' && (ch != '$' || quote == '\'')
			put(ch)
		case ch == '\'' || ch == '"':
			quote, in = ch, true
		case ch == ' ' || ch == '\t':
			end()
		case ch == '\\':
			if next == '\'' || next == '"' { // sh: a quote character; PowerShell: an opening quote
				return nil, nil, false, false
			}
			// sh drops the backslash and PowerShell keeps it. That changes what
			// the argument is when it leads (\-o) or ends it (a\ b).
			fixed = fixed && a.Len() > 0 && next != 0 && next != ' ' && next != '\t'
			if next == '\\' { // sh: one backslash
				a.WriteByte('\\')
				p.WriteByte('\\')
				i++
			}
			p.WriteByte('\\')
			in, commas = true, 0
		case ch == ',':
			// Inside an argument both shells keep a comma. Beside a blank
			// PowerShell drops it, and two in a row it does not parse.
			fixed = fixed && next != ','
			a.WriteByte(ch)
			if p.Len() > 0 {
				p.WriteByte(ch)
				commas++
			}
			in = true
		default:
			switch ch {
			case '$', '<', '*', '?', '[':
				// A variable; a redirection; a glob, which sh turns into file
				// names: an option, or two operands.
				fixed = false
			case '#': // leading: a comment
				fixed = fixed && in
			case '.': // PowerShell splits -c.a into -c and .a
				fixed = fixed && (a.Len() == 0 || a.String()[0] != '-' || strings.HasPrefix(a.String(), "--"))
			}
			put(ch)
			in = true
		}
	}
	if quote != 0 {
		return nil, nil, false, false
	}
	end()
	return sh, ps, fixed, true
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
