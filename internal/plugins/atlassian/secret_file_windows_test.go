//go:build windows

package atlassian

import (
	"testing"

	"golang.org/x/sys/windows"
)

func assertSecretFileSecurity(t *testing.T, path string) {
	t.Helper()
	// Go's 0600 mode is advisory on Windows. Verify the native security
	// descriptor instead; owner-only DACL enforcement is a separate contract.
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read Windows DACL for %s: %v", path, err)
	}
	if sd == nil {
		t.Fatalf("%s has no Windows security descriptor", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("read Windows DACL for %s: %v", path, err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s has no Windows access-control entries", path)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 {
		t.Fatalf("%s has no present Windows DACL (control=%#x, err=%v)", path, control, err)
	}
}
