//go:build procprotecttest

package main

// A build made with -tags procprotecttest exists for the acceptance tests of
// the acknowledgement flow (protectiongate_process_test.go, TEN-535) and is
// never released: it lets the environment say that a control cannot be
// applied, which in a released binary would be the run-time off switch the
// plan forbids. `tenant version` says what it is.
//
//	TENANT_TEST_FORCE_FAIL  control=reason[;control=reason]: Protect leaves
//	                        each one out and reports it as not in force; the
//	                        helper serve starts on Windows is told the same
//	TENANT_TEST_VERSION     the version this process says it is
//	TENANT_TEST_SYSTEM_LOG  a file that stands in for the system's log
//	TENANT_TEST_TERMINAL    1: stdin and stderr count as a terminal
//	TENANT_TEST_REPORT      a file Protect's report is written to, as JSON

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"

	"tenant/internal/procprotect"
	"tenant/internal/version"
)

func init() { // before main, so before Protect
	testBuildNote = "TEST BUILD (-tags procprotecttest): the environment can switch a process protection off. Never install it."
	if v := os.Getenv("TENANT_TEST_FORCE_FAIL"); v != "" {
		sessionHelperEnv = []string{"TENANT_TEST_FORCE_FAIL=" + v}
	}
	for _, kv := range strings.Split(os.Getenv("TENANT_TEST_FORCE_FAIL"), ";") {
		if id, reason, ok := strings.Cut(kv, "="); ok {
			procprotect.ForceFail(id, reason)
		}
	}
	if v := os.Getenv("TENANT_TEST_VERSION"); v != "" {
		version.Version = v
	}
	if path := os.Getenv("TENANT_TEST_SYSTEM_LOG"); path != "" {
		systemLog = func(msg string) error {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString(msg + "\n")
			return err
		}
	}
	if os.Getenv("TENANT_TEST_TERMINAL") == "1" {
		ackConsole = func() (*bufio.Reader, io.Writer, bool) { return bufio.NewReader(os.Stdin), os.Stderr, true }
		stderrAtTerminal = func() bool { return true }
	}
	if path := os.Getenv("TENANT_TEST_REPORT"); path != "" {
		afterProtect = func(r procprotect.Report) {
			b, _ := json.Marshal(r)
			_ = os.WriteFile(path, b, 0o600)
		}
	}
}
