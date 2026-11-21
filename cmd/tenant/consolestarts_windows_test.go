//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"tenant/internal/procprotect"
)

// Console starts (TEN-535 round three). Process protection makes Tenant
// leave its console and join it again at every start, and round two of the
// independent test lost the keyboard in about one start in four of `tenant
// chat` alone on a pseudo console: the rejoin closed a stream it had just
// opened. This starts the real binary, `chat` and the terminal UI, in every
// shape a console start has, types a line at the console, and wants the
// answer on it and a clean exit:
//
//	alone on an ordinary console (created hidden);
//	alone on a pseudo console (no window; the host Windows Terminal uses),
//	started the way Windows Terminal starts a program;
//	from cmd.exe on each, where cmd keeps the console while Tenant is away;
//	on a console of its own with all three streams redirected to pipes.
//
// One start of each in an ordinary test run. TENANT_CONSOLE_STARTS=200 is
// the soak: that many consecutive starts of each, any failure fails it.
// Nothing here shows a window.

const envConsoleDriver = "TENANT_CONSOLE_DRIVER" // this test binary, run as the program that types at a victim's console

func consoleDriverIfAsked() {
	if pid, err := strconv.Atoi(os.Getenv(envConsoleDriver)); err == nil {
		consoleDriver(uint32(pid))
	}
}

// testConsole is the console a start runs on, as the test reaches it.
type testConsole interface {
	typeText(s string) error // keys typed at the console
	text() string            // what is, or has been, on it
	close()
}

var (
	conKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ansi        = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\\\^_=>]")
)

// squeezed is s without escape sequences and without white space: what a
// console shows, however its host chose to draw the gaps.
func squeezed(s string) string {
	return strings.Join(strings.Fields(ansi.ReplaceAllString(s, " ")), "")
}

// ---- a pseudo console ----

type ptyConsole struct {
	hpc     windows.Handle
	in, out windows.Handle // this end
	mu      sync.Mutex
	seen    strings.Builder
	done    chan struct{}
}

func newPtyConsole() (*ptyConsole, error) {
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		return nil, err
	}
	p := &ptyConsole{in: inW, out: outR, done: make(chan struct{})}
	err := windows.CreatePseudoConsole(windows.Coord{X: 120, Y: 40}, inR, outW, 0, &p.hpc)
	windows.CloseHandle(inR) // the pseudo console has its own
	windows.CloseHandle(outW)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(p.done)
		buf := make([]byte, 16384)
		for {
			var n uint32
			if err := windows.ReadFile(p.out, buf, &n, nil); err != nil || n == 0 {
				return
			}
			chunk := string(buf[:n])
			p.mu.Lock()
			p.seen.WriteString(chunk)
			p.mu.Unlock()
			// What a terminal answers when its host asks what it is and
			// where the cursor is.
			var w uint32
			if strings.Contains(chunk, "\x1b[c") {
				_ = windows.WriteFile(p.in, []byte("\x1b[?61;4;6;7;14;21;22;23;24;28;32;42c"), &w, nil)
			}
			if strings.Contains(chunk, "\x1b[6n") {
				_ = windows.WriteFile(p.in, []byte("\x1b[1;1R"), &w, nil)
			}
		}
	}()
	return p, nil
}

func (p *ptyConsole) typeText(s string) error {
	var n uint32
	return windows.WriteFile(p.in, []byte(s), &n, nil)
}

func (p *ptyConsole) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen.String()
}

func (p *ptyConsole) close() {
	windows.ClosePseudoConsole(p.hpc) // ends what is still on it
	windows.CloseHandle(p.in)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	windows.CloseHandle(p.out)
}

// ---- an ordinary console, hidden, reached through a second process ----

// hiddenConsole talks to this test binary run as consoleDriver, which joins
// the victim's console: a process is on one console at a time, and the test
// keeps its own.
type hiddenConsole struct {
	cmd  *exec.Cmd
	say  *bufio.Writer
	hear *bufio.Reader
	mu   sync.Mutex
}

func joinHiddenConsole(pid uint32) (*hiddenConsole, error) {
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), envConsoleDriver+"="+strconv.Itoa(int(pid)))
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	h := &hiddenConsole{cmd: c, say: bufio.NewWriter(in), hear: bufio.NewReader(out)}
	if line, err := h.hear.ReadString('\n'); err != nil || strings.TrimSpace(line) != "joined" {
		h.close()
		return nil, fmt.Errorf("the console driver did not join the console of %d: %q %v", pid, line, err)
	}
	return h, nil
}

func (h *hiddenConsole) ask(line string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.say.WriteString(line + "\n"); err != nil {
		return "", err
	}
	if err := h.say.Flush(); err != nil {
		return "", err
	}
	reply, err := h.hear.ReadString('\n')
	return strings.TrimSpace(reply), err
}

func (h *hiddenConsole) typeText(s string) error {
	reply, err := h.ask("type " + base64.StdEncoding.EncodeToString([]byte(s)))
	if err == nil && reply != "typed" {
		err = errors.New(reply)
	}
	return err
}

func (h *hiddenConsole) text() string {
	reply, err := h.ask("screen")
	if err != nil {
		return ""
	}
	b, _ := base64.StdEncoding.DecodeString(reply)
	return string(b)
}

func (h *hiddenConsole) close() {
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
}

// consoleDriver is this test binary as the program at a victim's console:
// it leaves its own (which has no window), joins the victim's, and types and
// reads the screen as it is told on its standard input. It starts nothing.
func consoleDriver(pid uint32) {
	_, _, _ = conKernel32.NewProc("FreeConsole").Call()
	joined := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r, _, _ := conKernel32.NewProc("AttachConsole").Call(uintptr(pid)); r != 0 {
			joined = true
			break
		}
	}
	if !joined {
		fmt.Println("no console to join")
		os.Exit(1)
	}
	fmt.Println("joined")
	type keyRecord struct { // INPUT_RECORD holding a KEY_EVENT_RECORD
		EventType uint16
		_         uint16
		KeyDown   int32
		Repeat    uint16
		VKey      uint16
		Scan      uint16
		Char      uint16
		Control   uint32
	}
	open := func(name string) (windows.Handle, error) {
		return windows.CreateFile(windows.StringToUTF16Ptr(name), windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	}
	for sc := bufio.NewScanner(os.Stdin); sc.Scan(); {
		verb, arg, _ := strings.Cut(sc.Text(), " ")
		switch verb {
		case "type":
			b, _ := base64.StdEncoding.DecodeString(arg)
			var keys []keyRecord
			for _, r := range string(b) {
				vk, ctl := uint16(0), uint32(0)
				switch r {
				case '\r':
					vk = 0x0D
				case 0x1a:
					vk, ctl = 'Z', 0x0008 // LEFT_CTRL_PRESSED
				}
				keys = append(keys, keyRecord{EventType: 1, KeyDown: 1, Repeat: 1, VKey: vk, Char: uint16(r), Control: ctl},
					keyRecord{EventType: 1, KeyDown: 0, Repeat: 1, VKey: vk, Char: uint16(r), Control: ctl})
			}
			in, err := open("CONIN$")
			if err == nil {
				var n uint32
				if r, _, e := conKernel32.NewProc("WriteConsoleInputW").Call(uintptr(in), uintptr(unsafe.Pointer(&keys[0])), uintptr(len(keys)), uintptr(unsafe.Pointer(&n))); r == 0 {
					err = e
				}
				windows.CloseHandle(in)
			}
			if err != nil {
				fmt.Println("not typed: " + strings.ReplaceAll(err.Error(), "\n", " "))
			} else {
				fmt.Println("typed")
			}
		case "screen":
			var text strings.Builder
			if out, err := open("CONOUT$"); err == nil {
				var info windows.ConsoleScreenBufferInfo
				if windows.GetConsoleScreenBufferInfo(out, &info) == nil {
					row := make([]uint16, info.Size.X)
					for y := int16(0); y < info.Size.Y; y++ {
						var n uint32
						_, _, _ = conKernel32.NewProc("ReadConsoleOutputCharacterW").Call(uintptr(out), uintptr(unsafe.Pointer(&row[0])),
							uintptr(len(row)), uintptr(uint32(uint16(y))<<16), uintptr(unsafe.Pointer(&n)))
						text.WriteString(strings.TrimRight(windows.UTF16ToString(row[:n]), " ") + "\n")
					}
				}
				windows.CloseHandle(out)
			}
			fmt.Println(base64.StdEncoding.EncodeToString([]byte(text.String())))
		case "break": // Ctrl-Break at the console, for every program on it but this one
			_, _, _ = conKernel32.NewProc("SetConsoleCtrlHandler").Call(windows.NewCallback(func(uint32) uintptr { return 1 }), 1)
			if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0); err != nil {
				fmt.Println("not sent: " + err.Error())
			} else {
				fmt.Println("sent")
			}
		case "clients": // the processes on the console, this one among them
			buf := make([]uint32, 64)
			n, _, _ := conKernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			fmt.Println(strings.Trim(fmt.Sprint(buf[:min(int(n), len(buf))]), "[]"))
		}
	}
	os.Exit(0)
}

// ---- one start ----

type consoleShape struct {
	name  string
	pty   bool // a pseudo console; otherwise an ordinary console, created hidden
	cmd   bool // started by cmd.exe on that console; otherwise alone on it
	pipes bool // alone on a hidden console, with all three streams redirected to pipes
}

var consoleShapes = []consoleShape{
	{name: "alone on an ordinary console"},
	{name: "alone on a pseudo console", pty: true},
	{name: "from cmd.exe on an ordinary console", cmd: true},
	{name: "from cmd.exe on a pseudo console", pty: true, cmd: true},
	{name: "with its streams redirected to pipes", pipes: true},
}

// consoleStart is one program started on a console of the test's making.
type consoleStart struct {
	process windows.Handle
	pid     uint32
	console testConsole
}

func (s *consoleStart) end() {
	_ = windows.TerminateProcess(s.process, 1)
	_, _ = windows.WaitForSingleObject(s.process, 10_000)
	windows.CloseHandle(s.process)
	if s.console != nil {
		s.console.close()
	}
}

// exited waits for the program and returns its exit code.
func (s *consoleStart) exited(d time.Duration) (uint32, error) {
	if ev, _ := windows.WaitForSingleObject(s.process, uint32(d/time.Millisecond)); ev != windows.WAIT_OBJECT_0 {
		return 0, errors.New("it is still running")
	}
	var code uint32
	err := windows.GetExitCodeProcess(s.process, &code)
	return code, err
}

func startOnConsole(sh consoleShape, cmd *exec.Cmd) (*consoleStart, error) {
	args := cmd.Args
	if sh.cmd {
		args = append([]string{filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/d", "/c"}, cmd.Args...)
	}
	cmdline := windows.StringToUTF16Ptr(windows.ComposeCommandLine(args))
	var block []uint16
	for _, kv := range cmd.Env {
		block = append(block, windows.StringToUTF16(kv)...)
	}
	block = append(block, 0)
	var pi windows.ProcessInformation
	s := &consoleStart{}
	if sh.pty {
		p, err := newPtyConsole()
		if err != nil {
			return nil, err
		}
		attrs, err := windows.NewProcThreadAttributeList(1)
		if err == nil {
			// The attribute's value is the pseudo console's handle itself, passed where a pointer goes.
			err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&p.hpc)), unsafe.Sizeof(p.hpc))
		}
		if err != nil {
			p.close()
			return nil, err
		}
		defer attrs.Delete()
		var si windows.StartupInfoEx
		si.Cb = uint32(unsafe.Sizeof(si))
		si.ProcThreadAttributeList = attrs.List()
		si.Flags = windows.STARTF_USESTDHANDLES // none named: as Windows Terminal starts a program
		if err := windows.CreateProcess(nil, cmdline, nil, nil, false,
			windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, &block[0], nil, &si.StartupInfo, &pi); err != nil {
			p.close()
			return nil, err
		}
		s.console = p
	} else {
		si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Flags: windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE}
		if err := windows.CreateProcess(nil, cmdline, nil, nil, false,
			windows.CREATE_NEW_CONSOLE|windows.CREATE_UNICODE_ENVIRONMENT, &block[0], nil, &si, &pi); err != nil {
			return nil, err
		}
	}
	windows.CloseHandle(pi.Thread)
	s.process, s.pid = pi.Process, pi.ProcessId
	if !sh.pty {
		h, err := joinHiddenConsole(pi.ProcessId)
		if err != nil {
			s.end()
			return nil, err
		}
		s.console = h
	}
	return s, nil
}

// waitForText waits until the console has shown want.
func waitForText(c testConsole, want string, d time.Duration) error {
	want = squeezed(want)
	for deadline := time.Now().Add(d); ; time.Sleep(25 * time.Millisecond) {
		got := squeezed(c.text())
		if strings.Contains(got, want) {
			return nil
		}
		if time.Now().After(deadline) {
			if len(got) > 600 {
				got = "…" + got[len(got)-600:]
			}
			return fmt.Errorf("%q did not appear on the console in %v; it shows: %s", want, d, got)
		}
	}
}

// cleanStart is one start of verb ("chat" or "tui") in shape: a line typed
// at the console is answered on it, and the program ends cleanly when told.
func cleanStart(u *tenantUnderTest, sh consoleShape, verb string, n int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report := filepath.Join(u.home, "report.json")
	_ = os.Remove(report)
	cmd := u.command(ctx, []string{"TENANT_TEST_REPORT=" + report}, verb, "--backend", "echo", "--data", u.data, "--config", u.cfg)
	// Every start leaves its console and joins it again: the control is
	// applied in every shape counted here, and a start where it is not is
	// not a clean one.
	rejoined := func(err error) error {
		var r procprotect.Report
		b, rerr := os.ReadFile(report)
		if rerr == nil {
			rerr = json.Unmarshal(b, &r)
		}
		for _, c := range r.Controls {
			if c.ID == "windows.console-host" && (!c.OK || c.Note != "the console was left and joined again") {
				rerr = fmt.Errorf("the console host's control: ok=%v, %s%s", c.OK, c.Reason, c.Note)
			}
		}
		if rerr != nil && err != nil {
			return fmt.Errorf("%w (%v)", err, rerr)
		}
		if rerr != nil {
			return rerr
		}
		return err
	}
	// The echo backend's answer begins the same way whatever it remembers
	// of earlier starts, and nothing else on the console says it.
	line, answer := fmt.Sprintf("start %d of %s", n, verb), "echo[dev backend]"
	prompt, bye := "you>", "\x1a\r" // Ctrl-Z: the end of the input
	if verb == "tui" {
		prompt, bye = "/help", "/exit\r"
	}
	bad := []string{"handle is invalid", "error making raw", "parameter is incorrect", "panic:"}

	if sh.pipes {
		// A console of its own, hidden, and nothing of it used: the input is
		// a pipe, and so are the outputs. Only chat runs so (tuiNeedsConsole).
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_CONSOLE}
		cmd.Stdin = strings.NewReader(line + "\n")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(squeezed(string(out)), squeezed(answer)) {
			return rejoined(fmt.Errorf("exit %v, output:\n%s", err, out))
		}
		return rejoined(nil)
	}
	s, err := startOnConsole(sh, cmd)
	if err != nil {
		return err
	}
	defer s.end()
	fail := func(err error) error {
		err = rejoined(err)
		if h, ok := s.console.(*hiddenConsole); ok && err != nil && !sh.cmd {
			// Whether the program is on its console at all: one that left it
			// for good (console_windows.go) shows nothing and reads nothing.
			if on, _ := h.ask("clients"); !slices.Contains(strings.Fields(on), strconv.Itoa(int(s.pid))) {
				err = fmt.Errorf("%w (it is not on its console, whose programs are %q)", err, on)
			}
		}
		for _, b := range bad {
			if strings.Contains(s.console.text(), b) {
				return fmt.Errorf("%w (the console says %q)", err, b)
			}
		}
		return err
	}
	if err := waitForText(s.console, prompt, 60*time.Second); err != nil {
		return fail(fmt.Errorf("no prompt: %w", err))
	}
	if err := s.console.typeText(line + "\r"); err != nil {
		return fail(fmt.Errorf("typing: %w", err))
	}
	if err := waitForText(s.console, answer, 60*time.Second); err != nil {
		return fail(fmt.Errorf("the typed line was not answered: %w", err))
	}
	if err := s.console.typeText(bye); err != nil {
		return fail(fmt.Errorf("typing the goodbye: %w", err))
	}
	code, err := s.exited(30 * time.Second)
	if err != nil || code != 0 {
		return fail(fmt.Errorf("after the goodbye: exit code %d, %v", code, err))
	}
	return fail(nil)
}

// A Ctrl-Break at serve's console stops serve, cleanly and quietly: the
// helper that owns serve's window shares the console, takes no notice, and
// goes when serve has gone, so the stop logs nothing about it (TEN-535
// round three: it used to end with serve's console, and serve logged it as
// unavailable during a clean stop).
func TestCtrlBreakStopsServeQuietly(t *testing.T) {
	forcedControls(t) // skips where there are no controls
	bin := protectTestBinary(t)
	removeTestExclusion(t, bin)
	u := newTenantUnderTest(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := startOnConsole(consoleShapes[0], u.command(ctx, nil, u.startArgs("serve")...))
	if err != nil {
		t.Fatal(err)
	}
	defer s.end()
	if err := waitForText(s.console, "Ctrl-C / SIGTERM to stop", time.Minute); err != nil {
		t.Fatal(err)
	}
	if reply, err := s.console.(*hiddenConsole).ask("break"); err != nil || reply != "sent" {
		t.Fatalf("Ctrl-Break: %q %v", reply, err)
	}
	if code, err := s.exited(time.Minute); err != nil || code != 0 {
		t.Fatalf("serve after Ctrl-Break: exit code %d, %v", code, err)
	}
	const helperGone = "the helper that owns serve's window"
	_ = filepath.WalkDir(filepath.Join(u.data, "logs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), helperGone) {
				t.Errorf("%s says %q after a clean stop", p, helperGone)
			}
		}
		return nil
	})
}

func TestConsoleStartsAreClean(t *testing.T) {
	forcedControls(t) // skips where there are no controls
	bin := protectTestBinary(t)
	removeTestExclusion(t, bin) // every start below applies every control for real
	starts := 1
	if v, err := strconv.Atoi(os.Getenv("TENANT_CONSOLE_STARTS")); err == nil && v > 0 {
		starts = v
	}
	for _, verb := range []string{"chat", "tui"} {
		for _, sh := range consoleShapes {
			if sh.pipes && verb == "tui" {
				// tuiNeedsConsole: with all three streams redirected the
				// terminal UI prints "starting tenant…" and never draws, on a
				// build with the controls compiled out (-tags debugattach) as
				// on this one. Not a shape it runs in, so not one to count.
				continue
			}
			t.Run(verb+" "+sh.name, func(t *testing.T) {
				t.Parallel()
				u := newTenantUnderTest(t, bin)
				if err := os.WriteFile(launchConfigPath(u.cfg), []byte(`{"dashboard":{"enabled":false}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				clean := 0
				for n := 1; n <= starts; n++ {
					if err := cleanStart(u, sh, verb, n); err != nil {
						t.Fatalf("start %d of %d was not clean (%d before it were): %v", n, starts, clean, err)
					}
					clean++
				}
				t.Logf("%d of %d consecutive starts were clean", clean, starts)
			})
		}
	}
}
