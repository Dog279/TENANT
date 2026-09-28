//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Who a Windows secret path names. On a personal install that is the current
// user alone. A machine install (Tenant running as a built-in service account,
// or as an elevated administrator) is shared by two accounts: the service
// that runs Tenant and the operator who administers the machine, and a
// current-user-only DACL locks out whichever of them comes second. So on a
// machine install:
//
//   - a service account also grants BUILTIN\Administrators, so the operator
//     can still read config.json and the keys beside it;
//   - either side keeps the built-in service accounts a path already names,
//     so a tenant command run as administrator can't lock the service out of
//     its own config; and
//   - a private path may name the current user, Administrators and the
//     built-in service accounts, and no one else.
//
// Neither widens access past what it was: an elevated administrator can take
// ownership of any file anyway, and a service account is kept only where it
// was granted already.

var (
	administratorsSID = mustWellKnownSID(windows.WinBuiltinAdministratorsSid)
	// serviceSIDs are Windows' built-in service accounts.
	serviceSIDs = []*windows.SID{
		mustWellKnownSID(windows.WinLocalSystemSid),
		mustWellKnownSID(windows.WinLocalServiceSid),
		mustWellKnownSID(windows.WinNetworkServiceSid),
	}
)

// mustWellKnownSID builds a built-in SID, which can't fail for these types.
func mustWellKnownSID(t windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	sid, err := windows.CreateWellKnownSid(t)
	if err != nil {
		panic(err)
	}
	return sid
}

// identity is what protection depends on about the running process.
type identity struct {
	user    *windows.SID
	service bool // user is a built-in service account
	admin   bool // BUILTIN\Administrators is enabled in the token: elevated
}

// machine reports a machine install.
func (id identity) machine() bool { return id.service || id.admin }

func currentIdentity() (identity, error) {
	user, err := currentUserSID()
	if err != nil {
		return identity{}, err
	}
	id := identity{user: user, service: isServiceSID(user)}
	// The zero Token checks the thread's effective token, where an
	// administrator's group is enabled only when elevated.
	if id.admin, err = windows.Token(0).IsMember(administratorsSID); err != nil {
		return identity{}, err
	}
	return id, nil
}

func isServiceSID(sid *windows.SID) bool {
	for _, s := range serviceSIDs {
		if sid.Equals(s) {
			return true
		}
	}
	return false
}

// grants are the principals protectPath gives full control, for id, on a
// path whose DACL names named now.
func grants(id identity, named []*windows.SID) []*windows.SID {
	out := []*windows.SID{id.user}
	add := func(sid *windows.SID) {
		for _, have := range out {
			if have.Equals(sid) {
				return
			}
		}
		out = append(out, sid)
	}
	if !id.machine() {
		return out
	}
	if id.service {
		add(administratorsSID)
	}
	for _, sid := range named {
		if isServiceSID(sid) {
			add(sid)
		}
	}
	return out
}

// mayName reports whether a private path may grant sid access, for id.
func mayName(id identity, sid *windows.SID) bool {
	if sid.Equals(id.user) {
		return true
	}
	return id.machine() && (sid.Equals(administratorsSID) || isServiceSID(sid))
}

// ownEntry reports whether an entry for sid is id's own access: its user's,
// or Administrators' for an elevated administrator.
func ownEntry(id identity, sid *windows.SID) bool {
	return sid.Equals(id.user) || id.admin && sid.Equals(administratorsSID)
}

func (id identity) mechanism() string {
	if id.machine() {
		return "Windows DACL for this user, Administrators and service accounts"
	}
	return "Windows user-only DACL"
}

func protectPath(path string, directory bool) error {
	id, err := currentIdentity()
	if err != nil {
		return err
	}
	var named []*windows.SID
	if id.machine() {
		if named, err = namedSIDs(path); err != nil {
			return err
		}
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, sid := range grants(id, named) {
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
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

// namedSIDs lists whom path's DACL grants access now, inherited entries
// included.
func namedSIDs(path string) ([]*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || err == nil && dacl == nil {
		return nil, nil // no DACL: no entries to keep
	}
	if err != nil {
		return nil, err
	}
	var out []*windows.SID
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid, err := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Copy()
		if err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, nil
}

func probePath(path string, directory bool) (bool, string, string, error) {
	id, err := currentIdentity()
	if err != nil {
		return false, "Windows DACL", "", err
	}
	mechanism := id.mechanism()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, mechanism, "", err
	}
	if sd == nil {
		return false, mechanism, "security descriptor is missing", nil
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, mechanism, "", err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, mechanism, "DACL inherits access from its parent", nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, mechanism, "", err
	}
	if dacl == nil || dacl.AceCount == 0 {
		return false, mechanism, "DACL has no access entries", nil
	}
	allowed, private := "the current user", "a protected current-user-only DACL"
	if id.machine() {
		allowed, private = "the current user, Administrators or a service account", "a protected DACL for this user, Administrators and service accounts"
	}
	var effectiveFullControl, inheritableFullControl bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, mechanism, "", err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, mechanism, fmt.Sprintf("DACL entry %d is not an allow entry", i), nil
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !mayName(id, entrySID) {
			return false, mechanism, fmt.Sprintf("DACL entry %d grants access to %s, not %s", i, entrySID, allowed), nil
		}
		if !ownEntry(id, entrySID) {
			continue
		}
		// Windows expands GENERIC_ALL on the object itself to FILE_ALL_ACCESS,
		// while retaining a second generic, inherit-only ACE on directories.
		// Both representations mean full control for the same current user.
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
		return false, mechanism, "current user does not have effective full control", nil
	}
	kind := "file"
	if directory {
		kind = "parent directory"
		if !inheritableFullControl {
			return false, mechanism, "parent DACL does not propagate to children", nil
		}
	}
	return true, mechanism, kind + " has " + private, nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}
