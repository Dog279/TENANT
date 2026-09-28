//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// makeUnreadable leaves path's DACL naming only Guests, so this process can't
// open it, and hands it back to the current user when the test ends.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	guests, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	if err != nil {
		t.Fatal(err)
	}
	setOnlyDACL(t, path, guests, windows.GENERIC_READ)
	t.Cleanup(func() {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			t.Error(err)
			return
		}
		setOnlyDACL(t, path, user.User.Sid, windows.GENERIC_ALL)
	})
}

// setOnlyDACL gives sid access to path, and no one else.
func setOnlyDACL(t *testing.T, path string, sid *windows.SID, access windows.ACCESS_MASK) {
	t.Helper()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: access,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}
