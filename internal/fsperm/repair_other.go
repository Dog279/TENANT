//go:build !unix

package fsperm

// POSIX modes are not enforceable here (see Supported), so the repairers
// change nothing.

func repairPath(string) (bool, error) { return false, nil }

func repairTree(string) (int, error) { return 0, nil }
