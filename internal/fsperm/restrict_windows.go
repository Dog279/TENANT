package fsperm

import "os"

// Supported reports whether POSIX owner/group/world modes are enforceable.
// Windows DACL enforcement is tracked separately by TEN-325.
func Supported() bool { return false }

// restrict is intentionally a no-op on Windows because POSIX mode bits do not
// model Windows DACLs. TEN-325 tracks applying an equivalent DACL policy.
func restrict(_ string, _ os.FileMode) error { return nil }
