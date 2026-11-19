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
