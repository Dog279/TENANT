package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"tenant/internal/procprotect"
)

// fillLoginPath gives Tenant on macOS the PATH a login shell has (TEN-489).
// Started by launchd (serve) it gets only /usr/bin:/bin:/usr/sbin:/sbin, so
// brew, node and friends were "not found" in os_exec. path_helper is the
// OS's own builder (/etc/paths, /etc/paths.d); Homebrew's prefixes are added
// when present because its installer puts them in the shell rc instead.
func fillLoginPath() {
	if runtime.GOOS != "darwin" {
		return
	}
	dirs := filepath.SplitList(os.Getenv("PATH"))
	if out, err := procprotect.Output(exec.Command("/usr/libexec/path_helper", "-s")); err == nil {
		// Output: PATH="a:b:c"; export PATH;
		if _, v, ok := strings.Cut(string(out), `PATH="`); ok {
			v, _, _ = strings.Cut(v, `"`)
			dirs = append(dirs, filepath.SplitList(v)...)
		}
	}
	for _, d := range []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin"} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			dirs = append(dirs, d)
		}
	}
	var path []string
	for _, d := range dirs { // first occurrence wins, so the inherited order stays first
		if d != "" && !slices.Contains(path, d) {
			path = append(path, d)
		}
	}
	_ = os.Setenv("PATH", strings.Join(path, string(os.PathListSeparator)))
}
