//go:build darwin || linux

package osys

import (
	"errors"
	"fmt"
	"syscall"
)

// linkAppeared turns the ELOOP of an open that met a symbolic link into a
// refusal the model can act on. The path was checked with no symlinks left in
// it, so a link on it now was swapped in after the check.
func linkAppeared(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("refused: a symbolic link appeared on %s after it was checked, and the "+
			"file tools only open the exact path they checked. This is a hard boundary — it is not approvable", path)
	}
	return err
}
