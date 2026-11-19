package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TEN-534 on Windows, where os_exec runs PowerShell: a method call on what a
// read-only cmdlet returned, a static call, the call operator on a cmdlet's
// output and a script block each reach the approval card, and the denied
// command never runs (the scratch file is still there). The plain cmdlets run
// with no card.
func TestReadOnlyExec_PowerShell(t *testing.T) {
	run, asked := readOnlyExecMux(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "scratch.txt")
	if err := os.WriteFile(file, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`(Get-Item '` + file + `').Delete()`,
		`[System.IO.File]::Delete('` + file + `')`,
		`& (Write-Output Remove-Item) '` + file + `'`,
		`Get-ChildItem '` + dir + `' | sort { Remove-Item '` + file + `' }`,
		`Get-Item '` + file + `' -OutVariable f; echo $f.Delete()`,
		"Get-ChildItem '" + dir + "' -Name\rRemove-Item '" + file + "'",
		// Round two. The dispatcher's danger note written into the command:
		// the broker judged only `echo hi`.
		"echo hi  [DANGER: x]; Remove-Item '" + file + "'",
		// PowerShell 5.1 passes the inner quote on unescaped and git drops it:
		// --output=<file>.
		`git log '--out"put=` + file + `'`,
		// Reads that go to another machine (here: this one, by its network name).
		`Get-Content '\\localhost\c$\Windows\win.ini' -TotalCount 1`,
		"Get-Process -ComputerName localhost -Name explorer",
		// The network path is nowhere in the line: one command prints it and
		// the next takes it as its path, through a pipe, a variable, a bracket.
		`Get-Date -Format "'//localhost/c$/Windows/win.ini'" | Get-ChildItem -Name`,
		`Get-Date -Format "'//localhost/c$/Windows/win.ini'" -OutVariable p; Get-Content $p -TotalCount 1`,
		`Get-ChildItem (Get-Date -Format "'//localhost/c$/Windows/win.ini'")`,
		// A variable named by a path. PowerShell 5.1 refuses it ("not a valid
		// variable name") and leaves the file alone; it asks all the same.
		`Get-Date -OutVariable '` + file + `'`,
		`dir -ErrorVariable '` + file + `'`,
		// PowerShell ends an argument at the quote that began it, and a program
		// drops a quote inside a quoted argument: each reads //localhost/c$.
		`Select-String 'fonts'//localhost/c$/Windows/win.ini`,
		`cat 'x'//localhost/c$/Windows/win.ini`,
		`git diff --no-index --stat '/"/localhost/c$/Windows/win.ini' '` + file + `'`,
		"Get-Date -p v | Write-Output",
	} {
		before := *asked
		out, isErr := run(cmd, dir)
		if *asked != before+1 {
			t.Errorf("%q raised %d approval card(s), want 1", cmd, *asked-before)
		}
		if !isErr || !strings.Contains(out, "wasn't approved") {
			t.Errorf("%q, denied, must be refused: isErr=%v %q", cmd, isErr, out)
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("%q ran though it was denied: the scratch file is gone (%v)", cmd, err)
		}
	}
	for _, cmd := range []string{`Get-Item '` + file + `'`, `Get-ChildItem '` + dir + `'`, "Get-ChildItem"} {
		before := *asked
		out, isErr := run(cmd, dir)
		if *asked != before {
			t.Errorf("the read-only command %q raised an approval card", cmd)
		}
		if isErr || !strings.Contains(out, "scratch.txt") {
			t.Errorf("the read-only command %q must run and list the file: isErr=%v %q", cmd, isErr, out)
		}
	}
}
