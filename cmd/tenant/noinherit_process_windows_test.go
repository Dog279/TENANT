//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A folder whose permissions are its own and are not passed on to what is
// made in it (icacls /inheritance:r, with the entries not inheritable). A
// file or folder made there takes the default of the process that makes it,
// and a protected Tenant's default is the list that keeps other programs out
// of its threads: on a file that list lets the owner do nothing (TEN-535
// hardening, the tester's regression). This runs the real, protected binary
// there, as serve, chat and an export, and reads back everything it made as
// the user's other programs would.
func TestAFolderWithNoInheritablePermissions(t *testing.T) {
	first, _ := forcedControls(t) // the error-reporting exclusion, forced off: nothing is written to the registry
	bin := protectTestBinary(t)
	forced := []string{"TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test"}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(t.TempDir(), "no-inherit")
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(folder), &sa); err != nil {
		t.Fatal(err)
	}

	u := newTenantUnderTest(t, bin)
	// The config dir is there already, made by another program (this test),
	// with nothing to pass on either; the data dir is Tenant's to make.
	u.cfg, u.data = filepath.Join(folder, "config"), filepath.Join(folder, "data")
	if err := os.Mkdir(u.cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // an address nothing listens on, so no command dials a serve
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := os.WriteFile(launchConfigPath(u.cfg), []byte(fmt.Sprintf(`{"dashboard":{"addr":%q}}`, addr)), 0o600); err != nil {
		t.Fatal(err)
	}

	// serve makes the data dir and its log book, and writes its settings
	// into the config dir; chat then opens the same databases again.
	book := filepath.Join(u.data, "logs", "logbook.db")
	u.serves(t, append([]string{"TENANT_TEST_TERMINAL=1"}, forced...), "y\n", func() bool {
		_, err := os.Stat(book)
		return err == nil
	})
	if code, stdout, stderr := u.exits(t, forced, "one line turn\n", "chat", "--backend", "echo", "--data", u.data, "--config", u.cfg); code != 0 {
		t.Errorf("chat, in the data dir made there: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	out := filepath.Join(folder, "export.ndjson")
	code, stdout, stderr := u.exits(t, forced, "", "logs", "export", "--out", out, "--data", u.data, "--config", u.cfg)
	if code != 0 || !strings.Contains(stdout, "exported to") {
		t.Errorf("logs export --out, into the folder: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// A command that makes its output with the plain os calls, in the folder
	// itself: neither fsperm's nor in a directory of Tenant's own.
	runs := filepath.Join(folder, "runs.jsonl")
	if code, stdout, stderr := u.exits(t, forced, "", "runs", "export", "--data", u.data, "--config", u.cfg, "--out", runs); code != 0 {
		t.Errorf("runs export --out, into the folder: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(runs); err != nil {
		t.Errorf("the runs export: %v", err)
	}

	// Everything Tenant made there, read as its owner.
	files := 0
	err = filepath.WalkDir(folder, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			t.Errorf("%s cannot be listed by its owner: %v", path, err)
			return nil
		}
		if _, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); err != nil {
			t.Errorf("%s: its owner cannot read its access list: %v", path, err)
		}
		if d.IsDir() {
			return nil
		}
		files++
		f, err := os.Open(path) // sharing as os.Open does: a database Tenant has closed opens
		if err != nil {
			t.Errorf("%s cannot be read by its owner: %v", path, err)
			return nil
		}
		return f.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the export Tenant said it wrote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(u.data, "episodes.db")); err != nil {
		t.Errorf("the data dir Tenant was to make: %v", err)
	}
	t.Logf("%d files under %s, each read by its owner", files, folder)
}
