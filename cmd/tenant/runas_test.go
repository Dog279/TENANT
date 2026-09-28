package main

import "testing"

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
}
