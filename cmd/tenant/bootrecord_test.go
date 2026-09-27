package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/model"
)

// TestMain doubles as the Tenant processes the boot-record tests start
// (TEN-365): the test binary re-runs itself in a helper mode.
func TestMain(m *testing.M) {
	if mode := os.Getenv("TENANT_BOOT_HELPER"); mode != "" {
		bootHelper(mode, os.Getenv("TENANT_BOOT_ROOT"))
	}
	os.Exit(m.Run())
}

// bootHelper starts up the way serve and the TUI do, then ends as mode says.
func bootHelper(mode, root string) {
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		panic(err)
	}
	life := lifecycle.New(nil)
	defer life.Shutdown()
	onPanic := startBootRecord(hub, life, "helper")
	defer onPanic()
	switch mode {
	case "panic": // on another goroutine: no deferred call runs
		go func() { panic("boot helper: boom") }()
		time.Sleep(time.Minute)
	case "panic-main": // the deferred shutdown runs before the runtime prints the crash
		panic("boot helper: boom")
	case "live": // up until killed
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "clean":
		life.Shutdown()
	}
	os.Exit(0)
}

func bootHelperCmd(mode, root string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TENANT_BOOT_HELPER="+mode, "TENANT_BOOT_ROOT="+root)
	return cmd
}

func openBootTestHub(t *testing.T, root string) *logs.Hub {
	t.Helper()
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub
}

func criticalEntries(hub *logs.Hub) []logs.Entry {
	var out []logs.Entry
	es, _ := hub.Snapshot()
	for _, e := range es {
		if e.Level == logs.LevelCritical {
			out = append(out, e)
		}
	}
	return out
}

// awaitDeadBootReport runs the start-up check until it files the dead helper:
// Windows may free a dead process's lock a moment after it exits.
func awaitDeadBootReport(t *testing.T, hub *logs.Hub) logs.Entry {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		reportDeadBoots(hub, "")
		if got := criticalEntries(hub); len(got) > 0 {
			reportDeadBoots(hub, "") // and never twice
			if got = criticalEntries(hub); len(got) != 1 {
				t.Fatalf("filed %d times: %+v", len(got), got)
			}
			return got[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead helper was never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("%s still holds %v", dir, ents)
	}
}

// A crash leaves the runtime's dump in the crash file, and the next start
// files it as one critical system event with the stacks, keeping the whole
// dump in the system folder. A panic on the goroutine that runs the deferred
// shutdown must not read as a clean stop.
func TestBootRecordFilesACrash(t *testing.T) {
	for _, mode := range []string{"panic", "panic-main"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cmd := bootHelperCmd(mode, root)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("helper did not crash:\n%s", out)
			}
			ents, _ := os.ReadDir(filepath.Join(root, "crash"))
			if len(ents) != 1 {
				t.Fatalf("crash files = %v, want one", ents)
			}
			id := strings.TrimSuffix(ents[0].Name(), ".txt")
			dump, _ := os.ReadFile(filepath.Join(root, "crash", ents[0].Name()))
			if !strings.Contains(string(dump), "boot helper: boom") || !strings.Contains(string(dump), "goroutine ") {
				t.Fatalf("crash file lacks the panic and its stacks:\n%s", dump)
			}

			hub := openBootTestHub(t, root)
			hub.Start(context.Background()) // as serve does, before it files the crash
			for _, s := range hub.Sources() {
				if s.Name == "crash" || s.Name == "boots" {
					t.Fatalf("%s/ is read as a log source", s.Name)
				}
			}
			e := awaitDeadBootReport(t, hub)
			if e.Source != "system" || !strings.HasPrefix(e.Msg, "the previous run crashed: panic: boot helper: boom") ||
				e.Field("severity") != "critical" || e.Field("pid") != strconv.Itoa(cmd.Process.Pid) || e.Field("role") != "helper" {
				t.Fatalf("crash event = %+v", e)
			}
			if !strings.Contains(e.Field("stack"), "goroutine ") {
				t.Fatalf("event lacks the stacks: %q", e.Field("stack"))
			}
			moved := filepath.Join(root, "system", "crash-"+id+".txt")
			if b, err := os.ReadFile(moved); e.Field("file") != moved || err != nil || string(b) != string(dump) {
				t.Fatalf("whole dump not kept at %s (event file=%q, err=%v)", moved, e.Field("file"), err)
			}
			// v1 files bucket critical as ERROR; the severity field keeps it apart.
			var disk []byte
			days, _ := os.ReadDir(filepath.Join(root, "system"))
			for _, d := range days {
				if strings.HasSuffix(d.Name(), ".log") {
					b, _ := os.ReadFile(filepath.Join(root, "system", d.Name()))
					disk = append(disk, b...)
				}
			}
			if !strings.Contains(string(disk), "ERROR [system] the previous run crashed") || !strings.Contains(string(disk), "severity=critical") {
				t.Fatalf("system log:\n%s", disk)
			}
			assertEmptyDir(t, filepath.Join(root, "crash"))
			assertEmptyDir(t, filepath.Join(root, "boots"))
		})
	}
}

// A live process is never reported dead, however old its record looks (a
// laptop waking from sleep). Killed, its lock is free and its record, with no
// clean stop, reads as an unexpected end.
func TestBootRecordReportsAKilledProcessButNeverALiveOne(t *testing.T) {
	root := t.TempDir()
	cmd := bootHelperCmd("live", root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not come up: %q %v", line, err)
	}
	var rec string
	ents, _ := os.ReadDir(filepath.Join(root, "boots"))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			rec = filepath.Join(root, "boots", e.Name())
		}
	}
	lastYear := time.Now().AddDate(-1, 0, 0)
	if err := os.Chtimes(rec, lastYear, lastYear); err != nil {
		t.Fatal(err)
	}

	hub := openBootTestHub(t, root)
	reportDeadBoots(hub, "")
	if got := criticalEntries(hub); len(got) != 0 {
		t.Fatalf("a live process was reported dead: %+v", got)
	}
	if _, err := os.Stat(rec); err != nil {
		t.Fatalf("a live process's record was cleared: %v", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	e := awaitDeadBootReport(t, hub)
	if e.Msg != "the previous run ended unexpectedly" || e.Field("severity") != "critical" ||
		e.Field("pid") != strconv.Itoa(cmd.Process.Pid) || e.Field("role") != "helper" || e.Field("stack") != "" {
		t.Fatalf("unexpected-end event = %+v", e)
	}
	assertEmptyDir(t, filepath.Join(root, "crash"))
	assertEmptyDir(t, filepath.Join(root, "boots"))
}

// An orderly stop turns the crash output off, removes its empty crash file
// and marks its record clean, so the next start files nothing.
func TestBootRecordCleanStopIsNotReported(t *testing.T) {
	root := t.TempDir()
	if out, err := bootHelperCmd("clean", root).CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	assertEmptyDir(t, filepath.Join(root, "crash"))
	hub := openBootTestHub(t, root)
	reportDeadBoots(hub, "")
	if got := criticalEntries(hub); len(got) != 0 {
		t.Fatalf("a clean stop was reported: %+v", got)
	}
	assertEmptyDir(t, filepath.Join(root, "boots"))
}

// The spawn classifier's guard used to swallow a panic; it now logs it with
// the stack.
func TestSpawnRoleSafelyLogsThePanic(t *testing.T) {
	hub := logs.NewMemory(10)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	role, ok := spawnRoleSafely(func(string) (model.Role, bool) { panic("classifier bug") }, "task")
	es, _ := hub.Snapshot()
	if role != "" || ok || len(es) != 1 || es[0].Field("panic") != "classifier bug" || !strings.Contains(es[0].Field("stack"), "spawnRoleSafely") {
		t.Fatalf("role=%q ok=%v entries=%+v", role, ok, es)
	}
}
