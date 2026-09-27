//go:build !windows

package logs

import "golang.org/x/sys/unix"

// diskSpace reports the free space an unprivileged writer may use on the
// volume holding path, and the volume's size.
func diskSpace(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
