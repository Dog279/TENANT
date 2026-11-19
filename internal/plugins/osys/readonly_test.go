package osys

import (
	"regexp"
	"slices"
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

	// Round two (a red team's findings on the first fix, and their siblings).
	// git starts another program: man or a browser, gpg, ssh.
	{"git status --help", "git status --help"},
	{"git diff --help", "git diff --help"},
	{"git rev-parse --help", "git rev-parse --help victim.txt"},
	{"git log --show-signature", "git log --show-signature -1"},
	{"git show --show-signature", "git show --show-signature HEAD"},
	{"git log --format=%GG", "git log -1 --format=%GG"},
	{"git log --pretty with %G?", "git log --pretty='format:%h %G?' -3"},
	{"%G inside a bracket", "echo $(git show -s --format=%GK)"},
	{"git remote show NAME connects", "git remote show origin"},
	// Options that write.
	{"sort -T", "tail -c 5000000 -f /usr/bin/git | sort -T . -S 1k"},
	{"sort -TDIR", "sort -T/var/tmp/x -S 1k big"},
	{"sort --temporary-directory", "sort --temporary-directory=notes -S 1k big"},
	{"file -p", "file -p victim.txt"},
	{"file -p after the file", "file README.md -p"},
	{"file -p in a cluster", "file -bp victim.txt"},
	{"file --preserve-date", "file --preserve-date README.md"},
	// PowerShell 5.1 hands a native program a double quote inside a
	// single-quoted argument unescaped; the program's own parser then drops it
	// or splits there (each ran here against git.exe, or was seen with a
	// program that prints its command line).
	{"embedded quote hides --output", `git log '--out"put=victim.txt'`},
	{"doubled embedded quote splits off a branch name", `git branch '--sort=refname"" created-ref'`},
	{"doubled embedded quote splits off -D", `git branch '--sort=refname"" -D feature'`},
	{"embedded quote re-splits a later argument", `git log '--grep=a"' '--author=b --output=victim.txt'`},
	// Git for Windows' Unix tools expand a quoted glob themselves.
	{"quoted glob gives uniq two operands", "uniq '*.txt'"},
	// A read that goes to another machine.
	{"UNC path", `Get-Content \\attacker.example\share\x.txt`},
	{"UNC path with forward slashes", "Get-Content //attacker.example/share/x.txt"},
	{"UNC path, quoted", `Get-Content '\\attacker.example\share\x.txt'`},
	{"UNC path after -Path:", `Get-ChildItem -Path:\\attacker.example\share`},
	{"UNC path in a comma list", `Get-Content keep.txt,\\attacker.example\share\x.txt`},
	{"Get-Process -ComputerName", "Get-Process -ComputerName attacker.example"},
	{"ps -c HOST", "ps -c attacker.example"},
	{"bash /dev/tcp", "cat < /dev/tcp/127.0.0.1/9"},
	// PowerShell: one command prints a network path that is nowhere in the
	// line, and another takes it as its path, through a pipe, a variable or a
	// bracket (each read \\localhost\c$\Windows\win.ini here).
	{"a printed network path piped to Get-ChildItem", `Get-Date -Format "'//attacker.example/share/x'" | Get-ChildItem -Name`},
	{"a printed network path piped to Get-Item", "git log -1 --format=%x2f%x2fattacker.example%x2fshare%x2fx | Get-Item | Select-String y"},
	{"a printed network path piped to dir on the next line", "git log -1 --format=%x2f%x2fattacker.example%x2fshare |\ndir"},
	{"a printed network path through a variable", `Get-Date -Format "'//attacker.example/share/x'" -OutVariable p; Get-Content $p`},
	{"a printed network path through a bracket", `Get-ChildItem (Get-Date -Format "'//attacker.example/share'")`},
	{"a printed network path through a bracket, for sh's word", "wc -l $(git log -1 --format=%x2f%x2fattacker.example%x2fshare%x2fx)"},

	// Round two, finishing pass. Each was seen here: the network reads on
	// PowerShell 5.1 against \\localhost\c$ with Git for Windows' Unix tools on
	// PATH, the connections under bash --posix, git's programs with GIT_TRACE.
	// PowerShell ends an argument at the quote that began it: 'x'//h/s is x and
	// //h/s, and sh's one argument x//h/s is no network path.
	{"a quoted start splits off a network path, for a cmdlet", "Select-String 'x'//attacker.example/share/y"},
	{"a quoted start splits off a network path, for Get-Content", "cat 'x'//attacker.example/share/y"},
	{"a quoted start splits off a network path, for a program", "grep x 'a'//attacker.example/share/y"},
	{"a double-quoted start splits off a network path", `grep x "a"//attacker.example/share/y`},
	// A quote inside a quoted argument reaches a program unescaped, and the
	// program's own parser drops it: /"/h/s is //h/s to head.exe.
	{"a double quote inside single quotes hides a network path", `head -n 1 '/"/attacker.example/share/y'`},
	{"a single quote inside double quotes hides a network path", `grep x "/'/attacker.example/share/y"`},
	// Git for Windows' Unix tools read their arguments from @FILE.
	{"a file of arguments", "head @list.txt"},
	{"a file of arguments, quoted", "head '@list.txt'"},
	// bash opens /dev/tcp/HOST/PORT after quote removal and expansion.
	{"/dev/tcp behind quotes", "cat < /dev/t'c'p/127.0.0.1/9"},
	{"/dev/tcp behind a backslash", `cat < /dev/tc\p/127.0.0.1/9`},
	{"/dev/tcp behind an unset variable", "cat < /dev/tcp$x/127.0.0.1/9"},
	{"/dev/tcp glued to <, double-quoted", `cat </dev/t"cp"/127.0.0.1/9`},
	// git branch takes for-each-ref's fields: the signature ones run gpg, describe
	// runs git describe, as a sort key as well as in a format.
	{"git branch --sort=signature", "git branch --sort=signature"},
	{"git branch --sort=-signature:grade", "git branch --sort=-signature:grade"},
	{"git branch --sort=describe", "git branch --sort=describe"},
	{"git describe --broken", "git describe --broken"},
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
	{"a read-only word after a substitution", "echo $(pwd) ls"},
	{"null byte", "ls\x00"},
	{"delete character", "ls\x7f"},
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
	{"two commas", "sort a,,b"},
	{"a comma that leaves hostname a name", "hostname -f, x"},
	{"a backslash at the end", `sort in\`},
	{"a backslash before a tab", "sort in\\\tx"},
	{"a backslash before a double quote", `echo \"a`},
	{"a backslash before a parenthesis", `echo \(ls)`},
	{"a backslash before the last newline", "ls \\\n"},
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
	{"date -d, which sets the kernel's DST flag on some BSDs", "date -d yesterday"},
	{"date -u with a time to set", "date -u 010100002020"},
	{"date -r FILE with a time to set", "date -r go.mod 010100002020"},
	{"date -r without its value", "date -r"},
	{"date options in a cluster", "date -uR"},
	{"hostname -f with a name to set", "hostname -f newname"},
	{"hostname -F", "hostname -fF /tmp/x"},
	{"hostname --file", "hostname --file=/tmp/x"},
	{"hostname with a comma, which sh passes as the name", "hostname ,"},
	{"hostname with a no-break space, which sh passes as the name", "hostname  "},
	{"a comma PowerShell drops, leaving --output and its file", "git log --output, x"},
	{"a comma that leaves uniq a second operand", "uniq in, out"},
	{"git branch --sort with its value apart", "git branch --sort refname"},
	{"uniq -f N with an output file", "uniq -f 1 in out"},
	{"uniq -f without a number", "uniq -f in out"},
	{"uniq -w, which BSD uniq does not have", "uniq -w 1 in"},
	{"uniq with a quoted bracket glob", "uniq '[ab].txt'"},
	{"sort -Property and an output file", "sort -Property -o out"},
	{"sort -Prop, which is not Sort-Object's whole word", "sort -Prop x"},

	// Round two: refused because the shells, or a native program's own
	// parsing of what PowerShell 5.1 hands it, do not read them alike.
	{"double quote inside single quotes, one argument here", `git diff 'HEAD" --output=victim.txt'`},
	{"double quote inside single quotes for rg", `rg '--pr"e=pwned.exe' TODO .`},
	{"double quote inside single quotes for env", `env 'A=1" pwned.exe'`},
	{"single quote inside double quotes", `git log --grep="it's"`},
	{"a quoted argument that ends in a backslash", `git log '--grep=a b\' x`},
	{"a response file for Git's Unix tools", "sort '@keep.txt'"},
	// A POSIX word is taken only as written: HEAD is lwp-request, not head.
	{"HEAD is not head", "HEAD -m POST http://127.0.0.1:9/x"},
	{"Head", "Head -n 1 x"},
	{"GIT", "GIT status"},
	{"Git", "Git status"},
	{"LS, though PowerShell would take it", "LS"},
	{"Sort", "Get-Process | Sort CPU"},
	// Options that start another program, or reach the network.
	{"git log --help", "git log -1 --help"},
	{"git --help abbreviated", "git status --hel"},
	{"git %G in a later argument", "git log -1 --grep=x --format=%h%GS"},
	{"git remote show without -n", "git remote show --all origin"},
	{"rg -z", "rg -z TODO ."},
	{"rg --search-zip", "rg --search-zip TODO ."},
	{"file -z", "file -z x.gz"},
	{"file -Z", "file -Z x.gz"},
	{"file --uncompress", "file --uncompress x.gz"},
	{"brew info --github", "brew info --github wget"},
	{"brew outdated --fetch-HEAD", "brew outdated --fetch-HEAD"},
	{"/dev/udp", "cat < /dev/udp/10.0.0.1/53"},
	{"UNC path echoed for a bracket", `Get-Content (echo \\attacker.example\share\x)`},
	{"UNC path through the device namespace", `Get-Content \\?\UNC\attacker.example\share\x`},
	{"UNC path through the NT namespace", `Get-Content \??\UNC\attacker.example\share\x`},
	{"UNC path with three separators", `Get-Content \\\attacker.example\share\x`},
	{"UNC path with mixed separators", `Get-Content \/attacker.example/share/x`},
	{"UNC path attached to a short option", "git log -O//attacker.example/share/order"},
	{"UNC path after =", "sort --files0-from=//attacker.example/share/list"},
	{"UNC host alone", `Get-ChildItem \\attacker.example`},
	{"UNC host alone for a POSIX word", "ls //attacker.example"},
	{"UNC host alone as the place grep searches", "grep -r x //attacker.example"},
	// What a command prints must not become another command's path: a
	// bracket is an argument of echo and Write-Output only, the cmdlets that
	// take a piped string as a path take no pipe, and no PowerShell word sets
	// a variable.
	{"a bracket as cat's argument", "cat $(find . -name x)"},
	{"a bracket as wc's argument", "wc -l $(git ls-files)"},
	{"a bracket as Get-Item's argument", "Get-Item (Get-Location)"},
	{"a bracket after a tab", "Get-Item\t(Get-Location)"},
	{"a pipe into ls", "git ls-files | ls"},
	{"a pipe into gci", "echo x | gci"},
	{"a pipe into Get-ChildItem after a blank line", "echo x |\n\n  Get-ChildItem"},
	{"-OutVariable", `Get-Item C:\x -OutVariable f; echo $f.Length`},
	{"-OutVariable on an alias, in capitals", "ls -OUTVARIABLE x"},
	{"-ov", "Get-Date -ov x"},
	{"-ev:", "Get-ChildItem -ev:e"},
	{"-ErrorVariable", "dir -ErrorVariable e"},
	{"-WarningVariable", "Get-Content x -WarningVariable w"},
	{"-InformationVariable", "Write-Output x -InformationVariable i"},
	{"-PipelineVariable abbreviated", "Get-ChildItem -pi p | Select-String x"},
	{"-pv", "echo x -pv p"},
	{"-wv and -iv", "pwd -wv w -iv i"},
	{"-outv on sort", "Get-Process | sort CPU -outv s"},
	{"UNC host alone in an option of rg", "rg --ignore-file=//attacker.example x"},
	{"UNC path to git", "git diff --no-index a.txt //attacker.example/share/b.txt"},
	{"Get-Process -Comp", "Get-Process -Comp attacker.example"},
	{"Get-Process -cn:HOST", "Get-Process -cn:attacker.example"},
	{"double quote inside single quotes, three arguments", "git log --grep='a \"quoted\" word' -1"},
	{"git branch with an unknown flag", "git branch --frobnicate"},
	{"git branch --", "git branch -- x"},
	{"git remote show with a flag", "git remote show --frobnicate"},
	{"git remote show a path", "git remote show ../other"},
	{"brew with a substitution", "brew list $(echo x)"},

	// Round two, finishing pass.
	{"git branch --format with a signature field", "git branch --format=%(signature:grade)"},
	{"git branch --format=signature text, even without brackets", "git branch --format=signature"},
	{"git branch --sort=SIGNATURE, folded", "git branch --sort=SIGNATURE"},
	{"git log %(describe)", "git log -1 --format=%(describe)"},
	{"git describe --bro", "git describe --bro"},
	{"-p is -PipelineVariable for Get-Date", "Get-Date -p v"},
	{"-p is -PipelineVariable for echo", "echo hi -p v"},
	{"-P: for Write-Output", "Write-Output hi -P:v"},
	{"date -p, which is Get-Date where no date program is on PATH", `date -p C:\x`},
	{"date -OutVariable", `date -OutVariable C:\x`},
	{"brew info of a path", "brew info ./evil.rb"},
	{"brew info of a .rb", "brew info evil.rb"},
	{"brew info of a .json cask", "brew info --cask evil.JSON"},
	{"brew info of a tap-qualified name", "brew info someone/tap/formula"},
	{"brew info of a URL", "brew info https://evil.example/x.rb"},
	{"brew deps --brewfile", "brew deps --brewfile"},
	{"brew deps --for-each", "brew deps --for-each jq"},
	{"brew info --eval-all", "brew info --eval-all --json=v2"},
	{"a quote inside a quoted argument for a program", `grep "it's" notes.txt`},
	{"a quoted argument ending in a backslash for a program", `grep 'a b\' notes.txt`},
	{"PowerShell's --% for any word", "cat --% x"},
	{"a splat for any word", "echo @args"},
	{"two quoted pieces, which PowerShell keeps apart", `cat "x"'y'`},
	{"a variable in what < opens", "wc -l < $HOME/x"},
}

// The plain forms must go on running without a prompt.
var readOnlyPlain = []string{
	"env", "env FOO=bar", "env FOO=bar BAZ='a b'", "printenv", "printenv HOME",
	"date", "date +%s", "date '+%Y-%m-%d %H:%M'", "hostname",
	`Get-Item C:\x`, "Get-ChildItem", `Get-ChildItem C:\`, `Get-ChildItem 'C:\Program Files\'`,
	`Get-ChildItem C:\ | Select-String x`, `Get-Content C:\x\y.txt`, "Get-ChildItem -Recurse -Filter *.go",
	"Get-Process | Select-String tenant", "Get-Date", "Get-Location", "Write-Output hi", "dir", `gci C:\`,
	"Get-ChildItem | sort Name", "Write-Output (Get-Location)", "echo\t(pwd)", "Get-ChildItem | Get-Content",
	"git log --oneline | cat", "Get-ChildItem -ErrorAction SilentlyContinue", "ls -o", "ls -e", "ps -p 1", "cat -e x", "Get-Content x -Wait",
	"git status", "git status && git log -3", "git log --oneline -5", "git log --format='%h %s' -n 5",
	"git log --oneline | head -5", "git diff --stat HEAD~1", "git show HEAD:README.md", "git diff -- src/main.go",
	"git diff src\\main.go", "git rev-parse HEAD", "git ls-files", "git blame -L 1,5 x.go", "git describe --tags",
	"git log --output-indicator-new=+ -1", "git log --oneline --grep='fix bug'",
	"git branch", "git branch -a", "git branch -vv", "git branch --show-current", "git branch --list 'feat/*'",
	"git branch --contains HEAD", "git branch --merged main", "git branch --sort=-committerdate --format=%H",
	"git remote", "git remote -v", "git remote get-url origin", "git remote get-url --push origin",
	"git remote show -n origin", "git remote show origin -n",
	"ls -la", "ls -la | grep x", "ls ~/src", "ls *.go", "cat x | grep y | wc -l", "cat $HOME/.profile",
	"echo $(whoami)", "echo $(echo $(whoami))", "echo $(whoami) $(pwd)", "echo $(find . -name x)", "echo hi # a note",
	"echo \"it's\"", "echo 'say \"hi\"'", "grep -E \"a b\" f", "grep -rn TODO .", "wc -l < file", "ls &",
	"ls\npwd", "ls\r\npwd", "ls;", "\tls\t", "ls\t-la",
	// PowerShell's own names, which no POSIX system has, in any case.
	"GET-CHILDITEM", "get-childitem -Force", "Get-childItem C:\\", "GCI", "select-string -Path *.go -Pattern x",
	// Round two: look-only forms that had been refused and run unasked again.
	"date -u", "date -u +%s", "date --utc", "date -R", "date -Iseconds", "date -I", "date --iso-8601",
	"date --iso-8601=seconds", "date --rfc-3339=seconds", "date -r go.mod", "date -r go.mod +%s", "date --reference=go.mod",
	"hostname -f", "hostname -s", "hostname -I", "hostname --fqdn",
	"sort -t, -k1,1 data.csv", "uniq -f 1 names.txt", "uniq -s 2 -c names.txt",
	"Get-Process | sort -Property CPU", "Get-ChildItem | sort -Property Length", "sort -property Name",
	// Round two: near neighbours of what is refused that only look.
	"git status -h", "git log --format=%h%g", "git log --show-notes -1", "git log --stat --since=1.week",
	"rg '// TODO'", "grep -rn '//go:build' .", "rg '\\\\d+' src", "echo https://example.com/a/b", "cat src//pkg/x.go",
	`Get-Content C:\\Users\\me\\notes.txt`, "Get-Process -Name tenant", "ps -ef", "cat /dev/null",
	"file -b --mime-type go.mod", "file -i README.md", "rg -n TODO -g '*.go'", "brew info --json=v2 jq",
	"sort -t: -k2 f", "env A=1, B=2", "sort -t,", "sort a,\tb", "sort ,a", "git log --grep='$HOME' -1",
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
	// Round two, finishing pass: the nearest neighbours that only look. %G is no
	// field of git branch (git: "unknown field name"); -p is ambiguous for
	// Get-Process on 5.1; a bare start and a quoted rest stay one argument in
	// both shells; a cmdlet takes a quote inside a quoted argument as text.
	"git branch --sort=%G", "git branch --sort=v:refname", "git branch --sort=-objectsize", "git describe --tags --dirty",
	"git log --no-show-signature -1", "ps -p 1", "Get-Process -Id 1", "cat 'notes.txt'", "grep 'x y' f",
	"git log --format='%h %s' -n'3'", "cat < go.mod", "wc -l <go.mod", "echo 'a\"b'", "Select-String \"it's\" x",
	"brew info python@3.12", "brew list --cask", "brew info --json=v2 jq libxml2",
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
	roTestSeps    = regexp.MustCompile(`&&|\|\||\$\(|[;&|\n()]`)
	roGluedParen  = regexp.MustCompile(`(^|[^ \t$])\(`)
	roEmptyGroup  = regexp.MustCompile(`\([ \t\n]*\)`)
	roAfterGroup  = regexp.MustCompile(`\)[ \t]*([^ \t\n;|&()$]|\$[^(]|\$$)`)
	roEscapedSep  = regexp.MustCompile(`\\(\r?\n|[;&|()]|\$\()`)
	roBareWord    = regexp.MustCompile(`^[A-Za-z-]+$`)
	roAssignment  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	roNumber      = regexp.MustCompile(`^[0-9]+$`)
	roNetworkPath = regexp.MustCompile(`^[\\/]{2,}[^\\/\s]+[\\/]`) // \\host\share, //host/share
	roStrictWords = map[string]bool{"env": true, "date": true, "hostname": true, "find": true, "sort": true, "uniq": true, "tree": true, "file": true, "rg": true, "git": true, "brew": true}
	roCmdlets     = map[string]bool{"get-childitem": true, "gci": true, "dir": true, "ls": true, "get-item": true, "get-content": true, "cat": true, "type": true, "get-location": true, "pwd": true, "get-process": true, "ps": true, "get-date": true, "select-string": true, "write-output": true, "echo": true, "sort": true}
	roGitVerbs    = map[string]bool{"status": true, "log": true, "diff": true, "show": true, "rev-parse": true, "ls-files": true, "blame": true, "describe": true, "branch": true, "remote": true}
	roFindActs    = map[string]bool{"-delete": true, "-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-fprint": true, "-fprint0": true, "-fprintf": true, "-fls": true}
	roBranchActs  = map[string]bool{"-d": true, "-D": true, "-m": true, "-M": true, "-c": true, "-C": true, "-f": true, "--force": true, "--delete": true, "--move": true, "--copy": true, "-u": true, "--set-upstream-to": true, "--unset-upstream": true, "--edit-description": true, "--track": true, "-t": true}
	// What date and hostname print with, written out again here rather than
	// read from the classifier's tables.
	roDateShows     = map[string]bool{"-u": true, "--utc": true, "--universal": true, "-R": true, "--rfc-email": true, "--rfc-2822": true, "--iso-8601": true, "-I": true, "-Idate": true, "-Ihours": true, "-Iminutes": true, "-Iseconds": true, "-Ins": true}
	roHostnameShows = map[string]bool{"-f": true, "-s": true, "-i": true, "-I": true, "-A": true, "-d": true, "--fqdn": true, "--long": true, "--short": true, "--ip-address": true, "--all-ip-addresses": true, "--all-fqdns": true, "--domain": true}
	roAppendedActs  = []string{"; rm -rf x", " | sh", " && sudo ls", "\nrm x", " > out", "; x=1", " | xargs rm", "; (Get-Item x).Delete()", " `id`", "; ./ls", "; env sh", "; $(echo rm x)", "; [IO.File]::Delete('x')", " | sort { rm x }", "\rrm x",
		"  [DANGER: x]; rm -rf x", "; git status --help", "; cat //host/share/x", "; cat < /dev/tcp/host/80", "; HEAD x", "; sort -T . x", "; file -p x",
		"; cat 'x'//host/share/y", "; head '/\"/host/share/y'", "; cat < /dev/t'c'p/host/80", "; git branch --sort=signature", "; Get-Date -p v", "; head @x"}
	roPrefixedActs = []string{"sudo ", "X=1 ", "exec ", "command ", "time ", "nohup ", "xargs ", "rm x; ", "$(echo sh) "}
)

// roLong reports whether a is the long option name, as --name or --name=value,
// or a shorter spelling of it that getopt would take for it.
func roLong(a, name string) bool {
	long, isLong := strings.CutPrefix(a, "--")
	long, _, _ = strings.Cut(long, "=")
	return isLong && long != "" && strings.HasPrefix(name, long)
}

// roShort reports whether a is a single-dash cluster holding one of letters.
func roShort(a, letters string) bool {
	return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, letters)
}

// roSplit is a command's arguments after its word, split at blanks outside
// quotes, the quotes dropped: enough to tell where an argument begins.
func roSplit(piece string) []string {
	var out []string
	var b strings.Builder
	quote, in := rune(0), false
	for _, r := range piece {
		switch {
		case quote == 0 && (r == '\'' || r == '"'):
			quote, in = r, true
		case r == quote:
			quote = 0
		case quote == 0 && (r == ' ' || r == '\t'):
			if in {
				out = append(out, b.String())
			}
			b.Reset()
			in = false
		default:
			b.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, b.String())
	}
	if len(out) > 0 {
		out = out[1:]
	}
	return out
}

// readOnlyViolation names the invariant a command ReadOnly accepted breaks,
// or "" when it holds them all.
func readOnlyViolation(c string) string {
	if !utf8.ValidString(c) {
		return "invalid UTF-8"
	}
	for _, bad := range []string{">", "{", "}", "`", "::", "/dev/tcp/", "/dev/udp/"} {
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
	t := lf
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
	pos, words, closed, piped := 0, 0, false, false // the separator before this piece was a ")"; a "|"
	for _, m := range seps {
		piece, sep := t[pos:m[0]], t[m[0]:m[1]]
		pos = m[1]
		wasClosed, wasPiped := closed, piped
		closed = sep == ")"
		f := strings.Fields(piece)
		// A pipe feeds the next command, also when lines end in between.
		piped = sep == "|" || (wasPiped && len(f) == 0 && sep == "\n")
		if len(f) == 0 {
			if (sep == "(" || sep == "$(") && !wasClosed {
				return "a group in the command position"
			}
			continue
		}
		words++
		// The word as written is in the table; only a name PowerShell alone has
		// (a hyphen in it, or gci) may differ from it in case.
		word := strings.ToLower(f[0])
		_, asWritten := readOnlyWords[f[0]]
		_, folded := readOnlyWords[word]
		if !roBareWord.MatchString(f[0]) || !(asWritten || (folded && (strings.Contains(word, "-") || word == "gci"))) {
			return "the command word " + f[0] + " is not a bare read-only word"
		}
		given := roSplit(piece)
		for _, a := range given {
			if roNetworkPath.MatchString(a) {
				return "an argument is a network path: " + a
			}
			if p, _, _ := strings.Cut(strings.ToLower(a), ":"); (word == "ps" || word == "get-process") && (p == "-cn" || (len(p) > 1 && strings.HasPrefix("-computername", p))) {
				return "Get-Process asks another machine: " + a
			}
		}
		// What a command prints is nobody's path: a bracket is an argument of
		// the two words that only print, the cmdlets that read a piped string
		// as a path have no pipe before them, and no cmdlet sets a variable.
		if (sep == "(" || sep == "$(") && word != "echo" && word != "write-output" {
			return word + " takes a bracket as an argument"
		}
		if wasPiped && (word == "get-childitem" || word == "gci" || word == "dir" || word == "ls" || word == "get-item") {
			return word + " takes its path from a pipe"
		}
		if roCmdlets[word] {
			for _, a := range given {
				p, _, _ := strings.Cut(strings.ToLower(a), ":")
				for full, least := range map[string]int{"-outvariable": 5, "-errorvariable": 7, "-warningvariable": 9, "-informationvariable": 13, "-pipelinevariable": 3} {
					if len(p) >= least && strings.HasPrefix(full, p) {
						return word + " sets a variable: " + a
					}
				}
				if p == "-ov" || p == "-ev" || p == "-wv" || p == "-iv" || p == "-pv" ||
					(p == "-p" && (word == "get-date" || word == "write-output" || word == "echo")) {
					return word + " sets a variable: " + a
				}
			}
		}
		for _, a := range given {
			if strings.HasPrefix(a, "@") || a == "--%" {
				return "PowerShell or a Unix tool of Git for Windows reads more into " + a
			}
		}
		// A quote of the other kind inside a quoted argument: a program parsing
		// the command line PowerShell builds sees other arguments (a cmdlet does
		// not parse one). And a quote that begins an argument and is followed by
		// text: PowerShell makes two arguments of it, sh one.
		quote, prev := rune(0), ' '
		quoteStarts := false
		closed := false // the rune before this one closed a quote
		for _, r := range piece {
			if closed && quoteStarts && r != ' ' && r != '\t' {
				return "PowerShell splits a quoted start from the text after it"
			}
			closed = false
			switch {
			case quote == 0 && (r == '\'' || r == '"'):
				quote, quoteStarts = r, prev == ' ' || prev == '\t'
			case r == quote:
				quote, closed = 0, true
			case quote != 0 && (r == '\'' || r == '"') && !roCmdlets[word]:
				return word + " takes a quote inside a quoted argument"
			}
			prev = r
		}
		// What < opens, as sh opens it: quotes and backslashes removed.
		if i := strings.LastIndex(piece, "<"); i >= 0 {
			target := strings.Fields(strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(piece[i+1:]) + " x")[0]
			if strings.Contains(target, "$") || strings.Contains(target, "/dev/tcp/") || strings.Contains(target, "/dev/udp/") {
				return "< opens a connection, or a name only expansion tells: " + target
			}
		}
		strict := roStrictWords[word]
		if !strict || strings.ContainsAny(piece, `'"\`) {
			continue // strings.Fields reads a quoted or escaped argument wrongly
		}
		args := f[1:]
		for _, a := range args {
			if strings.Contains(a, "$") {
				return word + " takes a variable as an argument"
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
			for _, a := range args {
				if !roHostnameShows[a] {
					return "hostname is given something that does not only print: " + a
				}
			}
		case "date":
			for i := 0; i < len(args); i++ {
				a := args[i]
				switch {
				case a == "-r" && i+1 < len(args):
					i++ // the file or the seconds whose time to print
				case !strings.HasPrefix(a, "+") && !roDateShows[a] && !strings.HasPrefix(a, "--iso-8601=") &&
					!strings.HasPrefix(a, "--rfc-3339=") && !strings.HasPrefix(a, "--reference="):
					return "date is given something that does not only print: " + a
				}
			}
		case "find":
			for _, a := range args {
				if roFindActs[a] {
					return "find acts: " + a
				}
			}
		case "sort":
			for _, a := range args {
				if strings.EqualFold(a, "-Property") {
					continue // Sort-Object's; no sort program takes an option P or p
				}
				if roLong(a, "output") || roLong(a, "temporary-directory") || roLong(a, "compress-program") || roShort(a, "oT") {
					return "sort writes a file or runs a program: " + a
				}
			}
		case "tree":
			for _, a := range args {
				if roLong(a, "output") || roShort(a, "oR") {
					return "tree writes a file: " + a
				}
			}
		case "uniq":
			operands := 0
			for i := 0; i < len(args); i++ {
				switch {
				case (args[i] == "-f" || args[i] == "-s") && i+1 < len(args) && roNumber.MatchString(args[i+1]):
					i++
				case !strings.HasPrefix(args[i], "-"):
					operands++
				}
			}
			if operands > 1 {
				return "uniq is given an output file"
			}
		case "file":
			for _, a := range args {
				if a == "--compile" || a == "--preserve-date" || a == "--uncompress" || a == "--uncompress-noreport" || roShort(a, "CpzZ") {
					return "file writes, or starts a program: " + a
				}
			}
		case "rg":
			for _, a := range args {
				if a == "--pre" || strings.HasPrefix(a, "--pre=") || a == "--hostname-bin" || strings.HasPrefix(a, "--hostname-bin=") ||
					a == "--search-zip" || roShort(a, "z") {
					return "rg runs a program: " + a
				}
			}
		case "brew":
			for _, a := range args[min(1, len(args)):] {
				if roLong(a, "github") || roLong(a, "fetch-HEAD") || roLong(a, "brewfile") || roLong(a, "eval-all") || roLong(a, "for-each") {
					return "brew opens a browser, fetches or evaluates Ruby: " + a
				}
				// A formula by its plain name: a path, a URL or a tap-qualified
				// name makes brew load (and run) a .rb, or fetch.
				l := strings.ToLower(a)
				if !strings.HasPrefix(a, "-") && (strings.ContainsAny(a, `/\:`) || strings.HasSuffix(l, ".rb") || strings.HasSuffix(l, ".json")) {
					return "brew loads a formula from a path or a tap: " + a
				}
			}
		case "git":
			if len(args) == 0 || !roGitVerbs[args[0]] {
				return "git without a read subcommand"
			}
			if args[0] == "remote" && len(args) > 1 && args[1] == "show" && !slices.Contains(args, "-n") {
				return "git remote show connects to the remote"
			}
			for _, a := range args {
				if a == "--output" || strings.HasPrefix(a, "--output=") {
					return "git writes a file: " + a
				}
				if a == "--help" || a == "--show-signature" || roLong(a, "broken") {
					return "git starts another program: " + a
				}
				// pretty-formats: %G… verifies a signature, %(describe) runs git
				// describe. git branch takes for-each-ref's fields instead, where
				// the signature ones and describe do the same and %G is nothing.
				lower := strings.ToLower(a)
				if args[0] != "branch" && (strings.Contains(a, "%G") || strings.Contains(lower, "%(describe")) {
					return "git starts another program: " + a
				}
				if args[0] == "branch" && (strings.HasPrefix(a, "--format=") || strings.HasPrefix(a, "--sort=")) &&
					(strings.Contains(lower, "signature") || strings.Contains(lower, "describe")) {
					return "git branch starts another program: " + a
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
	if !ReadOnly("\n \t" + c + " \t") {
		return "outer blanks change the answer"
	}
	if !strings.HasSuffix(c, `\`) && !ReadOnly(c+"\n") {
		return "a final newline changes the answer"
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
