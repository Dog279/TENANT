package main

// runas.go: who this process runs as. The dashboard's consequence lines say
// it ("Commands run as SYSTEM, which has full control of this Windows
// computer"), and serve's research turns web browsing off under a Windows
// service account with an unsandboxed browser (D19).

import (
	"os/user"
	"strings"
)

// runAs is this process's account name ("NT AUTHORITY\SYSTEM", "dylan"),
// or "" when the OS won't say.
func runAs() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// isServiceAccount reports whether u is one of Windows' built-in service
// accounts: SYSTEM, LOCAL SERVICE or NETWORK SERVICE, in any letter case,
// with or without the NT AUTHORITY\ prefix.
func isServiceAccount(u string) bool {
	name := strings.ToUpper(strings.TrimSpace(u))
	name = strings.TrimPrefix(name, `NT AUTHORITY\`)
	switch name {
	case "SYSTEM", "LOCAL SERVICE", "NETWORK SERVICE":
		return true
	}
	return false
}
