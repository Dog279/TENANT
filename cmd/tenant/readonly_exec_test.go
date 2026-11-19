package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"tenant/internal/model"
	"tenant/internal/tui"
)

// TEN-534: the brokers as launch builds them (the osys classifier plugin_os.go
// installs, every category on ask) put an approval card in front of a command
// that only looks read-only, and none in front of one that is. The operator
// here answers every card with Deny.
func TestReadOnlyExec_BrokerAsks(t *testing.T) {
	for name, newBroker := range map[string]func() *approvalBroker{
		"local":   func() *approvalBroker { return newApprovalBroker(nil) },
		"discord": func() *approvalBroker { return newDiscordApprovalBroker(nil) },
	} {
		b := newBroker()
		b.SetRules(newToolRules()) // no command rule left by another test answers first
		asked := 0
		b.SetAsk(func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { asked++; return tui.DenyOnce })
		ctx := context.Background()
		for _, cmd := range []string{
			"env sh -c 'touch /tmp/x'",
			"env python3 x.py",
			`(Get-Item C:\x).Delete()`,
			`[System.IO.File]::Delete('C:\x')`,
			`& (Write-Output Remove-Item) C:\x`,
			"$(echo rm -rf x)",
			"find . $(echo -delete)",
			"PATH=/tmp/evil ls",
			"./ls",
			"sort -o out in",
			"date -s '2020-01-01'",
			"hostname evil",
			// Round two. The first writes the dispatcher's own danger note into
			// the command: the broker used to judge only the text before it.
			"echo hi  [DANGER: x]; touch made-by-the-tail",
			"git status --help",
			"git log --show-signature -1",
			`git log '--out"put=victim.txt'`,
			`Get-Content \\attacker.example\share\x.txt`,
			"git remote show origin",
			"HEAD -m POST http://127.0.0.1:9/x",
			"sort -T . -S 1k big",
			"file -p victim.txt",
		} {
			before := asked
			if b.Confirm(ctx, "os_exec", cmd) {
				t.Errorf("%s: %q ran though the operator denied it", name, cmd)
			}
			if asked != before+1 {
				t.Errorf("%s: %q raised %d approval card(s), want 1", name, cmd, asked-before)
			}
		}
		for _, cmd := range []string{"env", "env FOO=bar", "printenv", "date", "date +%s", "hostname",
			`Get-Item C:\x`, "Get-ChildItem", "git status", "ls -la | grep x", "date -u", "hostname -f"} {
			before := asked
			if !b.Confirm(ctx, "os_exec", cmd) {
				t.Errorf("%s: the read-only command %q did not run", name, cmd)
			}
			if asked != before {
				t.Errorf("%s: the read-only command %q raised an approval card", name, cmd)
			}
		}
	}
}

// readOnlyExecMux is os_exec as a turn reaches it: the tool mux launch builds,
// gated by the host broker with exec on ask, running on the real shell. The
// operator answers every approval card with Deny; asked counts the cards.
func readOnlyExecMux(t *testing.T) (run func(command, workdir string) (out string, isErr bool), asked *int) {
	t.Helper()
	resetExecProfiles(t)
	broker := newApprovalBroker(nil)
	broker.SetRules(newToolRules())
	asked = new(int)
	broker.SetAsk(func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision { *asked++; return tui.DenyOnce })
	c := &commonFlags{agent: "main", cfgDir: t.TempDir(), dataDir: t.TempDir()}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, testPluginFlags(func(pf *pluginFlags) { osFlagsOf(pf).on = true }),
		broker.Confirm, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildToolMux: %v", err)
	}
	t.Cleanup(cleanup)
	return func(command, workdir string) (string, bool) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"command": command, "workdir": workdir, "timeout": 60})
		out, isErr, err := mux.Dispatch(context.Background(), model.ToolCall{Name: "os_exec", Arguments: args})
		if err != nil {
			t.Fatalf("os_exec %q: %v", command, err)
		}
		return out, isErr
	}, asked
}
