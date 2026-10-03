//go:build !windows

package main

// Only Windows has a service control manager and a console window to hide
// (svc_windows.go).

func runWindowsService() bool { return false }

func hideConsoleWindow() {}
