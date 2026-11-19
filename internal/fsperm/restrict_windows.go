package fsperm

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Supported reports whether POSIX owner/group/world modes are enforceable.
// Windows DACL enforcement is tracked separately by TEN-325.
func Supported() bool { return false }

// restrict is intentionally a no-op on Windows because POSIX mode bits do not
// model Windows DACLs. TEN-325 tracks applying an equivalent DACL policy.
func restrict(_ string, _ os.FileMode) error { return nil }

// passOn makes a directory of Tenant's own pass its permissions on to the
// files and folders made in it, when it passes on none: a folder whose access
// list has no inheritable entry, which is what a folder made where nothing is
// inherited looks like, and what `icacls /inheritance:r` can leave. What is
// made in such a folder takes the default of the process that makes it, and a
// protected Tenant's default is not one for files (see the package comment).
//
// Nobody gains or loses access to the directory: its own entries are kept
// and marked to be inherited, and Windows adds them to what is in it already.
// A directory that passes anything on, one whose access list cannot be read,
// and one on a volume that keeps no access lists (FAT, exFAT) are left alone.
func passOn(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return nil
	}
	const inherited = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	aces := make([]*windows.ACCESS_ALLOWED_ACE, dacl.AceCount)
	for i := range aces {
		if err := windows.GetAce(dacl, uint32(i), &aces[i]); err != nil || aces[i].Header.AceFlags&inherited != 0 {
			return nil
		}
	}
	if !keepsAccessLists(path) {
		return nil
	}
	for _, ace := range aces { // in sd's own memory, which dacl points into
		ace.Header.AceFlags = inherited
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("%s passes no permissions on to the files made in it, and that could not be changed: %w", path, err)
	}
	return nil
}

// keepsAccessLists: the volume path is on stores an access list per file.
func keepsAccessLists(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	volume := make([]uint16, windows.MAX_PATH+1)
	var flags uint32
	return windows.GetVolumePathName(p, &volume[0], uint32(len(volume))) == nil &&
		windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, &flags, nil, 0) == nil &&
		flags&windows.FILE_PERSISTENT_ACLS != 0
}
