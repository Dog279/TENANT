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

// folderWith makes a folder with exactly the access list sddl says.
func folderWith(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(path), &sa); err != nil {
		t.Fatal(err)
	}
}

func userSID(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid.String()
}

// A folder that was there before is not Tenant's to change, whatever is
// wrong with it. Round three of TEN-535: a user's folder with "owner: full"
// and "Everyone: read", both for this folder only, was handed to Tenant as
// its data dir; MkdirAll marked both entries as inherited, and a file in it
// that Everyone could not read became one Everyone could. Nothing that was
// there may gain or lose anything: not the folder, not a file in it, not a
// folder in it, not the folder above.
func TestMkdirAllLeavesAFolderThatWasThereAlone(t *testing.T) {
	sid := userSID(t)
	above := filepath.Join(t.TempDir(), "above")
	folderWith(t, above, "D:P(A;;FA;;;"+sid+")")
	dir := filepath.Join(above, "the users own")
	folderWith(t, dir, "D:P(A;;FA;;;"+sid+")(A;;0x1200a9;;;WD)")
	private, old := filepath.Join(dir, "private.txt"), filepath.Join(dir, "old folder")
	if err := os.WriteFile(private, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{above, dir, private, old}
	before := map[string]string{}
	for _, p := range paths {
		before[p] = daclOf(t, p)
	}
	if strings.Contains(before[private], ";WD)") {
		t.Fatalf("the file is Everyone's to read before the test: %s", before[private])
	}

	for range 2 {
		if err := MkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	// What Tenant then makes in it, through this package: its owner's.
	mine := filepath.Join(dir, "tenant.db")
	if err := WriteFile(mine, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(dir, "settings.json"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(mine); err != nil {
		t.Errorf("a file Tenant made there, read by its owner: %v", err)
	}
	for _, p := range paths {
		if got := daclOf(t, p); got != before[p] {
			t.Errorf("%s was changed:\n  before %s\n  after  %s", p, before[p], got)
		}
	}
}

// A directory MkdirAll makes itself, where nothing is inherited from the
// folder above, passes its own permissions on to what is made in it, by any
// part of the program and in any way. The folder above, and what was in it,
// stay as they were.
func TestMkdirAllMakesItsOwnDirectoryPassPermissionsOn(t *testing.T) {
	sid := userSID(t)
	above := filepath.Join(t.TempDir(), "above")
	folderWith(t, above, "D:P(A;;FA;;;"+sid+")(A;;0x1200a9;;;WD)")
	neighbour := filepath.Join(above, "neighbour.txt")
	if err := os.WriteFile(neighbour, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{above: daclOf(t, above), neighbour: daclOf(t, neighbour)}

	dir := filepath.Join(above, "made", "by tenant")
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	got := daclOf(t, dir)
	if !strings.Contains(got, "(A;OICI;FA;;;"+sid+")") || strings.Contains(got, ";WD)") {
		t.Errorf("the directory MkdirAll made: %s; want the entries it was born with, the user's among them, inherited from now on, and nothing of the folder above", got)
	}
	for _, entry := range strings.Split(strings.Trim(got[strings.Index(got, "("):], "()"), ")(") {
		if !strings.HasPrefix(entry, "A;OICI;") {
			t.Errorf("the directory MkdirAll made has the entry (%s), which is not passed on", entry)
		}
	}
	fresh := filepath.Join(dir, "sub", "new.txt")
	if err := os.MkdirAll(filepath.Dir(fresh), 0o700); err != nil { // the plain os calls, as most of Tenant uses
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := daclOf(t, fresh); !strings.Contains(f, "(A;ID;FA;;;"+sid+")") || strings.Contains(f, ";WD)") {
		t.Errorf("a file made below it: %s; want what the directory passes on, and nothing of the folder above", f)
	}
	// A second call, and a directory that inherits like any other: unchanged.
	if err := MkdirAll(dir); err != nil || daclOf(t, dir) != got {
		t.Errorf("a second MkdirAll: %v, %s; want %s", err, daclOf(t, dir), got)
	}
	plain := filepath.Join(t.TempDir(), "plain", "new")
	if err := MkdirAll(plain); err != nil {
		t.Fatal(err)
	}
	if p := daclOf(t, plain); !strings.Contains(p, "ID;") || strings.HasPrefix(p, "D:P") {
		t.Errorf("a directory made where permissions are inherited: %s; want only inherited entries, untouched", p)
	}
	for p, want := range before {
		if got := daclOf(t, p); got != want {
			t.Errorf("%s was changed:\n  before %s\n  after  %s", p, want, got)
		}
	}
}
