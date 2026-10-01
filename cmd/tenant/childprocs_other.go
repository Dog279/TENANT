//go:build !windows

package main

import "os/exec"

// killChildrenOnExit is Windows-only (TEN-401). Elsewhere the browser still
// closes on a clean shutdown; a hard kill can leave it running.
func killChildrenOnExit() {}

func breakAway(*exec.Cmd) {}
