//go:build windows

package secrets

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsCurrentUserOnlyDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token")
	if err := WriteFile(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	assertCurrentUserOnlyDACL(t, path, false)
	assertCurrentUserOnlyDACL(t, filepath.Dir(path), true)
}

func TestWindowsDPAPIRoundTrip(t *testing.T) {
	p, err := NewUserProtector()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := p.Protect([]byte("high-value-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == "high-value-token" {
		t.Fatal("DPAPI returned plaintext")
	}
	plaintext, err := p.Unprotect(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "high-value-token" {
		t.Fatalf("DPAPI round trip = %q", plaintext)
	}
}

func assertCurrentUserOnlyDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("DACL is not protected from inheritance")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		t.Fatal("DACL has no access entries")
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	var effectiveFullControl, inheritableFullControl bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d type = %d, want allow", i, ace.Header.AceType)
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !user.Equals(entrySID) {
			t.Fatalf("ACE %d grants access to %s, want only current user %s", i, entrySID, user)
		}
		fullControl := ace.Mask&windows.GENERIC_ALL != 0 || ace.Mask&windows.ACCESS_MASK(0x1f01ff) == windows.ACCESS_MASK(0x1f01ff)
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 && fullControl {
			effectiveFullControl = true
		}
		inheritFlags := uint8(windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE)
		if ace.Header.AceFlags&inheritFlags == inheritFlags && fullControl {
			inheritableFullControl = true
		}
	}
	if !effectiveFullControl {
		t.Fatal("current user lacks effective full control")
	}
	if directory && !inheritableFullControl {
		t.Fatal("directory DACL does not propagate current-user-only access")
	}
}
