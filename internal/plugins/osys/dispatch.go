package osys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/model"
)

// Policy gates command execution — the dangerous capability. Read
// tools (sysinfo/read/list/processes) are always allowed once the
// plugin is enabled. Exec is OFF unless AllowExec; even then a command
// the classifier flags as catastrophic is denied unless Confirm
// approves it. With AllowExec off, Confirm (if set) can still approve
// individual commands. nil Confirm + AllowExec off ⇒ no exec at all.
type Policy struct {
	AllowExec  bool
	AllowWrite bool
	Confirm    func(ctx context.Context, action, detail string) bool
	// Roots bounds the FILE tools. Empty = unrestricted (historical behavior).
	// Deny is a hard boundary checked before any approval gate: an operator
	// saying yes to "write a file" is not saying yes to writing into System32.
	// NOTE: this does not bound os_exec — see paths.go.
	Roots Roots
}

// gateWrite gates the file-mutating tools (write/append/edit/mkdir). Mirrors
// gateExec: AllowWrite is the blanket grant; otherwise Confirm must approve.
// With neither, writes are blocked — a safety boundary, not a bug.
func (p Policy) gateWrite(ctx context.Context, action, detail string) error {
	if p.AllowWrite {
		return nil
	}
	if p.Confirm == nil {
		return fmt.Errorf("blocked: writing files is disabled — enable it (--os-allow-write)")
	}
	if p.Confirm(ctx, action, detail) {
		return nil
	}
	return errNotApproved("write")
}

// errNotApproved is a gated action the approval gate said no to: denied,
// timed out, or refused by a setting. Not "disabled": the operator may just
// have pressed Deny (UX-6).
func errNotApproved(what string) error {
	return fmt.Errorf("blocked: this %s wasn't approved (denied, timed out, or refused by a safety setting). "+
		"This is a safety boundary, not a bug", what)
}

func (p Policy) gateExec(ctx context.Context, command string) error {
	dangerous, reason := Classify(command)
	// Action id encodes the category for the approval broker: a destructive
	// command is "os_exec_dangerous" regardless of AllowExec, so it maps to
	// the destructive category and isn't blanket-approved by an exec grant.
	action, detail := "os_exec", command
	if dangerous {
		action, detail = "os_exec_dangerous", command+"  [DANGER: "+reason+"]"
	}
	// Fast path: a plainly-allowed, non-destructive command needs no asking.
	if p.AllowExec && !dangerous {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, action, detail) {
		return nil
	}
	if dangerous {
		return fmt.Errorf("blocked: this command looks destructive (%s) and was not approved. "+
			"This is a safety boundary, not a bug", reason)
	}
	if p.Confirm != nil {
		return errNotApproved("command")
	}
	return fmt.Errorf("blocked: command execution is disabled — enable it (--os-allow-exec). " +
		"Running shell commands is the highest-risk capability; off by default")
}

// Dispatcher implements agent.ToolDispatcher for the OS plugin.
type Dispatcher struct {
	svc    *Service
	policy Policy
}

func NewDispatcher(svc *Service, policy Policy) *Dispatcher {
	return &Dispatcher{svc: svc, policy: policy}
}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "os_sysinfo", Description: "Report this machine's OS, arch, hostname, CPU count, user, home and working directory.",
			Parameters: obj(``)},
		{Name: "os_read_file", Description: "Read a text file from this machine by absolute or relative path: lines from offset (1-based) for limit lines, default the whole file up to one page (~100K chars). A cut read ends with next_offset; read again from there for the rest.",
			Parameters: obj(`"path":{"type":"string"},"offset":{"type":"integer","description":"first line to read, 1-based (default 1)"},"limit":{"type":"integer","description":"how many lines (default: to the end of the page)"}`, "path"), ResultLabels: []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceLocalFile}}},
		{Name: "os_read_image", Description: "Inspect an image file (png/jpeg/gif): reports format and pixel dimensions.",
			Parameters: obj(`"path":{"type":"string"}`, "path")},
		{Name: "os_list_dir", Description: "List a directory's entries (type, size, name). Defaults to the current directory.",
			Parameters: obj(`"path":{"type":"string","description":"directory path (default '.')"}`)},
		{Name: "os_processes", Description: "List running processes on this machine.",
			Parameters: obj(``)},
		{Name: "os_write_file", Description: "Create or overwrite a text file with the given content (markdown, code, JSON, etc.). Missing parent folders are created. GATED: needs write permission or approval.",
			Parameters: obj(`"path":{"type":"string"},"content":{"type":"string"},"create_dirs":{"type":"boolean","description":"create missing parent folders (default true)"}`, "path", "content"), Gated: true},
		{Name: "os_append_file", Description: "Append text to a file, creating it (and parent folders) if absent. GATED: needs write permission or approval.",
			Parameters: obj(`"path":{"type":"string"},"content":{"type":"string"}`, "path", "content"), Gated: true},
		{Name: "os_edit_file", Description: "Edit an existing text file by replacing old_string with new_string. old_string must be unique unless replace_all is set. GATED: needs write permission or approval.",
			Parameters: obj(`"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}`, "path", "old_string", "new_string"), Gated: true},
		{Name: "os_make_dir", Description: "Create a directory (and any missing parents). GATED: needs write permission or approval.",
			Parameters: obj(`"path":{"type":"string"}`, "path"), Gated: true},
		{Name: "os_exec", Description: "Run a shell command on this machine (PowerShell on Windows, sh on Linux/macOS) and return its combined stdout+stderr; long output keeps its start and end. GATED: off unless enabled; destructive commands need explicit approval. " +
			"Use for builds, installs, git, package managers, processes and scripts. To read or change files prefer os_read_file / os_edit_file / os_write_file over cat, sed or heredocs. " +
			"There is no terminal: nothing can answer a prompt, so pass non-interactive flags (-y, --yes, --non-interactive, CI=1). sudo can't ask for a password; if a command needs it, ask the operator to run it. " +
			"A command still running at its timeout is moved to the background (see os_process) instead of being killed; background: true starts it there at once (servers, watchers).",
			Parameters: obj(`"command":{"type":"string"},"workdir":{"type":"string","description":"absolute directory to run in (default: the home folder)"},"timeout":{"type":"integer","description":"seconds, default 120, max 600"},"background":{"type":"boolean","description":"return at once with a process id instead of waiting"}`, "command"), Gated: true, Outbound: true},
		{Name: "os_process", Description: "The commands os_exec left running in the background: list; log (lines from offset for limit lines, default the last 200); wait (until it exits, up to timeout seconds, default 30); kill; write (one line to its stdin, for a y/n prompt). Logs page like os_read_file. GATED: kill and write need exec permission or approval.",
			Parameters: obj(`"action":{"type":"string","enum":["list","log","wait","kill","write"]},"id":{"type":"string","description":"the process id os_exec returned"},"offset":{"type":"integer"},"limit":{"type":"integer"},"timeout":{"type":"integer","description":"wait: seconds, default 30"},"input":{"type":"string","description":"write: the line to send"}`, "action"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "os_sysinfo":
		return d.svc.SysInfo(), false, nil
	case "os_read_file":
		return d.readFile(call.Arguments)
	case "os_read_image":
		return d.readImage(call.Arguments)
	case "os_list_dir":
		return d.listDir(call.Arguments)
	case "os_processes":
		out, err := d.svc.Processes(ctx)
		if err != nil {
			return "os_processes failed: " + err.Error(), true, nil
		}
		return out, false, nil
	case "os_write_file":
		return d.writeFile(ctx, call.Arguments)
	case "os_append_file":
		return d.appendFile(ctx, call.Arguments)
	case "os_edit_file":
		return d.editFile(ctx, call.Arguments)
	case "os_make_dir":
		return d.makeDir(ctx, call.Arguments)
	case "os_exec":
		return d.exec(ctx, call.Arguments)
	case "os_process":
		return d.process(ctx, call.Arguments)
	default:
		return "unknown os tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) readFile(args json.RawMessage) (string, bool, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	target, err := d.policy.Roots.Resolve(a.Path, false)
	if err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.ReadFile(target, a.Offset, a.Limit, d.policy.Roots.checkOpenedRead)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) readImage(args json.RawMessage) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	target, err := d.policy.Roots.Resolve(a.Path, false)
	if err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.ReadImage(target, d.policy.Roots.checkOpenedRead)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) writeFile(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		CreateDirs *bool  `json:"create_dirs"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	target, err := d.resolveWrite(ctx, a.Path)
	if err != nil {
		return err.Error(), true, nil
	}
	detail := "write " + a.Path
	if d.svc.Exists(target) {
		detail = "OVERWRITE existing file " + a.Path
	}
	if err := d.policy.gateWrite(ctx, "os_write", detail); err != nil {
		return err.Error(), true, nil
	}
	createDirs := true
	if a.CreateDirs != nil {
		createDirs = *a.CreateDirs
	}
	out, err := d.svc.WriteFile(target, a.Content, createDirs, d.policy.Roots.checkOpened)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) appendFile(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	target, err := d.resolveWrite(ctx, a.Path)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.policy.gateWrite(ctx, "os_append", "append to "+a.Path); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.AppendFile(target, a.Content, d.policy.Roots.checkOpened)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) editFile(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	target, err := d.resolveWrite(ctx, a.Path)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.policy.gateWrite(ctx, "os_edit", "edit "+a.Path); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.EditFile(target, a.OldString, a.NewString, a.ReplaceAll, d.policy.Roots.checkOpened)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) makeDir(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	target, err := d.resolveWrite(ctx, a.Path)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.policy.gateWrite(ctx, "os_mkdir", "create directory "+a.Path); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.MakeDir(target)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) listDir(args json.RawMessage) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	target, err := d.policy.Roots.Resolve(a.Path, false)
	if err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.ListDir(target)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

// resolveWrite resolves a path for writing. Outside the writable roots it asks
// (os_write_outside, destructive) rather than refusing; Deny, Protect and
// Secret stay hard refusals.
func (d *Dispatcher) resolveWrite(ctx context.Context, path string) (string, error) {
	target, err := d.policy.Roots.Resolve(path, true)
	var out *OutsideRootsError
	if !errors.As(err, &out) {
		return target, err
	}
	if d.policy.Confirm == nil || !d.policy.Confirm(ctx, "os_write_outside", "write "+out.Target+" (outside the writable roots: "+strings.Join(out.Roots, ", ")+")") {
		return "", err
	}
	return out.Target, nil
}

func (d *Dispatcher) exec(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Command    string `json:"command"`
		Workdir    string `json:"workdir"`
		Timeout    int    `json:"timeout"`    // seconds
		Background bool   `json:"background"` // start it and return at once (TEN-490)
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return "command is required", true, nil
	}
	if a.Workdir != "" {
		dir := a.Workdir
		if dir == "~" || strings.HasPrefix(dir, "~/") { // not ~user
			home, _ := os.UserHomeDir()
			dir = home + dir[1:]
		}
		if !filepath.IsAbs(dir) {
			return "workdir must be an absolute path (or start with ~)", true, nil
		}
		ctx = WithWorkDir(ctx, filepath.Clean(dir))
	}
	// The approval gateExec gets fills this slot, so the command's Security
	// record names the approval that let it run.
	ctx = event.WithApproval(ctx, "")
	if err := d.policy.gateExec(ctx, a.Command); err != nil {
		return err.Error(), true, nil
	}
	if a.Background {
		out, err := d.svc.Background(ctx, a.Command)
		if err != nil {
			return err.Error(), true, nil
		}
		return out, false, nil
	}
	out, err := d.svc.Exec(ctx, a.Command, time.Duration(a.Timeout)*time.Second)
	if err != nil {
		// Surface the output AND the error — a non-zero exit is signal,
		// not a tool failure.
		return fmt.Sprintf("%s\n[exit error: %v]", out, err), true, nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(no output)"
	}
	return out, false, nil
}

// process is os_process (TEN-490). list, log and wait only look; kill and
// write act on a process the operator's exec grant or approval let start,
// so they are gated like a command.
func (d *Dispatcher) process(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Action  string `json:"action"`
		ID      string `json:"id"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Timeout int    `json:"timeout"` // seconds
		Input   string `json:"input"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if a.Action != "list" && strings.TrimSpace(a.ID) == "" {
		return "id is required", true, nil
	}
	var out string
	var err error
	switch a.Action {
	case "list":
		return d.svc.ProcessList(), false, nil
	case "log":
		out, err = d.svc.ProcessLog(a.ID, a.Offset, a.Limit)
	case "wait":
		out, err = d.svc.ProcessWait(ctx, a.ID, time.Duration(a.Timeout)*time.Second)
	case "kill", "write":
		if !d.policy.AllowExec {
			// Gated as an os_exec: the same category, rules and surface locks as the
			// command that started the process.
			if d.policy.Confirm == nil || !d.policy.Confirm(ctx, "os_exec", "os_process "+a.Action+" "+a.ID) {
				return errNotApproved("command").Error(), true, nil
			}
		}
		if a.Action == "kill" {
			out, err = d.svc.ProcessKill(a.ID)
		} else {
			out, err = d.svc.ProcessWrite(a.ID, a.Input)
		}
	default:
		return "action must be list, log, wait, kill or write", true, nil
	}
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}
