package logs

import "golang.org/x/sys/windows"

// diskSpace reports the free space available to this account on the volume
// holding path, and the volume's size.
func diskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, nil)
	return free, total, err
}
