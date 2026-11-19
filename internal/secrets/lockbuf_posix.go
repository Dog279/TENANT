//go:build !windows

package secrets

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func mapPages(n int) ([]byte, error) {
	mem, err := unix.Mmap(-1, 0, n, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	return mem, nil
}

func lockPages(mem []byte) (locked bool, reason string) {
	var failed []string
	lockErr := unix.Mlock(mem)
	if lockErr != nil {
		failed = append(failed, "mlock: "+lockErr.Error())
	}
	if err := noDump(mem); err != nil {
		failed = append(failed, "madvise(MADV_DONTDUMP): "+err.Error())
	}
	return lockErr == nil, strings.Join(failed, "; ")
}

func unmapPages(mem []byte, locked bool) error {
	var unlockErr error
	if locked {
		unlockErr = unix.Munlock(mem)
	}
	return errors.Join(unlockErr, unix.Munmap(mem))
}
