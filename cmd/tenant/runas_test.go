package main

import (
	"runtime"
	"testing"
)

func TestIsServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		u    string
		want bool
	}{
		{`NT AUTHORITY\SYSTEM`, true},
		{"SYSTEM", true},
		{"system", true},
		{`nt authority\local service`, true},
		{`NT AUTHORITY\NETWORK SERVICE`, true},
		{" NETWORK SERVICE ", true},
		{"S-1-5-18", true}, // SIDs: the same in every language
		{"s-1-5-19", true},
		{"S-1-5-20", true},
		{"S-1-5-18-1", false},
		{"S-1-5-21-1004336348-1177238915-682003330-1001", false},
		{"501", false},
		{"dylan", false},
		{`DESKTOP-1\dylan`, false},
		{`NT AUTHORITY\ANONYMOUS LOGON`, false},
		{"systemd", false},
		{"", false},
	} {
		if got := isServiceAccount(tc.u); got != tc.want {
			t.Errorf("isServiceAccount(%q) = %v, want %v", tc.u, got, tc.want)
		}
	}
}

func TestRunAs_NamesThisAccount(t *testing.T) {
	if runAs() == "" {
		t.Error("runAs() is empty: the OS should name the account running the tests")
	}
	if runtime.GOOS != "windows" && runAsService() {
		t.Error("runAsService() calls the account running the tests a Windows service account")
	}
}
