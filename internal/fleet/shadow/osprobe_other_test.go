//go:build !windows && !linux

package shadow

// On macOS and the rest there is no way for a process to list the files it
// has open and unlinked, or to read its own memory use, without cgo. The
// tests that need these say so and skip that part.

func sqliteTempFiles() (names []string, supported bool) { return nil, false }

func processMemory() (now, peak uint64, ok bool) { return 0, 0, false }
