package fleet

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Every file the fleet writes that is worth stealing is readable by its
// owner alone: here, a DACL that inherits nothing and names only the account
// that runs Tenant (and, on a machine install, Administrators and the
// built-in service accounts, which is the secrets package's rule).
func TestFleetFilesAreOwnerOnly(t *testing.T) {
	for _, path := range fleetFiles(t) {
		assertOwnerOnly(t, path)
		assertOwnerOnly(t, filepath.Dir(path))
	}
}

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s inherits access from its folder", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("%s has no access entries", path)
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	user := tokenUser.User.Sid
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	elevated, err := windows.Token(0).IsMember(admins)
	if err != nil {
		t.Fatal(err)
	}
	builtin := func(sid *windows.SID) bool {
		return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinLocalServiceSid) || sid.IsWellKnown(windows.WinNetworkServiceSid)
	}
	machine := elevated || builtin(user)
	owner := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("%s: entry %d is not an allow entry", path, i)
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user):
			owner = true
		case machine && (sid.Equals(admins) || builtin(sid)):
		default:
			t.Errorf("%s grants access to %s, who is not the account running Tenant (%s)", path, sid, user)
		}
	}
	if !owner && !machine {
		t.Errorf("%s does not grant its owner access", path)
	}
}
