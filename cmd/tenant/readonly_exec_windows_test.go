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
	// A common parameter that takes a variable name, given a file's path, stays
	// read-only: Windows PowerShell 5.1 refuses a drive or provider path there
	// ("not a valid variable name") and leaves the file as it was. Should an
	// engine ever write through such a name, this fails.
	for _, cmd := range []string{
		`Get-Date -OutVariable '` + file + `'`,
		`Get-ChildItem '` + dir + `' -ErrorVariable '` + file + `'`,
		`dir -PipelineVariable '` + file + `'`,
		`Get-Date -ov:'` + file + `'`,
		`Get-Content '` + file + `' -OutVariable '` + file + `'`,
	} {
		before := *asked
		out, _ := run(cmd, dir)
		if *asked != before {
			t.Errorf("%q raised an approval card", cmd)
		}
		if !strings.Contains(out, "not a valid variable name") {
			t.Errorf("%q: PowerShell did not refuse the path as a variable name: %q", cmd, out)
		}
		if b, err := os.ReadFile(file); err != nil || string(b) != "keep me\n" {
			t.Fatalf("%q changed the scratch file: %q, %v", cmd, b, err)
		}
	}
}
