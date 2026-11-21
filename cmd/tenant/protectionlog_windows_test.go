package main

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

// denyWrites makes dir unwritable to this user the way another program of
// the same user can (round four of the independent test): an entry that
// refuses this user adding or writing files, on a folder the user owns. The
// test takes it off again.
func denyWrites(t *testing.T, dir string) {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := u.User.Sid.String()
	set := func(sddl string) error {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	// 0x116: write data or add a file, append or add a folder, write
	// extended attributes and attributes.
	if err := set(fmt.Sprintf("D:(D;OICI;0x116;;;%s)(A;OICI;FA;;;%s)", sid, sid)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set(fmt.Sprintf("D:(A;OICI;FA;;;%s)", sid)); err != nil {
			t.Error(err)
		}
	})
}
