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
// or "" when the OS won't say. It is for showing; runAsService decides.
func runAs() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// runAsService reports whether this process runs as one of Windows' built-in
// service accounts. It goes by the SID (user.Current's Uid on Windows), which
// is the same in every language where the name isn't: a German SYSTEM is
// "NT-AUTORITÄT\SYSTEM". When the OS won't say who this is, the answer is
// yes, so D19 turns web browsing off: the safe way to be wrong.
func runAsService() bool {
	u, err := user.Current()
	if err != nil {
		return true
	}
	return isServiceAccount(u.Uid) || isServiceAccount(u.Username)
}

// isServiceAccount reports whether u is one of Windows' built-in service
// accounts, SYSTEM, LOCAL SERVICE or NETWORK SERVICE: by SID (S-1-5-18, -19,
// -20), or by English name in any letter case, with or without the
// NT AUTHORITY\ prefix.
func isServiceAccount(u string) bool {
	name := strings.ToUpper(strings.TrimSpace(u))
	name = strings.TrimPrefix(name, `NT AUTHORITY\`)
	switch name {
	case "SYSTEM", "LOCAL SERVICE", "NETWORK SERVICE", "S-1-5-18", "S-1-5-19", "S-1-5-20":
		return true
	}
	return false
}
