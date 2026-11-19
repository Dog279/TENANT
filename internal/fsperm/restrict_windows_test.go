package fsperm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func daclOf(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return sd.String()
}

// A directory of Tenant's own that passes no permissions on is made to pass
// its own on, to what is in it and to what is made in it; nobody gains or
// loses access to it. One that passes something on is left as it is.
func TestMkdirAllPassesPermissionsOn(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + sid + ")(A;;FR;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	dir := filepath.Join(t.TempDir(), "own")
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(dir), &sa); err != nil {
		t.Fatal(err)
	}
	sub, old := filepath.Join(dir, "sub"), filepath.Join(dir, "old.txt")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := daclOf(t, dir); strings.Contains(got, "OI") || strings.Contains(got, "CI") {
		t.Fatalf("the directory passes permissions on before the test: %s", got)
	}

	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	if got, want := daclOf(t, dir), "D:PAI(A;OICI;FA;;;"+sid+")(A;OICI;FR;;;BA)"; got != want {
		t.Errorf("the directory: %s, want its own two entries, inherited from now on: %s", got, want)
	}
	inherited := "(A;OICIID;FA;;;" + sid + ")"
	if got := daclOf(t, sub); !strings.Contains(got, inherited) {
		t.Errorf("a folder that was in it: %s, want %s among its entries", got, inherited)
	}
	if got, want := daclOf(t, old), "(A;ID;FA;;;"+sid+")"; !strings.Contains(got, want) {
		t.Errorf("a file that was in it: %s, want %s among its entries", got, want)
	}
	fresh := filepath.Join(sub, "new.txt")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := daclOf(t, fresh), "D:AI(A;ID;FA;;;"+sid+")(A;ID;FR;;;BA)"; got != want {
		t.Errorf("a file made in a folder of it: %s, want only what the directory passes on: %s", got, want)
	}

	// A second call, and a directory that inherits like any other: unchanged.
	before := daclOf(t, dir)
	if err := MkdirAll(dir); err != nil || daclOf(t, dir) != before {
		t.Errorf("a second MkdirAll: %v, %s; want %s", err, daclOf(t, dir), before)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	before = daclOf(t, plain)
	if err := MkdirAll(plain); err != nil || daclOf(t, plain) != before {
		t.Errorf("a directory that inherits: %v, %s; want it unchanged: %s", err, daclOf(t, plain), before)
	}
}
