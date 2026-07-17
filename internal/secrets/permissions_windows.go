//go:build windows

package secrets

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func protectPath(path string, directory bool) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
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

func probePath(path string, directory bool) (bool, string, string, error) {
	user, err := currentUserSID()
	if err != nil {
		return false, "Windows user-only DACL", "", err
	}
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, "Windows user-only DACL", "", err
	}
	if sd == nil {
		return false, "Windows user-only DACL", "security descriptor is missing", nil
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, "Windows user-only DACL", "", err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, "Windows user-only DACL", "DACL inherits access from its parent", nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, "Windows user-only DACL", "", err
	}
	if dacl == nil || dacl.AceCount == 0 {
		return false, "Windows user-only DACL", "DACL has no access entries", nil
	}
	var effectiveFullControl, inheritableFullControl bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, "Windows user-only DACL", "", err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, "Windows user-only DACL", fmt.Sprintf("DACL entry %d is not an allow entry", i), nil
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !user.Equals(entrySID) {
			return false, "Windows user-only DACL", fmt.Sprintf("DACL entry %d grants access to %s, not the current user", i, entrySID), nil
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
		return false, "Windows user-only DACL", "current user does not have effective full control", nil
	}
	kind := "file"
	if directory {
		kind = "parent directory"
		if !inheritableFullControl {
			return false, "Windows user-only DACL", "parent DACL does not propagate to children", nil
		}
	}
	return true, "Windows user-only DACL", kind + " has a protected current-user-only DACL", nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}
