package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/control"
	"tenant/internal/logs/event"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
	"tenant/internal/plugins/osys"
	"tenant/internal/sandbox"
)

// secSink keeps the Security log's records, one "id actor@surface [approval]
// k=v…" line each. err is what Audit returns: a failing Security log.
type secSink struct {
	mu  sync.Mutex
	got []string
	err error
}

func (s *secSink) Emit(context.Context, *event.Event) {}

func (s *secSink) Audit(_ context.Context, e *event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Log == "security" {
		l := fmt.Sprintf("%d %s@%s", e.ID, e.Actor, e.Surface)
		if e.Corr.ApprovalID != "" {
			l += " [" + e.Corr.ApprovalID + "]"
		}
		for _, f := range e.Data {
			l += " " + f.Key + "=" + f.Text()
		}
		s.got = append(s.got, l)
	}
	return s.err
}

// policyIDs hides the counter in a policy approval's ID.
var policyIDs = regexp.MustCompile(`\[policy:([^\]]*):[0-9a-f]{0,8}-\d+\]`)

func (s *secSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.got))
	for i, l := range s.got {
		out[i] = policyIDs.ReplaceAllString(l, "[policy:$1:N]")
	}
	return out
}

func installSecSink(t *testing.T) *secSink {
	s := &secSink{}
	event.Install(s)
	t.Cleanup(func() { event.Install(nil) })
	return s
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("security records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Every os_exec and cron shell command is a Security record with its
// scrubbed command, backend, exit code and approval; a sandbox refusal and
// an escalation (approved, or a dry run) have their own.
func TestExecSecurityRecords(t *testing.T) {
	s := installSecSink(t)
	token := "ghp_" + strings.Repeat("a", 36)
	ctx := event.WithApproval(context.Background(), "ap-test-1")
	run := profileRunner(resolveExecProfile(surfaceLocal, "", "", ""), surfaceLocal, nil)
	if _, err := run(ctx, "echo ok # "+token, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := runSandboxShell(context.Background(), "exit 3", resolveExecProfile(surfaceCronShell, "", "", t.TempDir())); err == nil {
		t.Fatal("exit 3: no error")
	}
	fs := []sandbox.Violation{{Kind: "filesystem", Detail: "read denied: ~/.ssh"}}
	cmd := sandbox.Command{Shell: "cat ~/.ssh/id", Surface: surfaceDiscord}
	auditExec(ctx, "restricted", cmd, true, sandbox.Result{ExitCode: 1, Violations: fs}, nil)
	auditExec(ctx, "restricted", cmd, true, sandbox.Result{Violations: fs, Escalated: true}, nil)
	auditExec(ctx, "restricted", cmd, false, sandbox.Result{Violations: fs, Escalated: true}, nil)

	got := s.lines()
	if len(got) > 0 && (strings.Contains(got[0], token) || !strings.Contains(got[0], "«secret:")) {
		t.Errorf("the command is not scrubbed: %s", got[0])
	}
	if len(got) > 0 {
		got[0] = regexp.MustCompile(`# \S+`).ReplaceAllString(got[0], "# «token»")
	}
	const refused = "command=cat ~/.ssh/id backend=restricted"
	wantLines(t, got,
		"4201 agent@local [ap-test-1] command=echo ok # «token» backend=host exit=0 timed_out=false",
		"4201 cron@cron-shell command="+strings.Join(cronShellArgv("exit 3"), " ")+" backend=host exit=3 timed_out=false",
		"4201 agent@discord [ap-test-1] "+refused+" exit=1 timed_out=false",
		"4202 agent@discord [ap-test-1] "+refused+" violations=filesystem: read denied: ~/.ssh",
		"4201 agent@discord [ap-test-1] "+refused+" exit=0 timed_out=false",
		"4203 agent@discord [ap-test-1] "+refused+" violations=filesystem: read denied: ~/.ssh why=approved",
		"4201 agent@discord [ap-test-1] "+refused+" exit=0 timed_out=false",
		"4203 agent@discord [ap-test-1] "+refused+" violations=filesystem: read denied: ~/.ssh why=dry_run")
}

// The cron-exec auto-approver no longer bypasses the record: each verdict is
// one, and the command it let run names it as its approval.
func TestCronExecApproverRecords(t *testing.T) {
	s := installSecSink(t)
	svc, _ := osys.Open(osys.Config{Runner: sandboxRunner("", "", nil)})
	d := osys.NewDispatcher(svc, osys.Policy{Confirm: originConfirm(nil)})
	ctx := withExecSurface(withOffsiteConfirm(context.Background(), cronExecApprover(t.TempDir(), t.TempDir())), surfaceCronExec)
	call := func(command string) {
		args, _ := json.Marshal(map[string]string{"command": command})
		_, _, _ = d.Dispatch(ctx, model.ToolCall{Name: "os_exec", Arguments: args})
	}
	call("echo hi")
	call("rm -rf ~/no-such-dir") // dangerous to the classifier, harmless if it ever ran
	_, danger := osys.Classify("rm -rf ~/no-such-dir")
	wantLines(t, s.lines(),
		"4102 tenant@cron [policy:exec:os_exec:N] category=exec action=os_exec detail=echo hi origin=cron grant=once via=cron",
		"4201 agent@cron-exec [policy:exec:os_exec:N] command=echo hi backend=host exit=0 timed_out=false",
		"4103 tenant@cron [policy:destructive:os_exec_dangerous:N] category=destructive action=os_exec_dangerous detail=rm -rf ~/no-such-dir  [DANGER: "+danger+"] origin=cron via=cron")
}

// A Discord card's verdict names the user who clicked and the card's nonce;
// an iMessage reply names the operator and its nonce.
func TestChannelApprovalSecurityRecords(t *testing.T) {
	s := installSecSink(t)
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 5 * time.Second
	a.setChannel("dm1")
	br := newDiscordApprovalBroker(testLog())
	br.SetAsk(func(ctx context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		return a.decide(ctx, req.Action, req.Detail)
	})
	done := make(chan bool, 1)
	go func() { done <- br.Confirm(context.Background(), "os_exec", "uptime") }()
	if !waitSends(fs, 1, 2*time.Second) {
		t.Fatal("no card posted")
	}
	nonce := approveNonce(fs, 0)
	a.tryInteraction(discord.Interaction{ID: "i1", Token: "tok", UserID: "42", CustomID: "approve:" + nonce})
	if !<-done {
		t.Fatal("clicked Approve: refused")
	}

	sender := &fakeApproveSender{}
	ap := newIMessageApprover(sender, opHandle, nil)
	ap.setChat("chatOP")
	im := newOffsiteApprovalBroker(testLog(), newApprovalBroker(testLog()))
	im.SetPermission(catExec, "ask")
	im.SetAsk(func(ctx context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		return ap.decide(ctx, req.Action, req.Detail)
	})
	go func() { done <- im.Confirm(context.Background(), "os_exec", "uptime") }()
	inonce := firstPromptNonce(t, ap, sender)
	ap.OnReply("no "+inonce, opHandle)
	if <-done {
		t.Fatal("replied no: allowed")
	}

	got := s.lines()
	id := regexp.MustCompile(`\[[^\]]*\]`)
	for i := range got {
		got[i] = id.ReplaceAllString(got[i], "[id]")
	}
	wantLines(t, got,
		"4101 agent@discord [id] category=exec action=os_exec detail=uptime origin=discord",
		"4102 discord:42@discord [id] category=exec action=os_exec detail=uptime origin=discord grant=once via=operator nonce="+nonce,
		"4106 operator@tui broker=imessage category=exec mode=ask was=deny",
		"4101 agent@imessage [id] category=exec action=os_exec detail=uptime origin=imessage",
		"4103 operator@imessage [id] category=exec action=os_exec detail=uptime origin=imessage via=operator nonce="+inonce)
}

// A click on an external approval link names the clicker, in the Security
// log and in the marker; with the Security log failing under the deny
// policy, the approval is not given.
func TestExternalApprovalSecurityRecords(t *testing.T) {
	s := installSecSink(t)
	fs := &fakeSender{}
	r := &relay{dataDir: t.TempDir(), approver: newDiscordApprover(fs, nil)}
	click := func(custom string) {
		r.handleExtApprove(discord.Interaction{ID: "i", Token: "t", UserID: "42", CustomID: custom})
	}
	click("ext-approve:qi:job-1")
	click("ext-deny:qi:job-2")
	s.err = fmt.Errorf("%w: disk full", event.ErrRefused)
	click("ext-approve:qi:job-3")

	wantLines(t, s.lines(),
		"4102 discord:42@discord [ext:qi:job-1] category=external action=qi/job-1 origin=discord via=operator grant=once",
		"4103 discord:42@discord [ext:qi:job-2] category=external action=qi/job-2 origin=discord via=operator",
		"4102 discord:42@discord [ext:qi:job-3] category=external action=qi/job-3 origin=discord via=operator grant=once")
	dir := filepath.Join(r.dataDir, "approvals", "qi")
	if b, err := os.ReadFile(filepath.Join(dir, "job-1.approve")); err != nil || !strings.Contains(string(b), "by 42") {
		t.Errorf("job-1's marker = %q, %v; want it to name the clicker", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-3.approve")); !os.IsNotExist(err) {
		t.Errorf("an approval the Security log refused was given: %v", err)
	}
	if acks := fs.acks(); len(acks) != 3 || !strings.Contains(acks[2], "not given") {
		t.Errorf("acks = %q", acks)
	}
}

// Changing a category's mode or the relay's exec mode is recorded with the
// surface the operator changed it on, as every Security record's actor and
// surface are.
func TestModeChangeSecurityRecords(t *testing.T) {
	s := installSecSink(t)
	br := newDiscordApprovalBroker(testLog())
	m := &discordRelayManager{base: context.Background(), runner: &fakeRunner{}, broker: br}
	access := dashAccess{relay: m}
	if _, err := access.SetRelayPermission(catExec, "allow"); err != nil {
		t.Fatal(err)
	}
	if err := access.SetRelayExec(true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetExec(false); err != nil {
		t.Fatal(err)
	}
	cm := newCronManager(context.Background(), nil, &execGate{}, nil)
	if err := (dashCron{cm}).SetExec(true); err != nil {
		t.Fatal(err)
	}
	if err := (tuiCron{cm}).SetExec(false); err != nil {
		t.Fatal(err)
	}
	wantLines(t, s.lines(),
		"4106 operator@dashboard broker=discord category=exec mode=allow was=ask",
		"4204 operator@dashboard mode=on",
		"4204 operator@tui mode=off",
		"4704 operator@dashboard state=on", // TEN-398's cron-exec switch, attributed the same way
		"4704 operator@tui state=off")
}

// logs.security.on_failure is read from config.json each time a record
// fails, so an edit applies with no restart.
func TestAuditDeniesReadsConfigLive(t *testing.T) {
	dir := t.TempDir()
	deny := auditDenies(dir, logsConfig{})
	if deny() {
		t.Fatal("no config: deny, want continue")
	}
	lc := &launchConfig{}
	lc.Logs.Security.OnFailure = "deny"
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	if !deny() {
		t.Fatal("on_failure: deny in config.json: continue")
	}
}
