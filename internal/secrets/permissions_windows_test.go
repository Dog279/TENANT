//go:build windows

package secrets

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token")
	if err := WriteFile(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	assertPrivateDACL(t, path, false)
	assertPrivateDACL(t, filepath.Dir(path), true)
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

// Either scope's blob opens through Unprotect, so a key the service sealed to
// the machine and one sealed to a user before are both readable.
func TestWindowsDPAPIBothScopesRoundTrip(t *testing.T) {
	for _, p := range []dpapiProtector{{machine: false}, {machine: true}} {
		ciphertext, err := p.Protect([]byte("key"))
		if err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
		plaintext, err := (dpapiProtector{machine: !p.machine}).Unprotect(ciphertext)
		if err != nil || string(plaintext) != "key" {
			t.Fatalf("%s: round trip = %q, %v", p.Name(), plaintext, err)
		}
	}
}

func TestNewUserProtectorSealsToMachineOnMachineInstall(t *testing.T) {
	id, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewUserProtector()
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(dpapiProtector).machine; got != id.machine() {
		t.Fatalf("machine scope = %v, want %v for %+v", got, id.machine(), id)
	}
}

// The rules for who a private path names, by install. They're pure, so each
// kind of install is checked here whichever account runs the test.
func TestGrantsMayNameAndOwnEntryByInstall(t *testing.T) {
	someone := sidFromString(t, "S-1-5-21-1-2-3-1001")
	other := sidFromString(t, "S-1-5-21-1-2-3-1002")
	users := mustWellKnownSID(windows.WinBuiltinUsersSid)
	admins := administratorsSID
	system := mustWellKnownSID(windows.WinLocalSystemSid)
	localService := mustWellKnownSID(windows.WinLocalServiceSid)
	networkService := mustWellKnownSID(windows.WinNetworkServiceSid)
	named := []*windows.SID{system, users, admins, localService, someone, networkService, other}

	personal := identity{user: someone}
	service := identity{user: localService, service: true}
	operator := identity{user: someone, admin: true}

	for _, tc := range []struct {
		name string
		id   identity
		want []*windows.SID
	}{
		{"personal install: the user alone", personal, []*windows.SID{someone}},
		{"service: itself, Administrators, and the service accounts named", service, []*windows.SID{localService, admins, system, networkService}},
		{"elevated admin: itself and the service accounts named", operator, []*windows.SID{someone, system, localService, networkService}},
	} {
		got := grants(tc.id, named)
		if !sameSIDs(got, tc.want) {
			t.Errorf("%s: grants = %v, want %v", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name    string
		id      identity
		sid     *windows.SID
		mayName bool
		own     bool
	}{
		{"personal: own user", personal, someone, true, true},
		{"personal: Administrators", personal, admins, false, false},
		{"personal: SYSTEM", personal, system, false, false},
		{"personal: LocalService", personal, localService, false, false},
		{"service: itself", service, localService, true, true},
		{"service: Administrators", service, admins, true, false},
		{"service: SYSTEM", service, system, true, false},
		{"service: Users", service, users, false, false},
		{"service: another user", service, other, false, false},
		{"admin: own user", operator, someone, true, true},
		{"admin: Administrators", operator, admins, true, true},
		{"admin: LocalService", operator, localService, true, false},
		{"admin: NetworkService", operator, networkService, true, false},
		{"admin: Users", operator, users, false, false},
		{"admin: another user", operator, other, false, false},
	} {
		if got := mayName(tc.id, tc.sid); got != tc.mayName {
			t.Errorf("%s: mayName = %v, want %v", tc.name, got, tc.mayName)
		}
		if got := ownEntry(tc.id, tc.sid); got != tc.own {
			t.Errorf("%s: ownEntry = %v, want %v", tc.name, got, tc.own)
		}
	}
}

// An operator running tenant as administrator on a service install re-protects
// the service's config directory without locking the service out: the service
// account the operator granted survives, the stray Users entry doesn't, and a
// secret the operator writes there stays readable by the service.
func TestWindowsElevatedAdminKeepsServiceAccounts(t *testing.T) {
	id := requireElevatedAdmin(t)
	localService := mustWellKnownSID(windows.WinLocalServiceSid)
	users := mustWellKnownSID(windows.WinBuiltinUsersSid)
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, dir, true, id.user, localService, users)

	path := filepath.Join(dir, "logbook.key")
	if err := WriteFile(path, []byte("key")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, path} {
		named := daclSIDs(t, p)
		if !hasSID(named, localService) {
			t.Errorf("%s dropped LocalService: %v", p, named)
		}
		if hasSID(named, users) {
			t.Errorf("%s still grants Users: %v", p, named)
		}
	}
	if got, err := ReadFile(path); err != nil || string(got) != "key" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
}

// A secret a service protected (itself and Administrators) reads in place for
// an elevated administrator: nothing is rewritten, so the service keeps
// exactly the access it gave itself.
func TestWindowsElevatedAdminReadsServiceSecretInPlace(t *testing.T) {
	requireElevatedAdmin(t)
	localService := mustWellKnownSID(windows.WinLocalServiceSid)
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, dir, true, localService, administratorsSID)
	path := filepath.Join(dir, "dashboard-auth-token")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	setDACL(t, path, false, localService, administratorsSID)

	if got, err := ReadFile(path); err != nil || string(got) != "token" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	want := []*windows.SID{localService, administratorsSID}
	for _, p := range []string{dir, path} {
		if got := daclSIDs(t, p); !sameSIDs(got, want) {
			t.Errorf("%s was rewritten: names %v, want %v", p, got, want)
		}
	}
	if report := Probe(path); !report.Protected {
		t.Fatalf("service-protected secret doesn't probe private: %+v", report)
	}
}

func requireElevatedAdmin(t *testing.T) identity {
	t.Helper()
	id, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !id.admin || id.service {
		t.Skip("needs an elevated administrator that is not a service account")
	}
	return id
}

// setDACL gives each SID full control of path, and no one else.
func setDACL(t *testing.T, path string, directory bool, sids ...*windows.SID) {
	t.Helper()
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// daclSIDs lists whom path's DACL names, once each.
func daclSIDs(t *testing.T, path string) []*windows.SID {
	t.Helper()
	named, err := namedSIDs(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []*windows.SID
	for _, sid := range named {
		if !hasSID(out, sid) {
			out = append(out, sid)
		}
	}
	return out
}

func hasSID(sids []*windows.SID, want *windows.SID) bool {
	for _, sid := range sids {
		if sid.Equals(want) {
			return true
		}
	}
	return false
}

// sameSIDs compares as sets.
func sameSIDs(a, b []*windows.SID) bool {
	if len(a) != len(b) {
		return false
	}
	for _, sid := range a {
		if !hasSID(b, sid) {
			return false
		}
	}
	return true
}

func sidFromString(t *testing.T, s string) *windows.SID {
	t.Helper()
	sid, err := windows.StringToSid(s)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// assertPrivateDACL checks path's DACL against the rule for the account the
// test runs as, spelled out here rather than taken from mayName: protected
// from inheritance, full control for the current user (an elevated
// administrator may hold it through Administrators), and no one else named but
// the current user, or on a machine install also Administrators and the
// built-in service accounts.
func assertPrivateDACL(t *testing.T, path string, directory bool) {
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
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	elevated, err := windows.Token(0).IsMember(admins)
	if err != nil {
		t.Fatal(err)
	}
	service := user.IsWellKnown(windows.WinLocalSystemSid) || user.IsWellKnown(windows.WinLocalServiceSid) || user.IsWellKnown(windows.WinNetworkServiceSid)
	machine := elevated || service
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
		isAdmins := entrySID.Equals(admins)
		isService := entrySID.IsWellKnown(windows.WinLocalSystemSid) || entrySID.IsWellKnown(windows.WinLocalServiceSid) || entrySID.IsWellKnown(windows.WinNetworkServiceSid)
		switch {
		case user.Equals(entrySID):
		case machine && (isAdmins || isService):
		default:
			t.Fatalf("ACE %d grants access to %s (machine install: %v), want only the current user %s", i, entrySID, machine, user)
		}
		if !user.Equals(entrySID) && !(elevated && isAdmins) {
			continue
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
		t.Fatal("directory DACL does not propagate the current user's access")
	}
}
