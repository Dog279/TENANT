package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
	"tenant/internal/model"
)

// TestMain doubles as the Tenant processes the boot-record tests start
// (TEN-365, TEN-373): the test binary re-runs itself in a helper mode.
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
	if _, err := hub.OpenStore(nil); err != nil {
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

// openBootTestHub opens the log book at root with its store, as the next
// start of serve or the TUI would.
func openBootTestHub(t *testing.T, root string) *logs.Hub {
	t.Helper()
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.OpenStore(nil); err != nil {
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

// awaitDeadBootReport runs the dead-process check until it files the dead
// helper (Windows may free a dead process's lock a moment after it exits),
// then waits for the writer to hand the event to the ring.
func awaitDeadBootReport(t *testing.T, hub *logs.Hub) logs.Entry {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_ = hub.CheckBoots(context.Background())
		if got := criticalEntries(hub); len(got) > 0 {
			_ = hub.CheckBoots(context.Background()) // and never twice
			time.Sleep(300 * time.Millisecond)       // past a batch, for a second report to show
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
			if e.Source != "system" || e.Field("event") != "system/4" || !strings.HasPrefix(e.Msg, "The previous run crashed: panic: boot helper: boom") ||
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
			if !strings.Contains(string(disk), "ERROR [system] The previous run crashed") || !strings.Contains(string(disk), "severity=critical") {
				t.Fatalf("system log:\n%s", disk)
			}
			assertEmptyDir(t, filepath.Join(root, "crash"))
			assertEmptyDir(t, filepath.Join(root, "boots"))
		})
	}
}

// A live process is never reported dead, however old its heartbeat looks (a
// laptop waking from sleep). Killed, its lock is free and its row, with no
// clean stop, reads as an unexpected end, carrying the time of its last
// event.
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
	ents, _ := os.ReadDir(filepath.Join(root, "boots"))
	if len(ents) != 1 {
		t.Fatalf("boots/ = %v, want the helper's lock", ents)
	}
	id := strings.TrimSuffix(ents[0].Name(), ".lock")

	hub := openBootTestHub(t, root)
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lastYear := time.Now().AddDate(-1, 0, 0)
	if err := st.Heartbeat(context.Background(), id, lastYear); err != nil {
		t.Fatal(err)
	}
	_ = hub.CheckBoots(context.Background())
	time.Sleep(300 * time.Millisecond)
	if got := criticalEntries(hub); len(got) != 0 {
		t.Fatalf("a live process was reported dead: %+v", got)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	e := awaitDeadBootReport(t, hub)
	last, _ := time.Parse(time.RFC3339Nano, e.Field("last"))
	if !strings.HasPrefix(e.Msg, "The previous run ended unexpectedly") || e.Field("event") != "system/3" || e.Field("severity") != "critical" ||
		e.Field("pid") != strconv.Itoa(cmd.Process.Pid) || e.Field("role") != "helper" || e.Field("stack") != "" || e.Field("boot") != id ||
		time.Since(last) > time.Minute {
		t.Fatalf("unexpected-end event = %+v", e)
	}
	assertEmptyDir(t, filepath.Join(root, "crash"))
	assertEmptyDir(t, filepath.Join(root, "boots"))
}

// An orderly stop turns the crash output off, removes its empty crash file
// and its lock, and marks its row clean, so the next start files nothing.
func TestBootRecordCleanStopIsNotReported(t *testing.T) {
	root := t.TempDir()
	if out, err := bootHelperCmd("clean", root).CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	assertEmptyDir(t, filepath.Join(root, "crash"))
	assertEmptyDir(t, filepath.Join(root, "boots"))
	hub := openBootTestHub(t, root)
	_ = hub.CheckBoots(context.Background())
	time.Sleep(300 * time.Millisecond)
	if got := criticalEntries(hub); len(got) != 0 {
		t.Fatalf("a clean stop was reported: %+v", got)
	}
}

// The log book is the last thing released: its final flush is the last
// Release step, after the boot record's clean stop, and the store stays open
// for the shutdown's own last events, so the shutdown activity's end and
// StoppedOK land in logbook.db when the deferred Close runs.
func TestBootRecordLogBookReleasedLast(t *testing.T) {
	root := t.TempDir()
	hub := openBootTestHub(t, root)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	life := lifecycle.New(nil)
	defer startBootRecord(hub, life, "tui")() // right after the group is made, as serve and the TUI do
	life.AddFunc(lifecycle.Release, "stores", func() {})
	life.AddFunc(lifecycle.Release, "tool mux", func() {})
	steps := life.Steps()
	if want := []string{"release/tool mux", "release/stores", "release/boot record", "release/log book"}; !slices.Equal(steps[len(steps)-4:], want) {
		t.Fatalf("steps = %v, want them to end %v", steps, want)
	}
	_ = life.Shutdown()
	catalog.StoppedOK.Log(context.Background(), "uptime", time.Second) // as run.stopped does
	_ = hub.Close()

	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var shutdownEnded, stopped bool
	for cur := (store.Cursor{}); ; {
		evs, next, _, err := st.Since(context.Background(), cur, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			break
		}
		for _, e := range evs {
			shutdownEnded = shutdownEnded || e.Provider == "system" && e.ID == 20 && e.Opcode == event.OpStop
			stopped = stopped || e.Provider == "system" && e.ID == catalog.StoppedOK.Spec().ID
		}
		cur = next
	}
	if !shutdownEnded || !stopped {
		t.Errorf("logbook.db lacks the shutdown's end (%v) or StoppedOK (%v)", shutdownEnded, stopped)
	}
	if open, _ := st.Unfinished(context.Background(), ""); len(open) != 0 {
		t.Errorf("the boot record was not marked clean: %+v", open)
	}
}

// The spawn classifier's guard used to swallow a panic; it now logs it with
// the stack.
func TestSpawnRoleSafelyLogsThePanic(t *testing.T) {
	hub := logs.NewMemory(10)
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	role, ok := spawnRoleSafely(context.Background(), func(context.Context, string) (model.Role, bool) { panic("classifier bug") }, "task")
	es, _ := hub.Snapshot()
	if role != "" || ok || len(es) != 1 || es[0].Field("panic") != "classifier bug" || !strings.Contains(es[0].Field("stack"), "spawnRoleSafely") {
		t.Fatalf("role=%q ok=%v entries=%+v", role, ok, es)
	}
}
