//go:build !windows

package main

// Only Windows has a service control manager (svc_windows.go).

func runWindowsService() bool { return false }
