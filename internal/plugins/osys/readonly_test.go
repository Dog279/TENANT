package osys

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// TEN-534: with exec on ask, ReadOnly is all that stands between the model and
// a command running with no approval card. Each command here changes the
// machine or runs a program, and ReadOnly called it read-only. The effect of
// each kind was seen on a real shell (dash under Linux, PowerShell 5.1 on
// Windows), with two exceptions: rg and tree were not installed, so theirs is
// what their manuals say; and date and hostname need root to succeed.
var readOnlyBypasses = []struct{ name, cmd string }{
	// env takes a command.
	{"env runs a command", "env sh deploy.sh"},
	{"env runs an interpreter", "env python3 x.py"},
	{"env runs a shell string", "env sh -c 'touch /tmp/x'"},
	{"env -i runs a command", "env -i sh -c 'rm x'"},
	{"env -S runs a command", `env -S 'sh -c "rm x"'`},
	{"env VAR=x runs a command", "env FOO=bar sh -c 'rm x'"},

	// PowerShell: a method call or an assignment on what a read-only command returned.
	{"method call after a parenthesis", `(Get-Item C:\x).Delete()`},
	{"method call after a parenthesis, later statement", `Get-ChildItem C:\ -Name; (Get-Item C:\x).Delete()`},
	{"method call after a parenthesis, as an argument", `echo (Get-Item C:\x).Delete()`},
	{"method call on a process", "(Get-Process notepad).Kill()"},
	{"property assignment after a parenthesis", `(Get-Item C:\x).IsReadOnly = $true`},
	{"method call on a variable", `Get-Item C:\x -OutVariable f; echo $f.Delete()`},
	{"method call by a quoted name", `Get-Item C:\x -OutVariable f; echo $f.'Delete'()`},
	{"method call through Invoke", `Get-Item C:\x -OutVariable f; echo $f.Delete.Invoke()`},
	// PowerShell: a script block runs per item.
	{"script block given to sort", `Get-ChildItem | sort { Remove-Item C:\x }`},
	{"script block bound to a parameter", `Get-ChildItem C:\x | Get-Item -Path { Remove-Item C:\x }`},
	// PowerShell: the call operator runs what a read-only command printed.
	{"call operator on a command's output", `& (Write-Output Remove-Item) C:\x`},
	{"call operator on a command's output, piped", "Get-Content x | & (Write-Output Invoke-Expression)"},
	// PowerShell ends a statement at a lone carriage return.
	{"carriage return hides a statement", "Get-ChildItem\rRemove-Item x"},
	// PowerShell: an assignment was skipped as a VAR=x prefix.
	{"PowerShell PATH assignment", `$env:PATH='C:\evil'; whoami`},
	{"PowerShell GIT_EXTERNAL_DIFF assignment", `$env:GIT_EXTERNAL_DIFF='calc.exe'; git diff`},

	// A substitution in the command position runs what a read-only command printed.
	{"substitution as the command", "$(echo rm -rf x)"},
	{"backticks as the command", "`echo rm -rf x`"},
	{"substitution as the command after a pipe", "ls | $(echo sh)"},
	{"substitution as the command with arguments", "echo x | $(echo xargs) rm"},
	// A substitution or a variable supplies the acting flag.
	{"substitution supplies find -delete", "find . $(echo -delete)"},
	{"backticks supply find -delete", "find . `echo -delete`"},
	{"substitution supplies sort -o", "sort $(echo -o) out in"},
	{"substitution supplies git branch -D", "git branch $(echo -D main)"},
	{"substitution supplies git log --output", "git log $(echo --output=x)"},
	{"variable supplies find -delete", "X=-delete; find . $X"},
	// Quotes and backslashes hide the acting flag from a word-by-word match.
	{"quoted find -delete", "find . '-delete'"},
	{"double-quoted find -delete", `find . "-delete"`},
	{"find -delete split by empty quotes", "find . -del''ete"},
	{"find -delete with a backslash", `find . -\delete`},
	{"quoted find -exec", `find . '-exec' rm {} \;`},
	{"quoted git branch -D", "git branch '-D' main"},

	// A VAR=value prefix or a bare assignment changes what the next word runs.
	{"LD_PRELOAD prefix", "LD_PRELOAD=./x.so ls"},
	{"PATH prefix", "PATH=/tmp/evil:$PATH ls"},
	{"GIT_EXTERNAL_DIFF prefix", "GIT_EXTERNAL_DIFF=./x git diff"},
	{"GIT_SSH_COMMAND prefix", "GIT_SSH_COMMAND=./x git remote show origin"},
	{"PATH assignment, then a read-only word", "PATH=/tmp/evil; ls"},
	// A path runs whatever file has a read-only name.
	{"./ls", "./ls"},
	{"./env", "./env"},
	{"a path to git", "/tmp/x/git status"},
	{"a Windows path with forward slashes", "C:/evil/git status"},
	{"a relative path", "evil/ls -la"},

	// Words that write a file or run a program with a flag or an operand.
	{"sort -o", "sort -o out in"},
	{"sort -o in a cluster", "sort -uo out in"},
	{"sort -oFILE", "sort -oout in"},
	{"sort --output", "sort --output=out in"},
	{"sort --output abbreviated", "sort --ou=out in"},
	{"sort --compress-program", "sort --compress-program=./x -S 1 big"},
	{"uniq IN OUT", "uniq in out"},
	{"uniq - OUT", "uniq - out"},
	{"uniq -- IN OUT", "uniq -- in -b"},
	{"uniq IN OUT where OUT is a comma", "uniq in ,"},
	{"uniq with a glob that matches two files", "uniq ~/.ssh/id_ed25519*"},
	// PowerShell makes two arguments of one: 'x'-o is x and -o, -c.a is -c and
	// .a (seen with a program that prints its arguments).
	{"PowerShell splits a quoted start from the option after it", "git log 'x'--output=y"},
	{"PowerShell splits -c.a, and uniq gets IN and OUT", "uniq -c.a -d.b"},
	{"tree -o", "tree -o out"},
	{"tree -o in a cluster", "tree -ao out"},
	{"tree -R", "tree -R -L 1 -H ."},
	{"find -fprint0", "find . -fprint0 /tmp/x"},
	{"rg --pre", "rg --pre rm pattern ."},
	{"rg --pre=", "rg --pre=./x pattern"},
	{"rg --hostname-bin", "rg --hostname-bin ./x --hyperlink-format '{host}' pattern"},
	{"file -C", "file -C -m magic"},
	{"file --compile", "file --compile -m magic"},
	{"git log --output", "git log --output=x"},
	{"git diff --output", "git diff --output=x"},
	{"git show --output", "git show --output=x HEAD"},
	{"git log --output behind an escaped separator", `git log --grep=a\;uniq --output=x`},
	{"git log --output behind a carriage return", "git log --grep=a\runiq --output=x"},
	{"git branch NAME", "git branch newname"},
	{"git branch -v NAME", "git branch -v newname"},
	{"git branch --no-color NAME", "git branch --no-color newname"},
	{"git branch NAME where --sort takes --list as its value", "git branch --sort --list newname"},
	{"git branch NAME where --format takes --list as its value", "git branch --format --list newname"},
	{"git branch NAME where NAME is a comma", "git branch ,"},
	{"git branch -f", "git branch -f main HEAD~3"},
	{"git branch --force", "git branch --force main HEAD~3"},
	{"git branch --edit-description", "git branch --edit-description"},
	{"git branch --track", "git branch --track x origin/x"},
	{"git remote -v add", "git remote -v add evil https://evil.example/x"},
	{"git remote -v set-url", "git remote -v set-url origin https://evil.example/x"},
	{"git remote -v remove", "git remote -v remove origin"},
	{"git remote show URL", "git remote show https://evil.example/x"},
	{"git remote show host:path", "git remote show evil.example:x"},

	// date and hostname set the clock and the name (as root).
	{"date -s", "date -s '2020-01-01'"},
	{"date --set", "date --set=2020-01-01"},
	{"date MMDDhhmm", "date 010100002020"},
	{"hostname NAME", "hostname evil"},
	{"hostname -F", "hostname -F /tmp/x"},
	{"hostname -b", "hostname -b evil"},
}

// Forms ReadOnly refuses though no harm was shown for them: their effect
// cannot be told from the text (a variable, a glob or an unknown flag where a
// flag decides), or the two shells read them differently. Refusing costs one
// approval card.
var readOnlyRefused = []struct{ name, cmd string }{
	// Refused before TEN-534 too, some only by accident of how the line was
	// split. Pinned so a rewrite cannot let them through.
	{"static call", `[System.IO.File]::Delete('C:\x')`},
	{"static call after a statement", `Get-ChildItem; [System.IO.File]::Delete('C:\x')`},
	{"static call as an argument", `Write-Output ([System.IO.File]::ReadAllText('C:\x'))`},
	{"sh function named for a read-only word", "ls () rm -rf x; ls"},
	{"text after a substitution", "sort $(pwd) -o out"},
	{"redirect", "ls > out.txt"},
	{"sudo", "sudo ls"},
	{"xargs", "cat x | xargs rm"},
	{"git global option", "git -c core.fsmonitor=./x status"},
	{"call operator on a string", "& 'calc.exe'"},
	{"dot-source", ". ./x.ps1"},
	{"module-qualified cmdlet", `Microsoft.PowerShell.Management\Get-Item C:\x`},
	{"bare assignment", "FOO=bar"},
	{"arithmetic", "echo $((1+2))"},
	{"text after a substitution, any word", "ls $(pwd)/src"},
	{"null byte", "ls\x00"},
	{"separator inside quotes", "echo 'a; b'"},
	{"backslash before a newline", "ls \\\n-la"},
	{"sort -o on a continued line", "sort in\\\n -o out"},
	{"git with no subcommand", "git"},
	{"git verb in capitals", "git STATUS"},
	{"git remote subcommand", "git remote prune origin"},
	{"brew with no subcommand", "brew"},
	{"brew install", "brew install jq"},
	{"nothing but separators", ";;"},
	{"blank", "   "},

	// Read-only before TEN-534, refused now.
	{"VAR=x before a read-only word", "FOO=1 env"},
	{"an absolute path to a read-only word", "/usr/bin/grep -r x ."},
	{"ENV in capitals, which macOS finds", "ENV sh deploy.sh"},
	{"uniq IN -OUT where options do not permute (BSD)", "uniq in -c"},
	{"subshell", "(ls)"},
	{"subshell after a pipe", "ls | (grep x)"},
	{"empty substitution", "echo $()"},
	{"substitution glued to a parenthesis", "echo $(pwd)(ls)"},
	{"array subexpression", "echo @(ls)"},
	{"process substitution", "cat <(ls)"},
	{"unbalanced parenthesis", "echo $(whoami"},
	{"stray parenthesis", "echo x)"},
	{"static call inside a pattern", "rg 'std::vector' src"},
	{"braces", "echo ${HOME}"},
	{"brace expansion", "ls *.{go,md}"},
	{"reflog syntax", "git show HEAD@{1}"},
	{"backticks as an argument", "echo `whoami`"},
	{"form feed", "ls\f-la"},
	{"no-break space", "ls\u00a0-la"},
	{"typographic quotes", "cat \u201cx y\u201d"},
	{"en dash", "ls \u2013la"},
	{"invalid UTF-8", "ls \xff"},
	{"a word that only lowercases to a read-only one", "g\u0130t status"},
	{"unterminated quote", "echo 'a"},
	{"unterminated double quote", `echo "a`},
	{"backslash before a quote", `echo \'a`},
	{"backslash before a closing double quote", `Get-ChildItem "C:\Program Files\"`},
	{"backslash before a separator", `ls C:\; pwd`},
	{"find with a variable", "find . $FLAGS"},
	{"find with a variable in double quotes", `find . "$FLAGS"`},
	{"find with a leading glob", "find *"},
	{"find with an unquoted pattern", "find . -name *.go"},
	{"find with a glob after a dash", "find . -na*"},
	{"find with a glob in a path", "find src/*.go -type f"},
	{"find with a one-character glob", "find . -name a?"},
	{"find with a bracket glob", "find . -name [ab]"},
	{"quotes in the middle of an argument", "sort a'b'c"},
	{"a dot in a short option", "sort -t. -k2 f"},
	{"a comma at the end", "sort -t,"},
	{"two commas", "sort a,,b"},
	{"a comma before a tab", "sort a,\tb"},
	{"a backslash at the end", `sort in\`},
	{"a backslash before a tab", "sort in\\\tx"},
	{"a backslash before a double quote", `echo \"a`},
	{"a backslash before a parenthesis", `echo \(ls)`},
	{"a read-only word after a substitution", "echo $(pwd) ls"},
	{"delete character", "ls\x7f"},
	{"find with an unknown primary", "find . -newerXY x"},
	{"find with a lone dash", "find . -"},
	{"find reading its standard input", "find . -name x < list"},
	{"sort with a leading backslash", `sort \-o out in`},
	{"sort with a trailing backslash", `sort in\ -o`},
	{"sort with a splat", "sort @args"},
	{"git branch NAME with --list in a comment", "git branch newname # --list"},
	{"PowerShell passes a comma list as separate arguments", "git diff a ,--output=x"},
	{"PowerShell stop-parsing token", "git log --% %X%"},
	{"env flag", "env -0"},
	{"env VAR from a substitution", "env FOO=$(whoami)"},
	{"env with an empty name", "env =x"},
	{"date flag", "date -u"},
	{"date -d", "date -d yesterday"},
	{"hostname flag", "hostname -f"},
	{"hostname with a comma, which sh passes as the name", "hostname ,"},
	{"hostname with a no-break space, which sh passes as the name", "hostname  "},
	{"a comma that ends an argument", "sort -t, -k1 f"},
	{"git branch --sort with its value apart", "git branch --sort refname"},
	{"uniq with a value-taking flag", "uniq -f 1 in"},
	{"git branch with an unknown flag", "git branch --frobnicate"},
	{"git branch --", "git branch -- x"},
	{"git remote show with a flag", "git remote show --frobnicate"},
	{"git remote show a path", "git remote show ../other"},
	{"brew with a substitution", "brew list $(echo x)"},
}

// The plain forms must go on running without a prompt.
var readOnlyPlain = []string{
	"env", "env FOO=bar", "env FOO=bar BAZ='a b'", "printenv", "printenv HOME",
	"date", "date +%s", "date '+%Y-%m-%d %H:%M'", "hostname",
	`Get-Item C:\x`, "Get-ChildItem", `Get-ChildItem C:\`, `Get-ChildItem 'C:\Program Files\'`,
	`Get-ChildItem C:\ | Select-String x`, `Get-Content C:\x\y.txt`, "Get-ChildItem -Recurse -Filter *.go",
	"Get-Process | Select-String tenant", "Get-Date", "Get-Location", "Write-Output hi", "dir", `gci C:\`,
	"Get-ChildItem | sort Name", `Get-Item (Get-Location)`, `Get-Item C:\x -OutVariable f; echo $f.Length`,
	"git status", "git status && git log -3", "git log --oneline -5", "git log --format='%h %s' -n 5",
	"git log --oneline | head -5", "git diff --stat HEAD~1", "git show HEAD:README.md", "git diff -- src/main.go",
	"git diff src\\main.go", "git rev-parse HEAD", "git ls-files", "git blame -L 1,5 x.go", "git describe --tags",
	"git log --output-indicator-new=+ -1", "git log --oneline --grep='fix bug'",
	"git branch", "git branch -a", "git branch -vv", "git branch --show-current", "git branch --list 'feat/*'",
	"git branch --contains HEAD", "git branch --merged main", "git branch --sort=-committerdate --format=%H",
	"git remote", "git remote -v", "git remote get-url origin", "git remote get-url --push origin",
	"git remote show origin", "git remote show -n origin",
	"ls -la", "ls -la | grep x", "ls ~/src", "ls *.go", "cat x | grep y | wc -l", "cat $HOME/.profile",
	"echo $(whoami)", "echo $(echo $(whoami))", "echo $(whoami) $(pwd)", "cat $(find . -name x)", "echo hi # a note",
	"echo \"it's\"", "echo 'say \"hi\"'", "grep -E \"a b\" f", "grep -rn TODO .", "wc -l < file", "ls &",
	"ls\npwd", "ls\r\npwd", "ls;", "\tls\t", "LS", "Git status", "ls\t-la", "Get-Item\t(Get-Location)",
	"find . -name '*.go'\t-type f", "git log --grep=fix#12 --author=a@b.c", "git log --format=%h.%s",
	"git remote --verbose", "git remote get-url --all origin",
	"brew list", "brew info jq", "brew --version", "brew deps jq", "brew outdated",
	"find . -name '*.go'", `find . -name "*.go" -newer go.mod`, "find . -maxdepth 2 -type d", "find . -mtime -7 -size -10k",
	"find src/main.go -type f", "find . -type f -name '*.go' -print0", "find . -iname 'read me' -not -path './x/*'",
	"rg -n foo src", "rg --files", "rg -p 'fn main' -t rust", "rg --pretty --pre-glob '*.pdf' x",
	"sort -u names.txt", "sort -k2,2n -t: f", "sort -rn", "cat a | sort | uniq -c | sort -rn | head -5",
	"sort src\\names.txt", "uniq -c", "uniq -c names.txt", "uniq -- -odd-name", "uniq -",
	"tree -L 2", "tree --noreport src", "file README.md", "file -b --mime-type x",
	"du -sh .", "df -h", "ps aux", "head -n 20 f", "tail -100 log", "stat x", "id", "uname -a", "whoami",
	"which go", "pwd", "type ls", "basename /a/b", "dirname /a/b", "realpath .", "cut -d: -f1 /etc/passwd",
	"tr a-z A-Z", "true", "egrep x f", "fgrep x f",
	"cat r\u00e9sum\u00e9.txt", "ls \u6587\u4ef6", "cat cafe\u0301.txt",
}

func TestReadOnlyBypasses(t *testing.T) {
	for _, tc := range readOnlyBypasses {
		t.Run(tc.name, func(t *testing.T) {
			if ReadOnly(tc.cmd) {
				t.Errorf("ReadOnly(%q) = true: it would run with no approval card", tc.cmd)
			}
		})
	}
}

func TestReadOnlyRefusedShapes(t *testing.T) {
	for _, tc := range readOnlyRefused {
		t.Run(tc.name, func(t *testing.T) {
			if ReadOnly(tc.cmd) {
				t.Errorf("ReadOnly(%q) = true, want false", tc.cmd)
			}
		})
	}
}

func TestReadOnlyPlainForms(t *testing.T) {
	for _, cmd := range readOnlyPlain {
		if !ReadOnly(cmd) {
			t.Errorf("ReadOnly(%q) = false: a plain read-only command must not ask", cmd)
		}
		if why := readOnlyViolation(cmd); why != "" {
			t.Errorf("the fuzz invariants reject the plain form %q: %s", cmd, why)
		}
	}
}

// What must hold for ANY command ReadOnly accepts. The checks read the text
// their own way (regexps and strings.Fields), not through ReadOnly's lexer, so
// a fault in the lexer does not hide from them.
var (
	roTestSeps     = regexp.MustCompile(`&&|\|\||\$\(|[;&|\n()]`)
	roGluedParen   = regexp.MustCompile(`(^|[^ \t$])\(`)
	roEmptyGroup   = regexp.MustCompile(`\([ \t\n]*\)`)
	roAfterGroup   = regexp.MustCompile(`\)[ \t]*([^ \t\n;|&()$]|\$[^(]|\$$)`)
	roEscapedSep   = regexp.MustCompile(`\\(\r?\n|[;&|()]|\$\()`)
	roBareWord     = regexp.MustCompile(`^[A-Za-z-]+$`)
	roAssignment   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	roStrictWords  = map[string]bool{"env": true, "date": true, "hostname": true, "find": true, "sort": true, "uniq": true, "tree": true, "file": true, "rg": true, "git": true, "brew": true}
	roGitVerbs     = map[string]bool{"status": true, "log": true, "diff": true, "show": true, "rev-parse": true, "ls-files": true, "blame": true, "describe": true, "branch": true, "remote": true}
	roFindActs     = map[string]bool{"-delete": true, "-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-fprint": true, "-fprint0": true, "-fprintf": true, "-fls": true}
	roBranchActs   = map[string]bool{"-d": true, "-D": true, "-m": true, "-M": true, "-c": true, "-C": true, "-f": true, "--force": true, "--delete": true, "--move": true, "--copy": true, "-u": true, "--set-upstream-to": true, "--unset-upstream": true, "--edit-description": true, "--track": true, "-t": true}
	roAppendedActs = []string{"; rm -rf x", " | sh", " && sudo ls", "\nrm x", " > out", "; x=1", " | xargs rm", "; (Get-Item x).Delete()", " `id`", "; ./ls", "; env sh", "; $(echo rm x)", "; [IO.File]::Delete('x')", " | sort { rm x }", "\rrm x"}
	roPrefixedActs = []string{"sudo ", "X=1 ", "exec ", "command ", "time ", "nohup ", "xargs ", "rm x; ", "$(echo sh) "}
)

// readOnlyViolation names the invariant a command ReadOnly accepted breaks,
// or "" when it holds them all.
func readOnlyViolation(c string) string {
	if !utf8.ValidString(c) {
		return "invalid UTF-8"
	}
	for _, bad := range []string{">", "{", "}", "`", "::"} {
		if strings.Contains(c, bad) {
			return "contains " + bad
		}
	}
	lf := strings.ReplaceAll(c, "\r\n", "\n")
	for _, r := range lf {
		if (r < ' ' && r != '\n' && r != '\t') || r == 0x7f {
			return "contains a control character"
		}
	}
	t := strings.TrimSpace(lf)
	switch {
	case roGluedParen.MatchString(t):
		return "a parenthesis glued to a word (a method call or a function)"
	case roEmptyGroup.MatchString(t):
		return "an empty group"
	case roAfterGroup.MatchString(t):
		return "text after a closing parenthesis"
	case roEscapedSep.MatchString(t):
		return "a backslash before a separator"
	case strings.Count(t, "(") != strings.Count(t, ")"):
		return "unbalanced parentheses"
	}
	seps := append(roTestSeps.FindAllStringIndex(t, -1), []int{len(t), len(t)})
	pos, words, closed := 0, 0, false // closed: the separator before this piece was a ")"
	for _, m := range seps {
		piece, sep := t[pos:m[0]], t[m[0]:m[1]]
		pos = m[1]
		wasClosed := closed
		closed = sep == ")"
		f := strings.Fields(piece)
		if len(f) == 0 {
			if (sep == "(" || sep == "$(") && !wasClosed {
				return "a group in the command position"
			}
			continue
		}
		words++
		word := strings.ToLower(f[0])
		if _, ok := readOnlyWords[word]; !ok || !roBareWord.MatchString(f[0]) {
			return "the command word " + f[0] + " is not a bare read-only word"
		}
		strict := roStrictWords[word]
		if strict && (sep == "(" || sep == "$(") {
			return word + " takes a substitution as an argument"
		}
		if !strict || strings.ContainsAny(piece, `'"\`) {
			continue // strings.Fields reads a quoted or escaped argument wrongly
		}
		args := f[1:]
		for _, a := range args {
			if strings.Contains(a, "$") {
				return word + " takes a variable as an argument"
			}
		}
		operands := 0
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				operands++
			}
		}
		switch word {
		case "env":
			for _, a := range args {
				if !roAssignment.MatchString(a) {
					return "env is given something that is not VAR=value: " + a
				}
			}
		case "hostname":
			if len(args) > 0 {
				return "hostname is given an argument"
			}
		case "date":
			for _, a := range args {
				if !strings.HasPrefix(a, "+") {
					return "date is given something that is not +FORMAT: " + a
				}
			}
		case "find":
			for _, a := range args {
				if roFindActs[a] {
					return "find acts: " + a
				}
			}
		case "sort", "tree":
			for _, a := range args {
				long, isLong := strings.CutPrefix(a, "--")
				long, _, _ = strings.Cut(long, "=")
				// --output, or what getopt takes for it: --o, --ou, --out…
				if (isLong && long != "" && strings.HasPrefix("output", long)) || (!isLong && strings.HasPrefix(a, "-") && strings.Contains(a, "o")) {
					return word + " writes a file: " + a
				}
			}
		case "uniq":
			if operands > 1 {
				return "uniq is given an output file"
			}
		case "file":
			for _, a := range args {
				if a == "--compile" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "C")) {
					return "file compiles a magic file: " + a
				}
			}
		case "rg":
			for _, a := range args {
				if a == "--pre" || strings.HasPrefix(a, "--pre=") || strings.HasPrefix(a, "--hostname-bin") {
					return "rg runs a program: " + a
				}
			}
		case "git":
			if len(args) == 0 || !roGitVerbs[args[0]] {
				return "git without a read subcommand"
			}
			for _, a := range args {
				if a == "--output" || strings.HasPrefix(a, "--output=") {
					return "git writes a file: " + a
				}
				if args[0] == "branch" && roBranchActs[a] {
					return "git branch acts: " + a
				}
			}
		}
	}
	if words == 0 {
		return "no command"
	}
	// Whatever is added to a read-only command, an acting command stays one.
	for _, s := range roAppendedActs {
		if ReadOnly(c + s) {
			return "still read-only with " + s + " appended"
		}
	}
	for _, p := range roPrefixedActs {
		if ReadOnly(p + c) {
			return "still read-only with " + p + " in front"
		}
	}
	// Two read-only commands in a row are read-only, each part of one is, and
	// neither outer blanks nor Windows line ends change the answer.
	if !ReadOnly(c + " ; " + c) {
		return "not read-only twice in a row"
	}
	if !ReadOnly(" \t" + c + "\n ") {
		return "outer blanks change the answer"
	}
	if !strings.Contains(c, "\r") && !ReadOnly(strings.ReplaceAll(c, "\n", "\r\n")) {
		return "Windows line ends change the answer"
	}
	if !strings.ContainsAny(t, "()") {
		for _, piece := range regexp.MustCompile(`&&|\|\||[;&|\n]`).Split(t, -1) {
			if strings.TrimSpace(piece) != "" && !ReadOnly(piece) {
				return "the part " + piece + " is not read-only by itself"
			}
		}
	}
	return ""
}

// FuzzReadOnly: ReadOnly never panics, and whatever it accepts holds every
// invariant above.
func FuzzReadOnly(f *testing.F) {
	for _, tc := range readOnlyBypasses {
		f.Add(tc.cmd)
	}
	for _, tc := range readOnlyRefused {
		f.Add(tc.cmd)
	}
	for _, cmd := range readOnlyPlain {
		f.Add(cmd)
	}
	f.Fuzz(func(t *testing.T, c string) {
		if !ReadOnly(c) {
			return
		}
		if why := readOnlyViolation(c); why != "" {
			t.Errorf("ReadOnly(%q) = true, but: %s", c, why)
		}
	})
}
