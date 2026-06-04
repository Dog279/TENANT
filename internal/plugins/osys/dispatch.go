package osys

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
}

// gateWrite gates the file-mutating tools (write/append/edit/mkdir). Mirrors
// gateExec: AllowWrite is the blanket grant; otherwise Confirm must approve.
// With neither, writes are blocked — a safety boundary, not a bug.
func (p Policy) gateWrite(ctx context.Context, action, detail string) error {
	if p.AllowWrite {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, action, detail) {
		return nil
	}
	return fmt.Errorf("blocked: writing files is disabled — enable it (--os-allow-write) " +
		"or approve this write")
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
	return fmt.Errorf("blocked: command execution is disabled — enable it (--allow-exec) " +
		"or approve this command. Running shell commands is the highest-risk capability; off by default")
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
		{Name: "os_read_file", Description: "Read a text file from this machine by absolute or relative path (capped).",
			Parameters: obj(`"path":{"type":"string"}`, "path")},
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
		{Name: "os_exec", Description: "Run a shell command on this machine (PowerShell on Windows, sh on Linux/macOS). GATED: off unless enabled; destructive commands need explicit approval. Returns combined stdout+stderr.",
			Parameters: obj(`"command":{"type":"string"}`, "command"), Gated: true},
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
	default:
		return "unknown os tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) readFile(args json.RawMessage) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	out, err := d.svc.ReadFile(a.Path)
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
	out, err := d.svc.ReadImage(a.Path)
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
	detail := "write " + a.Path
	if d.svc.Exists(a.Path) {
		detail = "OVERWRITE existing file " + a.Path
	}
	if err := d.policy.gateWrite(ctx, "os_write", detail); err != nil {
		return err.Error(), true, nil
	}
	createDirs := true
	if a.CreateDirs != nil {
		createDirs = *a.CreateDirs
	}
	out, err := d.svc.WriteFile(a.Path, a.Content, createDirs)
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
	if err := d.policy.gateWrite(ctx, "os_append", "append to "+a.Path); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.AppendFile(a.Path, a.Content)
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
	if err := d.policy.gateWrite(ctx, "os_edit", "edit "+a.Path); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.EditFile(a.Path, a.OldString, a.NewString, a.ReplaceAll)
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
	if err := d.policy.gateWrite(ctx, "os_mkdir", "create directory "+a.Path); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.MakeDir(a.Path)
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
	out, err := d.svc.ListDir(a.Path)
	if err != nil {
		return err.Error(), true, nil
	}
	return out, false, nil
}

func (d *Dispatcher) exec(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return "command is required", true, nil
	}
	if err := d.policy.gateExec(ctx, a.Command); err != nil {
		return err.Error(), true, nil
	}
	out, err := d.svc.Exec(ctx, a.Command)
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
