package main

// fleet_e2e_test.go: the fleet with every piece a process of its own
// (TEN-531; docs/plans/gap6-fleet-plan.md, "Phases"). A master is a real
// `tenant serve`; a machine is a real `tenant child install` and `tenant
// child run`; the operator is `tenant fleet …` in further processes and the
// dashboard's HTTP API. Every process is this test binary started again as
// TestFleetE2EProcess, with no window, on temp dirs, on loopback addresses
// and ports the system picks, with a service manager that only writes down
// what it is asked.
//
// A turn needs a model that calls tools, and the offline echo backend calls
// none. So a master of these tests runs on the vLLM backend, pointed at a
// scripted model inside its own process (scriptedModel): the message a test
// sends through the dashboard names the tool calls, the scripted model makes
// them one by one, and when the turn is over it prints what the real agent
// handed it: the tools it was offered, the fleet's lines of its system
// prompt, and each tool result exactly as a model would read it. Everything
// between that message and that result is the shipped code: the dashboard,
// the agent loop, the tool mux, the fleet tools, the approval broker and its
// cards, the link, the child's gate, the child's tools.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/control"
	"tenant/internal/procprotect"
)

const (
	e2eProcEnv = "TENANT_FLEET_E2E_PROC" // the helper's orders: base64 of an e2eSpec
	e2eDistro  = "Ubuntu-24.04"          // the WSL distribution a Linux process of a Windows run lives in
	e2eWait    = 90 * time.Second        // how long a test waits for what must happen
)

// --- the helper process ---

// e2eSpec is what one helper process does.
type e2eSpec struct {
	Argv []string          `json:"argv"`          // the tenant command line
	Env  map[string]string `json:"env,omitempty"` // set before the command runs
	// Answer is what the operator types when the command asks at its terminal
	// (the six digits; keep or delete). nil: nobody is there.
	Answer *string `json:"answer,omitempty"`
	// Hold: the process lives until its standard input closes, and answers
	// "stats" there with what it has cost so far.
	Hold bool `json:"hold,omitempty"`
	// Model: the command is serve; run it on the scripted model.
	Model bool `json:"model,omitempty"`
	// Shipped: the intervals a shipped Tenant runs with, where the tests
	// otherwise shorten them. What the idle budget is measured with.
	Shipped bool `json:"shipped,omitempty"`
	// Scratch is where the service manager's commands are written down and
	// where a unit file may go.
	Scratch string `json:"scratch,omitempty"`
}

// TestFleetE2EProcess is not a test: it is `tenant <argv>` as a process of
// its own, for the tests in this file.
func TestFleetE2EProcess(t *testing.T) {
	enc := os.Getenv(e2eProcEnv)
	if enc == "" {
		t.Skip("the fleet's end-to-end tests start this")
	}
	_ = os.Unsetenv(e2eProcEnv)
	var spec e2eSpec
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err == nil {
		err = json.Unmarshal(raw, &spec)
	}
	if err != nil || len(spec.Argv) == 0 {
		fmt.Fprintln(os.Stderr, "e2e helper: bad orders:", err)
		os.Exit(9)
	}
	time.AfterFunc(45*time.Minute, func() { os.Exit(4) }) // never outlives a test run
	// Protected as main protects every command (TEN-535). Every control is
	// applied: one forced off would stop serve at its acknowledgement gate.
	// On Windows that puts this binary's file name on the user's Windows
	// Error Reporting exclusion list.
	protection = procprotect.Protect()
	for k, v := range spec.Env {
		_ = os.Setenv(k, v)
	}
	e2eUpdateSeams(spec) // fleet_update_e2e_test.go: what an update of a machine needs here
	e2eRehomeSeams()     // fleet_e2e_rehome_test.go: an init that hands the test its recovery identity
	if spec.Hold {
		fmt.Printf("E2E-PID %d\n", os.Getpid())
		go e2eHold()
	}
	if spec.Answer != nil {
		fleetAsk = func(string) (string, error) { return *spec.Answer, nil }
	}
	fleetWatchInterval = 200 * time.Millisecond
	if spec.Shipped {
		fleetWatchInterval, childPoll = settingsWatchInterval, time.Second
	}
	// No helper reaches this computer's service manager, whatever it runs.
	runCmd = func(_ context.Context, name string, a ...string) (string, error) {
		if spec.Scratch == "" {
			return notLoaded(name, a)
		}
		f, err := os.OpenFile(filepath.Join(spec.Scratch, "service-manager.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err = fmt.Fprintln(f, strings.Join(append([]string{name}, a...), " ")); err != nil {
			return "", err
		}
		return notLoaded(name, a)
	}
	// That stand-in cannot sign a binary either, so the macOS signing step is
	// off in a helper: with it on, a machine refuses every update as "codesign
	// ran, but the hardened-runtime flag is not on" (CI's Mac, the update
	// test). childsign_test.go is the test of that step.
	signsBinaries = false
	if spec.Scratch != "" {
		_ = os.MkdirAll(spec.Scratch, 0o700)
		_ = os.Setenv("TENANT_CHILD_HELPER_UNITS", filepath.Join(spec.Scratch, "units"))
	}
	childHelperSeams()
	killChildrenOnExit() // what this process starts dies with it, however it ends

	argv := spec.Argv
	if spec.Model {
		endpoint := scriptedModel()
		argv = append(argv, "--backend", "vllm", "--vllm-endpoint", endpoint, "--vllm-model", "scripted", "--vllm-tool-format", "openai",
			"--embed-endpoint", endpoint, "--allow-no-memory")
	}
	if argv[0] == "serve" {
		// The dashboard's token of this temp config dir, for the test that
		// plays the operator there.
		if i := slices.Index(argv, "--config"); i >= 0 && i+1 < len(argv) && !slices.Contains(argv, "--dashboard=false") {
			if tok, err := resolveDashboardAuth(argv[i+1], ""); err == nil {
				fmt.Println("E2E-TOKEN " + tok)
			}
		}
	}
	c := cliRegistry().Lookup(argv[0])
	if c == nil {
		fmt.Fprintln(os.Stderr, "e2e helper: unknown command", argv[0])
		os.Exit(9)
	}
	procprotect.Ordinary(func() { err = c.Run(context.Background(), argv[1:]) }) // as main runs a command
	sessionEndFinished()
	if err != nil && err.Error() != "" {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	os.Exit(exitCode(err))
}

// e2eHold ends the process when the test that started it lets go of its
// standard input, however the test went away. Until then "stats" on a line
// is answered with the process's goroutines, CPU time and resident memory.
func e2eHold() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if sc.Text() == "stats" {
			cpu, rss := selfUsage()
			fmt.Printf("E2E-STATS %d %d %d\n", runtime.NumGoroutine(), cpu.Nanoseconds(), rss)
		}
		if cmd, ok := strings.CutPrefix(sc.Text(), "memory "); ok {
			fmt.Println("E2E-MEMORY " + e2eMemory(cmd)) // fleet_e2e_memory_test.go
		}
	}
	os.Exit(3)
}

// --- the scripted model ---

// e2eScriptMark opens a message that is a script: one "CALL <tool> <json>"
// line per tool call, made in order.
const e2eScriptMark = "E2E-SCRIPT"

var (
	e2eCallLine = regexp.MustCompile(`(?m)^CALL (\S+) (\{.*\})\s*$`)
	e2eCallSeq  atomic.Int64
)

// e2eTold is what a turn's model was handed, printed when the turn ends.
type e2eTold struct {
	Results []string `json:"results"` // each tool result, as the model reads it
	Tools   []string `json:"tools"`   // the tools it was offered
	Fleet   string   `json:"fleet"`   // its system prompt from "Fleet:" on
}

// scriptedModel serves the OpenAI chat API on a loopback port of this
// process and returns its base URL. It is the model of a master under test.
func scriptedModel() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e helper: scripted model:", err)
		os.Exit(9)
	}
	go func() { _ = http.Serve(ln, http.HandlerFunc(scriptedChat)) }()
	return "http://" + ln.Addr().String()
}

func scriptedChat(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/embeddings") && os.Getenv(e2eEmbedEnv) != "" {
		scriptedEmbed(w, r)
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r) // the model list, the tokenizer, embeddings: the callers fall back
		return
	}
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role      string            `json:"role"`
			Content   json.RawMessage   `json:"content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var script [][]string
	told := e2eTold{}
	finished := false
	var system string
	for _, m := range req.Messages {
		text := scriptedText(m.Content)
		switch {
		case m.Role == "system":
			system += text + "\n"
		case m.Role == "user" && strings.Contains(text, e2eScriptMark):
			script, told.Results, finished = e2eCallLine.FindAllStringSubmatch(text, -1), nil, false
		case m.Role == "tool":
			told.Results = append(told.Results, text)
		case m.Role == "assistant" && len(m.ToolCalls) == 0:
			finished = true // that script's turn ended earlier
		}
	}
	switch {
	case len(req.Tools) == 0 || script == nil || finished:
		scriptedAnswer(w, req.Stream, "ok", nil) // not a turn with a script: a probe, a title, a judge
	case len(told.Results) < len(script):
		scriptedAnswer(w, req.Stream, "", script[len(told.Results)])
	default:
		for _, tool := range req.Tools {
			told.Tools = append(told.Tools, tool.Function.Name)
		}
		if i := strings.Index(system, "Fleet:"); i >= 0 {
			told.Fleet = system[i:min(len(system), i+4096)]
		}
		raw, _ := json.Marshal(told)
		fmt.Println("E2E-TURN " + base64.RawURLEncoding.EncodeToString(raw))
		scriptedAnswer(w, req.Stream, "done", nil)
	}
}

// e2eEmbedEnv makes the scripted model an embedder too: a master on it then
// writes episodes, and its machines' memory (TEN-532).
const e2eEmbedEnv = "TENANT_FLEET_E2E_EMBED"

// scriptedEmbed answers the OpenAI embeddings API with a small vector per
// text, made from the text.
func scriptedEmbed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	out := struct {
		Data []item `json:"data"`
	}{}
	for i, s := range req.Input {
		out.Data = append(out.Data, item{Embedding: []float32{float32(len(s) % 97), float32(strings.Count(s, " ") % 13), 1}, Index: i})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// scriptedText is a message's text: a string, or the text parts of a list.
func scriptedText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	for _, p := range parts {
		s += p.Text
	}
	return s
}

// scriptedAnswer writes one model step: a tool call (call is a match of
// e2eCallLine) or text, streamed or whole as the request asked.
func scriptedAnswer(w http.ResponseWriter, stream bool, text string, call []string) {
	var delta, message map[string]any
	finish := "stop"
	if call != nil {
		id := fmt.Sprintf("e2e%d", e2eCallSeq.Add(1))
		fn := map[string]any{"name": call[1], "arguments": call[2]}
		delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "function": fn}}}
		message = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": fn}}}
		finish = "tool_calls"
	} else {
		delta = map[string]any{"content": text}
		message = map[string]any{"role": "assistant", "content": text}
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
}

// e2eScript is the message that makes a master's agent make these calls.
func e2eScript(calls ...string) string {
	return e2eScriptMark + "\n" + strings.Join(calls, "\n")
}

// e2eCall is one line of a script.
func e2eCall(tool string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	raw, _ := json.Marshal(args)
	return "CALL " + tool + " " + string(raw)
}

// --- where a process runs ---

// e2eHost is a machine of a test: this one, or (from Windows) the Linux
// inside WSL. Each has its own directory for the test's files.
type e2eHost struct {
	t    *testing.T
	wsl  bool
	bin  string // the test binary as this host runs it
	root string // this host's directory, as this host names it
	os   string // windows | linux | darwin
	// rehome, when not nil, names the only variables of this account's
	// environment a process here gets pointed at the test's own home folder
	// (e2eHomeVars lists them all, and nil means all of them). An empty list
	// leaves the environment the account's own.
	rehome []string
}

// e2eHomeVars are the variables that say where an account keeps its things.
var e2eHomeVars = []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME"}

// thisHost is the computer the test runs on.
func thisHost(t *testing.T) *e2eHost {
	t.Helper()
	h := &e2eHost{t: t, bin: os.Args[0], root: realTemp(t), os: runtime.GOOS}
	if err := os.MkdirAll(h.join("home"), 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *e2eHost) join(elem ...string) string {
	if h.wsl {
		return path.Join(append([]string{h.root}, elem...)...)
	}
	return filepath.Join(append([]string{h.root}, elem...)...)
}

// command is a helper process on this host that will do spec.
func (h *e2eHost) command(spec e2eSpec) *exec.Cmd {
	raw, _ := json.Marshal(spec)
	orders := e2eProcEnv + "=" + base64.RawURLEncoding.EncodeToString(raw)
	home := h.join("home")
	var cmd *exec.Cmd
	if h.wsl {
		cmd = exec.Command("wsl.exe", "-d", e2eDistro, "-e", "env", "HOME="+home, "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home,
			"TMPDIR=/tmp", h.bin, "-test.run=^TestFleetE2EProcess$")
		cmd.Env = append(os.Environ(), orders, "WSLENV="+e2eProcEnv+"/u")
	} else {
		cmd = exec.Command(h.bin, "-test.run=^TestFleetE2EProcess$")
		// clobberfree: the collector overwrites what it frees, so memory
		// that was freed while still in use reads as garbage every time and
		// not only when something else has moved in. That is how a master
		// reporting the wrong update rule on Apple Silicon was pinned on
		// go1.26's arm64 compiler (sendUpdate in fleetupdate.go): without
		// this the update test below failed one run in three, with it
		// every run. A helper's own children inherit it.
		cmd.Env = append(os.Environ(), orders, "GODEBUG=clobberfree=1")
		// No default dir of this account is in reach, whatever a command's flags say.
		vars := e2eHomeVars
		if h.rehome != nil {
			vars = h.rehome
		}
		for _, env := range vars {
			cmd.Env = append(cmd.Env, env+"="+home)
		}
	}
	e2eQuiet(cmd)
	return cmd
}

// run runs one command to its end.
func (h *e2eHost) run(spec e2eSpec) (stdout, stderr string, exit int) {
	h.t.Helper()
	cmd := h.command(spec)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil && cmd.ProcessState == nil {
		h.t.Fatalf("tenant %v did not run: %v", spec.Argv, err)
	}
	return strings.ReplaceAll(out.String(), "\r\n", "\n"), strings.ReplaceAll(errb.String(), "\r\n", "\n"), cmd.ProcessState.ExitCode()
}

// Commands in the shell of this host's os_exec: PowerShell or sh.

// say prints word.
func (h *e2eHost) say(word string) string {
	if h.os == "windows" {
		return "Write-Output " + word
	}
	return "echo " + word
}

// sayJoined prints a and b as one word that is nowhere in the command.
func (h *e2eHost) sayJoined(a, b string) string {
	if h.os == "windows" {
		return fmt.Sprintf("Write-Output ('%s'+'%s')", a, b)
	}
	return fmt.Sprintf(`printf '%%s%%s\n' '%s' '%s'`, a, b)
}

// touch makes a file: a command no reader would call read-only. It prints
// nothing. (A command that prints its own argument back, as New-Item does
// with the path, makes that argument a machine's untrusted text: the next
// call that carries the same path then meets the egress check.)
func (h *e2eHost) touch(file string) string {
	if h.os == "windows" {
		return fmt.Sprintf("Set-Content -LiteralPath '%s' -Value made", file)
	}
	return fmt.Sprintf("touch '%s'", file)
}

// slow writes the file start, waits secs seconds, writes the file end and
// prints word: a command a test can see start, and see finish or not.
func (h *e2eHost) slow(start string, secs int, end, word string) string {
	if h.os == "windows" {
		return fmt.Sprintf("Set-Content -LiteralPath '%s' -Value started; Start-Sleep -Seconds %d; Set-Content -LiteralPath '%s' -Value ended; Write-Output %s", start, secs, end, word)
	}
	return fmt.Sprintf("echo started > '%s'; sleep %d; echo ended > '%s'; echo %s", start, secs, end, word)
}

// --- a running process ---

// e2eLines collects what a process prints on one stream, line by line.
type e2eLines struct {
	mu   sync.Mutex
	part []byte
	all  []string
}

func (l *e2eLines) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.part = append(l.part, b...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			return len(b), nil
		}
		l.all = append(l.all, strings.TrimRight(string(l.part[:i]), "\r"))
		l.part = l.part[i+1:]
	}
}

func (l *e2eLines) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.all)
}

// find is the first line at or after from that holds substr: its index, the
// line, and whether there is one.
func (l *e2eLines) find(from int, substr string) (int, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.all); i++ {
		if strings.Contains(l.all[i], substr) {
			return i, l.all[i], true
		}
	}
	return 0, "", false
}

// String is what was printed, without the lines that are the tests' own
// plumbing (a turn's report is tens of kilobytes of base64).
func (l *e2eLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []string
	for _, s := range l.all {
		if !strings.HasPrefix(s, "E2E-TURN ") && !strings.HasPrefix(s, "E2E-TOKEN ") {
			keep = append(keep, s)
		}
	}
	return strings.Join(keep, "\n")
}

// e2eProc is a helper process that runs until the test lets go of it.
type e2eProc struct {
	t        *testing.T
	host     *e2eHost
	what     string
	cmd      *exec.Cmd
	in       io.WriteCloser
	out, err *e2eLines
	done     chan struct{}
	pid      string // the process's own id, on its host
	stats    int    // lines of out already read for stats
}

// start starts a process that lives until the test ends or stop is called.
func (h *e2eHost) start(what string, spec e2eSpec) *e2eProc {
	h.t.Helper()
	spec.Hold = true
	p := &e2eProc{t: h.t, host: h, what: what, cmd: h.command(spec), out: &e2eLines{}, err: &e2eLines{}, done: make(chan struct{})}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.err
	var err error
	if p.in, err = p.cmd.StdinPipe(); err != nil {
		h.t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	h.t.Cleanup(p.stop)
	_, line := p.await(0, "E2E-PID ", "the process starts")
	p.pid = strings.TrimPrefix(line, "E2E-PID ")
	if !h.wsl { // the helper prints its pid once Protect has run, as main runs it first
		if err := e2eProtected(p.pid); err != nil {
			h.t.Errorf("%s is not a protected process (TEN-535): %v", what, err)
		}
	}
	return p
}

// said is everything the process printed, for a failure message.
func (p *e2eProc) said() string {
	return fmt.Sprintf("%s printed:\n%s\n%s", p.what, p.out, p.err)
}

func (p *e2eProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// await waits for a line of standard output, at or after from, that holds
// substr.
func (p *e2eProc) await(from int, substr, what string) (int, string) {
	p.t.Helper()
	for deadline := time.Now().Add(e2eWait); ; time.Sleep(20 * time.Millisecond) {
		if i, line, ok := p.out.find(from, substr); ok {
			return i, line
		}
		if p.exited() {
			if i, line, ok := p.out.find(from, substr); ok { // printed as it went
				return i, line
			}
			p.t.Fatalf("%s ended before %s\n%s", p.what, what, p.said())
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("after %s: %s\n%s", e2eWait, what, p.said())
		}
	}
}

// ended waits for the process to end by itself.
func (p *e2eProc) ended(what string) {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(e2eWait):
		p.t.Fatalf("after %s: %s\n%s", e2eWait, what, p.said())
	}
}

// stop ends the process the way a service manager stops a service: it is
// told, and given time.
func (p *e2eProc) stop() {
	_ = p.in.Close()
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		p.kill()
		p.t.Errorf("%s had to be killed", p.what)
	}
}

// kill ends the process the way a power cut does: at once, with no goodbye.
func (p *e2eProc) kill() {
	if p.host.wsl && p.pid != "" {
		k := exec.Command("wsl.exe", "-d", e2eDistro, "-e", "kill", "-9", p.pid)
		e2eQuiet(k)
		_ = k.Run()
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

// usage asks the process what it has cost so far.
func (p *e2eProc) usage() (goroutines int, cpu time.Duration, rss int64) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, "stats\n"); err != nil {
		p.t.Fatalf("%s: %v", p.what, err)
	}
	i, line := p.await(p.stats, "E2E-STATS ", "the process says what it has cost")
	p.stats = i + 1
	var ns int64
	if _, err := fmt.Sscanf(line, "E2E-STATS %d %d %d", &goroutines, &ns, &rss); err != nil {
		p.t.Fatalf("%s: %q: %v", p.what, line, err)
	}
	return goroutines, time.Duration(ns), rss
}

// --- a master ---

// e2eMaster is a `tenant serve` on a host, and the operator at it.
type e2eMaster struct {
	t         *testing.T
	host      *e2eHost
	cfg, data string
	flags     []string          // serve's own, beside the dirs
	scripted  bool              // on the scripted model, with a dashboard: it takes turns
	master    bool              // its dirs hold a fleet: serve listens for it as it starts
	shipped   bool              // the intervals a shipped Tenant runs with
	env       map[string]string // for its serve process
	proc      *e2eProc
	dash      string // the dashboard's base URL
	token     string
	fleet     string // where the fleet port listens
	turns     int    // lines of proc.out already read for turns
	client    *http.Client
}

// master makes a master's dirs on h. serve starts it.
func (h *e2eHost) master(scripted bool, flags ...string) *e2eMaster {
	return &e2eMaster{t: h.t, host: h, cfg: h.join("master-cfg"), data: h.join("master-data"), flags: flags, scripted: scripted,
		client: &http.Client{Timeout: 30 * time.Second}}
}

// serve starts the master's process, or starts it again after it ended. A
// master that is a fleet master already is listening when this returns.
func (m *e2eMaster) serve() {
	m.t.Helper()
	argv := append([]string{"serve", "--config", m.cfg, "--data", m.data, "--self-improve=false"}, m.flags...)
	if m.scripted {
		argv = append(argv, "--dashboard-addr", "127.0.0.1:0")
	} else {
		argv = append(argv, "--backend", "echo", "--dashboard=false")
	}
	m.proc = m.host.start("serve", e2eSpec{Argv: argv, Model: m.scripted, Shipped: m.shipped, Env: m.env})
	m.turns, m.fleet = 0, ""
	if m.scripted {
		_, line := m.proc.await(0, "E2E-TOKEN ", "serve has a dashboard token")
		m.token = strings.TrimPrefix(line, "E2E-TOKEN ")
		_, line = m.proc.await(0, "dashboard (control + chat + approvals): ", "serve says where its dashboard is")
		m.dash = strings.TrimSpace(line[strings.Index(line, "http://"):])
	}
	m.proc.await(0, "running 24/7", "serve is up")
	if m.master {
		m.listening(0)
	}
}

var e2eListening = regexp.MustCompile(`fleet: listening on (127\.0\.0\.1:\d+)`)

// listening waits for serve to say where the fleet port listens.
func (m *e2eMaster) listening(from int) {
	m.t.Helper()
	_, line := m.proc.await(from, "fleet: listening on ", "serve listens for its fleet")
	found := e2eListening.FindStringSubmatch(line)
	if found == nil {
		m.t.Fatalf("serve listens for its fleet somewhere that is not a loopback port: %q", line)
	}
	m.fleet = found[1]
}

// cli runs `tenant <args>` on the master's dirs in a process of its own.
func (m *e2eMaster) cli(answer *string, args ...string) (stdout, stderr string, exit int) {
	m.t.Helper()
	return m.host.run(e2eSpec{Argv: append(args, "--config", m.cfg, "--data", m.data), Answer: answer})
}

// must is cli for a command that must succeed: what it printed.
func (m *e2eMaster) must(answer *string, args ...string) string {
	m.t.Helper()
	out, errOut, exit := m.cli(answer, args...)
	if exit != 0 {
		m.t.Fatalf("tenant %s: exit %d\n%s\n%s", strings.Join(args, " "), exit, out, errOut)
	}
	return out + errOut
}

// init makes the running master a fleet master on a loopback port the
// system picks, and sets what its machines dial: dial, or the port itself.
func (m *e2eMaster) init(dial string) {
	m.t.Helper()
	from := m.proc.out.count()
	// --listen before anything else: the default is every interface.
	m.must(nil, "fleet", "init", "--listen", "127.0.0.1:0", "--wait", "60s")
	m.master = true
	m.listening(from)
	if dial == "" {
		dial = m.fleet
	}
	m.must(nil, "fleet", "init", "--address", dial)
}

// schemes waits until the running master has read a change the command line
// made to the machines' approval schemes (its settings watcher looks every
// few seconds).
func (m *e2eMaster) schemes(from int) {
	m.t.Helper()
	m.proc.await(from, "fleet scheme change", "serve reads the scheme the command line saved")
}

// api makes one call of the dashboard's API as the operator, and decodes a
// JSON answer into out when out is not nil.
func (m *e2eMaster) api(method, route string, body, out any) (int, string) {
	m.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, m.dash+route, rd)
	if err != nil {
		m.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.t.Fatalf("%s %s: %v\n%s", method, route, err, m.proc.said())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			m.t.Fatalf("%s %s: %v\n%s", method, route, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

// e2eTurn is one turn of a master's agent as a test saw it: what the model
// was handed, and the approval cards the operator was shown meanwhile.
type e2eTurn struct {
	e2eTold
	Cards []control.PendingApproval
}

// begin sends the master's agent a message through the dashboard's chat.
func (m *e2eMaster) begin(script string) {
	m.t.Helper()
	m.turns = m.proc.out.count()
	if code, body := m.api(http.MethodPost, "/chat/send", map[string]string{"text": script}, nil); code != http.StatusOK || strings.Contains(body, "Not sent") {
		m.t.Fatalf("the dashboard did not take the message: %d\n%s", code, body)
	}
}

// cards is the approval queue as the dashboard shows it now.
func (m *e2eMaster) cards() []control.PendingApproval {
	m.t.Helper()
	var pending []control.PendingApproval
	if code, body := m.api(http.MethodGet, "/api/approvals", nil, &pending); code != http.StatusOK {
		m.t.Fatalf("the approval queue: %d %s", code, body)
	}
	return pending
}

// decide answers one card as the operator: approve, approve_always or deny.
func (m *e2eMaster) decide(card control.PendingApproval, decision string) {
	m.t.Helper()
	if code, body := m.api(http.MethodPost, "/api/approvals/"+card.ID, map[string]string{"decision": decision}, nil); code != http.StatusOK {
		m.t.Fatalf("answering %s with %s: %d %s", card.ID, decision, code, body)
	}
}

// approve answers every card that comes up with approve, each once, until
// done says to stop. It is for a turn a test does not see to its end (a
// stop, a kill); it runs beside the test, so it fails nothing itself.
func (m *e2eMaster) approve(done func() bool) {
	seen := map[string]bool{}
	for !done() {
		var pending []control.PendingApproval
		req, err := http.NewRequest(http.MethodGet, m.dash+"/api/approvals", nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+m.token)
		if resp, err := m.client.Do(req); err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&pending)
			_ = resp.Body.Close()
		}
		for _, card := range pending {
			if seen[card.ID] {
				continue
			}
			seen[card.ID] = true
			post, err := http.NewRequest(http.MethodPost, m.dash+"/api/approvals/"+card.ID, strings.NewReader(`{"decision":"approve"}`))
			if err != nil {
				return
			}
			post.Header.Set("Authorization", "Bearer "+m.token)
			post.Header.Set("Content-Type", "application/json")
			if resp, err := m.client.Do(post); err == nil {
				_ = resp.Body.Close()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// finish plays the operator until the turn begin started is over: every
// card that comes up is answered with what decide returns for it (a nil
// decide, or "", leaves the card waiting).
func (m *e2eMaster) finish(decide func(control.PendingApproval) string) e2eTurn {
	m.t.Helper()
	var turn e2eTurn
	seen := map[string]bool{}
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(50 * time.Millisecond) {
		for _, card := range m.cards() {
			if seen[card.ID] {
				continue
			}
			seen[card.ID] = true
			turn.Cards = append(turn.Cards, card)
			if decide != nil {
				if d := decide(card); d != "" {
					m.decide(card, d)
				}
			}
		}
		if _, line, ok := m.proc.out.find(m.turns, "E2E-TURN "); ok {
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(line, "E2E-TURN "))
			if err == nil {
				err = json.Unmarshal(raw, &turn.e2eTold)
			}
			if err != nil {
				m.t.Fatalf("the turn's report: %v", err)
			}
			break
		}
		if m.proc.exited() || time.Now().After(deadline) {
			m.t.Fatalf("the turn did not end\n%s", m.proc.said())
		}
	}
	m.idle() // the agent is free for the next message once it has filed this turn
	return turn
}

// idle waits until the agent is in no turn.
func (m *e2eMaster) idle() {
	m.t.Helper()
	for deadline := time.Now().Add(e2eWait); ; time.Sleep(50 * time.Millisecond) {
		var status struct {
			TurnActive bool `json:"turn_active"`
		}
		if code, _ := m.api(http.MethodGet, "/api/status", nil, &status); code == http.StatusOK && !status.TurnActive {
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("the agent is still in its turn\n%s", m.proc.said())
		}
	}
}

// stop is the operator's Stop on the dashboard's chat. It returns once the
// agent is out of its turn.
func (m *e2eMaster) stop() {
	m.t.Helper()
	if code, body := m.api(http.MethodPost, "/chat/stop", nil, nil); code >= 300 {
		m.t.Fatalf("the dashboard's stop: %d\n%s", code, body)
	}
	m.idle()
}

// turn is begin and finish.
func (m *e2eMaster) turn(script string, decide func(control.PendingApproval) string) e2eTurn {
	m.t.Helper()
	m.begin(script)
	return m.finish(decide)
}

// one makes one tool call in a turn of its own and returns its result as
// the model read it, and the cards it raised.
func (m *e2eMaster) one(tool string, args map[string]any, decision string) (string, []control.PendingApproval) {
	m.t.Helper()
	turn := m.turn(e2eScript(e2eCall(tool, args)), func(control.PendingApproval) string { return decision })
	if len(turn.Results) != 1 {
		m.t.Fatalf("%s %v: the model was handed %d results: %q", tool, args, len(turn.Results), turn.Results)
	}
	return turn.Results[0], turn.Cards
}

// --- a machine ---

// e2eChild is a machine of a master's fleet: `tenant child` on a host.
type e2eChild struct {
	t                     *testing.T
	host                  *e2eHost
	m                     *e2eMaster
	name, dir, id, digits string
	scratch               string
	shipped               bool
	env                   map[string]string // for its run process
	proc                  *e2eProc
}

var e2eEnrolled = regexp.MustCompile(`Enrolled as (\S+) \((m[0-9a-f]{16})\)`)

// enroll makes a one-time code on the master and spends it on h with
// `tenant child install`: the machine is then pending, and has shown its six
// digits. The service it "installs" is written down, not registered.
func (m *e2eMaster) enroll(h *e2eHost, name string, flags ...string) *e2eChild {
	m.t.Helper()
	c := &e2eChild{t: m.t, host: h, m: m, name: name, dir: h.join(name, "child"), scratch: h.join(name)}
	if !h.wsl {
		if err := os.MkdirAll(c.scratch, 0o700); err != nil {
			m.t.Fatal(err)
		}
	}
	code, errOut, exit := m.cli(nil, "fleet", "enroll", name)
	if code = strings.TrimSpace(code); exit != 0 || !strings.HasPrefix(code, "tntfleet1_") || strings.ContainsAny(code, " \n") {
		m.t.Fatalf("fleet enroll %s: exit %d, standard output %q\n%s", name, exit, code, errOut)
	}
	out, errOut, exit := h.run(e2eSpec{Argv: append([]string{"child", "install", "--dir", c.dir}, flags...),
		Env: map[string]string{childCodeEnv: code}, Scratch: c.scratch})
	digits, enrolled := confirmationCode.FindStringSubmatch(out), e2eEnrolled.FindStringSubmatch(out)
	if exit != 0 || digits == nil || enrolled == nil || enrolled[1] != name {
		m.t.Fatalf("child install for %s: exit %d\n%s\n%s", name, exit, out, errOut)
	}
	if strings.Contains(out+errOut, code) {
		m.t.Errorf("child install printed the enrollment code")
	}
	c.digits, c.id = digits[1], enrolled[2]
	return c
}

// confirm confirms the machine on the master: the operator types its six
// digits. set is category=mode pairs for the confirm screen.
func (c *e2eChild) confirm(set ...string) string {
	c.t.Helper()
	from := c.m.proc.out.count()
	args := []string{"fleet", "confirm", c.name}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out := c.m.must(&c.digits, args...)
	c.m.schemes(from)
	return out
}

// run starts the machine's child the way its unit does: `tenant child run`.
func (c *e2eChild) run() {
	c.t.Helper()
	c.proc = c.host.start("child "+c.name, e2eSpec{Argv: []string{"child", "run", "--dir", c.dir}, Env: c.env, Scratch: c.scratch, Shipped: c.shipped})
}

// tenant runs `tenant child <verb>` on the machine.
func (c *e2eChild) tenant(verb string) (string, int) {
	c.t.Helper()
	out, errOut, exit := c.host.run(e2eSpec{Argv: []string{"child", verb, "--dir", c.dir}, Scratch: c.scratch})
	return out + errOut, exit
}

// state waits until `tenant child status` on the machine says want.
func (c *e2eChild) state(want string) string {
	c.t.Helper()
	var out string
	for deadline := time.Now().Add(e2eWait); ; time.Sleep(100 * time.Millisecond) {
		if out, _ = c.tenant("status"); strings.Contains(out, want) {
			return out
		}
		if time.Now().After(deadline) {
			said := ""
			if c.proc != nil {
				said = c.proc.said()
			}
			c.t.Fatalf("after %s `tenant child status` on %s does not say %q:\n%s\n%s", e2eWait, c.name, want, out, said)
		}
	}
}

// join takes a new machine all the way: enrolled, confirmed, running and
// connected.
func (m *e2eMaster) join(h *e2eHost, name string, flags ...string) *e2eChild {
	m.t.Helper()
	c := m.enroll(h, name, flags...)
	c.confirm()
	c.run()
	c.state("connected to the master")
	return c
}

// --- the wire between a machine and its master ---

// e2eWire is a TCP relay of the test's between machines and a master: what
// a network between them would be. It can record every byte in each
// direction, count them, go dead without a word (a pulled cable), go away
// altogether (an unreachable master), and lead somewhere else (a master
// that came back on another port).
type e2eWire struct {
	t    *testing.T
	addr string

	mu     sync.Mutex
	ln     net.Listener
	target string
	dead   bool
	record bool
	conns  []*e2eConn
	up     atomic.Int64 // bytes machines sent
	down   atomic.Int64 // bytes the master sent
}

// e2eConn is one connection through the wire, and what went over it.
type e2eConn struct {
	in, out  net.Conn
	up, down bytes.Buffer // machine to master, master to machine
}

func newE2EWire(t *testing.T) *e2eWire {
	t.Helper()
	w := &e2eWire{t: t}
	w.listen("127.0.0.1:0")
	t.Cleanup(w.close)
	return w
}

func (w *e2eWire) listen(addr string) {
	w.t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		w.t.Fatal(err)
	}
	w.mu.Lock()
	w.ln, w.addr = ln, ln.Addr().String()
	w.mu.Unlock()
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			w.mu.Lock()
			target := w.target
			w.mu.Unlock()
			out, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				_ = in.Close()
				continue
			}
			c := &e2eConn{in: in, out: out}
			w.mu.Lock()
			w.conns = append(w.conns, c)
			w.mu.Unlock()
			go w.pump(out, in, &c.up, &w.up)
			go w.pump(in, out, &c.down, &w.down)
		}
	}()
}

func (w *e2eWire) pump(dst, src net.Conn, rec *bytes.Buffer, n *atomic.Int64) {
	b := make([]byte, 32<<10)
	for {
		k, err := src.Read(b)
		w.mu.Lock()
		dead := w.dead
		if k > 0 && w.record && rec.Len() < 16<<20 {
			rec.Write(b[:k])
		}
		w.mu.Unlock()
		if dead {
			if err != nil {
				return // a dead cable carries no goodbye either
			}
			continue
		}
		n.Add(int64(k))
		if k > 0 {
			if _, werr := dst.Write(b[:k]); werr != nil {
				err = werr
			}
		}
		if err != nil {
			_ = dst.Close()
			_ = src.Close()
			return
		}
	}
}

// to makes the wire lead to target from the next connection on.
func (w *e2eWire) to(target string) {
	w.mu.Lock()
	w.target = target
	w.mu.Unlock()
}

// kill makes the wire dead: every byte from now on is lost, and neither end
// is told.
func (w *e2eWire) kill() {
	w.mu.Lock()
	w.dead = true
	w.mu.Unlock()
}

// mend makes a dead wire carry again: the connections that died with it are
// closed, and new ones pass.
func (w *e2eWire) mend() {
	w.mu.Lock()
	w.dead = false
	w.mu.Unlock()
	w.cut()
}

// cut closes every connection through the wire.
func (w *e2eWire) cut() {
	w.mu.Lock()
	conns := slices.Clone(w.conns)
	w.mu.Unlock()
	for _, c := range conns {
		_ = c.in.Close()
		_ = c.out.Close()
	}
}

// away takes the wire away: nothing listens where the machines dial.
func (w *e2eWire) away() {
	w.mu.Lock()
	ln := w.ln
	w.mu.Unlock()
	_ = ln.Close()
	w.cut()
}

// back puts the wire where it was.
func (w *e2eWire) back() {
	w.t.Helper()
	w.listen(w.addr)
}

func (w *e2eWire) close() {
	w.away()
}

// recorded is what went over every connection through the wire so far: per
// connection, the bytes the machine sent and the bytes the master sent.
func (w *e2eWire) recorded() (conns [][2][]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.conns {
		conns = append(conns, [2][]byte{bytes.Clone(c.up.Bytes()), bytes.Clone(c.down.Bytes())})
	}
	return conns
}
