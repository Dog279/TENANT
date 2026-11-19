//go:build !windows && !linux

package secrets

// noDump does nothing here: only Linux has MADV_DONTDUMP.
func noDump([]byte) error { return nil }
