//go:build !windows

package main

// Only Windows tells a program about a logoff or a shutdown through a window
// (sessionend_windows.go); elsewhere SIGTERM does it.

func watchSessionEnd() (ending <-chan struct{}, err error) { return nil, nil }

func sessionEndFinished() {}
