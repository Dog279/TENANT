//go:build windows

package main

// child_windows.go (TEN-531): what `tenant child install --system` needs
// from Windows: who is elevated, and the two things that make "runs as
// SYSTEM" safe to offer.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
)

// childElevated reports whether this command has an administrator's token.
// It goes through isElevated, so a test that swaps one swaps both.
var childElevated = func() bool { return isElevated() }

// childUnitFiles: a Windows child's unit is a task or a service, registered
// with their managers. There is no file to protect.
func childUnitFiles(string) []string { return nil }

// sealSystemChild makes the files of a --system child the service account's
// alone: SYSTEM, and the Administrators group so that an elevated `tenant
// child status`, pause or uninstall still works, hold full control and
// nobody else holds anything, inherited rights included.
//
// internal/secrets already writes these files owner-only, but "owner" there
// is whoever ran the command: an administrator who installs the child is
// then named on its key by their own account, which a program of theirs
// that is not elevated also runs as. Here the administrator's own name comes
// off, and when this command is elevated the files are handed to the
// Administrators group, so that not even ownership is left with the account.
func sealSystemChild(paths ...string) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	// Children before their directory: once a directory is sealed, an account
	// that is not elevated can no longer reach into it.
	var all []string
	for _, root := range paths {
		err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
			all = append(all, p)
			return err
		})
		if err != nil {
			return err
		}
	}
	slices.Reverse(all)
	for _, p := range all {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		inherit := uint32(windows.NO_INHERITANCE)
		if info.IsDir() {
			inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
		}
		var entries []windows.EXPLICIT_ACCESS
		for _, sid := range []*windows.SID{system, admins} {
			entries = append(entries, windows.EXPLICIT_ACCESS{
				AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inherit,
				Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN,
					TrusteeValue: windows.TrusteeValueFromSID(sid)},
			})
		}
		acl, err := windows.ACLFromEntries(entries, nil)
		if err != nil {
			return err
		}
		what := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
		var owner *windows.SID
		// Only a token that really is elevated may name the group as owner, so
		// this asks Windows and not childElevated, which a test may have swapped.
		if windows.GetCurrentProcessToken().IsElevated() {
			what, owner = what|windows.OWNER_SECURITY_INFORMATION, admins
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, what, owner, nil, acl, nil); err != nil {
			return fmt.Errorf("protect %s: %w", p, err)
		}
	}
	return nil
}

// childExeSafe refuses a binary in a place ordinary accounts write to: the
// service would run whatever was put there, as SYSTEM.
// ponytail: judged by where the file is (a user's profile, the temp
// folders). Read the file's and the folder's access lists instead if
// installs outside Program Files become common.
func childExeSafe(exe string) error {
	under := func(dir string) bool {
		dir = strings.TrimRight(dir, `\/`)
		return dir != "" && strings.HasPrefix(strings.ToLower(exe), strings.ToLower(dir)+`\`)
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	for _, dir := range []string{drive + `\Users`, os.Getenv("USERPROFILE"), os.Getenv("PUBLIC"), os.TempDir()} {
		if under(dir) {
			return fmt.Errorf("%s is under %s, which accounts that are not administrators write to, and a --system child runs this binary as SYSTEM at every boot; "+
				`install it under %%ProgramFiles%%\Tenant and run it from there`, exe, dir)
		}
	}
	return nil
}
