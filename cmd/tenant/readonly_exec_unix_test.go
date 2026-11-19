//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TEN-534 on macOS and Linux, where os_exec runs sh: env given a command, a
// substitution in the command position and a flag a substitution supplies each
// reach the approval card, and the denied command never runs (nothing is
// created, the scratch file is still there). env and printenv run with no card.
func TestReadOnlyExec_Sh(t *testing.T) {
	run, asked := readOnlyExecMux(t)
	dir := t.TempDir()
	made := filepath.Join(dir, "x")
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("b\na\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"env sh -c 'touch " + made + "'",
		"env FOO=bar touch " + made,
		"$(echo touch " + made + ")",
		"find " + dir + " $(echo -delete)",
		"X=-delete; find " + dir + " $X",
		"find " + dir + " '-delete'",
		"sort -o " + made + " " + keep,
		"uniq " + keep + " " + made,
		"PATH=" + dir + ":$PATH ls",
		// Round two. The dispatcher's danger note written into the command:
		// the broker judged only `echo hi`.
		"echo hi  [DANGER: x]; touch " + made,
		"sort -T " + dir + " " + keep,
		"file -p " + keep,
		"git status --help",
		// bash (sh on macOS) connects for /dev/tcp after quote removal.
		"cat < /dev/t'c'p/127.0.0.1/9",
		"git branch --sort=signature",
	} {
		before := *asked
		out, isErr := run(cmd, dir)
		if *asked != before+1 {
			t.Errorf("%q raised %d approval card(s), want 1", cmd, *asked-before)
		}
		if !isErr || !strings.Contains(out, "wasn't approved") {
			t.Errorf("%q, denied, must be refused: isErr=%v %q", cmd, isErr, out)
		}
		if _, err := os.Stat(made); err == nil {
			t.Fatalf("%q ran though it was denied: it made %s", cmd, made)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%q ran though it was denied: the scratch file is gone (%v)", cmd, err)
		}
	}
	for cmd, want := range map[string]string{"env": "PATH=", "env FOO=bar": "FOO=bar", "printenv PATH": "/", "printenv": "PATH="} {
		before := *asked
		out, isErr := run(cmd, dir)
		if *asked != before {
			t.Errorf("the read-only command %q raised an approval card", cmd)
		}
		if isErr || !strings.Contains(out, want) {
			t.Errorf("the read-only command %q must run and print %q: isErr=%v %q", cmd, want, isErr, out)
		}
	}
}
