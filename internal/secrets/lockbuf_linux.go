package secrets

import "golang.org/x/sys/unix"

// noDump leaves the pages out of a core dump.
func noDump(mem []byte) error { return unix.Madvise(mem, unix.MADV_DONTDUMP) }
