package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every line enroll prints for Windows, run by the real Windows PowerShell
// (5.1, the one every Windows has), puts the code where install.ps1 or
// `tenant child install` reads it, unchanged: in $env:TENANT_ENROLL_CODE, or
// on the pipeline. irm and tenant are stand-ins that only say what they were
// given (irm's alias is taken away first, and the script stops if it is
// still there); nothing is fetched or installed.
func TestEnrollPasteInWindowsPowerShell(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("no Windows PowerShell")
	}
	t.Setenv("TENANT_UPDATE_REPO", "acme/tenant")
	const stand = `$ErrorActionPreference = 'Stop'
Remove-Item -Force Alias:irm -ErrorAction SilentlyContinue
if (Get-Alias irm -ErrorAction SilentlyContinue) { throw 'irm is still the real one' }
function irm { param($u) $global:url = $u; 'Write-Output ("child={0} code={1} url={2}" -f $env:TENANT_CHILD, $env:TENANT_ENROLL_CODE, $global:url)' }
function tenant { $in = @($input) -join ''; Write-Output ("args={0} stdin={1}" -f ($args -join ' '), $in) }
`
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "TENANT_") {
			env = append(env, kv)
		}
	}
	script := filepath.Join(t.TempDir(), "paste.ps1")
	n := 0
	for _, code := range pasteCodes {
		lines, _ := enrollPaste(code)
		for _, l := range lines {
			if !strings.HasPrefix(l.For, "Windows") {
				continue
			}
			n++
			if err := os.WriteFile(script, []byte(stand+l.Line+"\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
			hideWindow(cmd)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%q: %v\n%s", l.For, err, out)
			}
			want := ""
			switch {
			case strings.Contains(l.Line, "install.ps1"):
				want = "child=system code=" + code + " url=https://github.com/acme/tenant/releases/latest/download/install.ps1"
			default:
				want = "args=child install --system stdin=" + code
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("%q in PowerShell gave %q, want %q", l.For, got, want)
			}
		}
	}
	if n != 2*len(pasteCodes) {
		t.Errorf("%d lines run, want %d", n, 2*len(pasteCodes))
	}
}
