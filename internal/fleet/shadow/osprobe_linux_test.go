package shadow

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// sqliteTempFiles lists the temporary files SQLite has open. On Linux SQLite
// unlinks a temporary file as soon as it has opened it, so no directory shows
// it: the open descriptors do.
func sqliteTempFiles() (names []string, supported bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && strings.Contains(target, "etilqs_") {
			names = append(names, target)
		}
	}
	return names, true
}

// processMemory is the memory this process has resident, now and at its
// highest, in bytes.
func processMemory() (now, peak uint64, ok bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "VmRSS:":
			now = kb << 10
		case "VmHWM:":
			peak = kb << 10
		}
	}
	return now, peak, now > 0 && peak > 0
}
