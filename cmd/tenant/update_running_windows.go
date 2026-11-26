//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processesOf lists the other processes whose program is the file at exe. A
// process of another account that this one may not open is not seen. Windows
// shows another process's command line only to who may read its memory, which
// a protected Tenant refuses (TEN-535), so a process is its number alone.
func processesOf(exe string) (out []string) {
	want, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ProcessID == windows.GetCurrentProcessId() || !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), filepath.Base(exe)) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			continue
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		err = windows.QueryFullProcessImageName(h, 0, &buf[0], &n)
		windows.CloseHandle(h)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(windows.UTF16ToString(buf[:n])); err == nil && os.SameFile(want, fi) {
			out = append(out, fmt.Sprintf("pid %d", pe.ProcessID))
		}
	}
	return out
}

// tasksOf lists the scheduled tasks this account can see whose action names
// the file at exe, as the task's name and its arguments.
func tasksOf(ctx context.Context, exe string) []string {
	out, err := runCmd(ctx, "schtasks", "/Query", "/XML", "ONE")
	if err != nil {
		return nil
	}
	return tasksNaming(out, exe)
}

// tasksNaming reads `schtasks /Query /XML ONE`: each task is a comment with
// its name, then its definition. A task counts when its command or its
// arguments name exe, so one that starts Tenant through a script's
// interpreter is found as well.
func tasksNaming(listing, exe string) (tasks []string) {
	exe = strings.ToLower(exe)
	name, command, counted := "", "", false
	text := func(line, tag string) (string, bool) {
		s, ok := strings.CutPrefix(strings.TrimSpace(line), "<"+tag+">")
		if !ok {
			return "", false
		}
		s, _, _ = strings.Cut(s, "</"+tag+">")
		r := strings.NewReplacer("&quot;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&apos;", "'")
		return r.Replace(s), true
	}
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if n, ok := strings.CutPrefix(line, "<!-- "); ok {
			name, command, counted = strings.TrimSuffix(n, " -->"), "", false
			continue
		}
		if c, ok := text(line, "Command"); ok {
			command = c
			if strings.Contains(strings.ToLower(c), exe) && !counted {
				tasks, counted = append(tasks, name), true
			}
			continue
		}
		if a, ok := text(line, "Arguments"); ok {
			switch {
			case counted && strings.Contains(strings.ToLower(command), exe):
				tasks[len(tasks)-1] = name + "  " + a // the task's own arguments: serve, mcp-memory, ...
			case !counted && strings.Contains(strings.ToLower(a), exe):
				tasks, counted = append(tasks, name+"  "+strings.Trim(command, `"`)+" "+a), true
			}
		}
	}
	return tasks
}
