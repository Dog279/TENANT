//go:build windows

package main

import (
	"slices"
	"testing"
)

// The scheduled tasks that start Tenant are read out of the task list as
// schtasks prints it: by the command, or by the arguments of a task that
// starts Tenant through something else (TEN-556: the R340's two tasks).
func TestTasksNaming(t *testing.T) {
	const listing = `<?xml version="1.0" encoding="UTF-16"?>
<Tasks>

<!-- \Launch Adobe CCXProcess -->

<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Actions Context="Author">
    <Exec>
      <Command>"C:\Program Files\Adobe\CCXProcess.exe"</Command>
    </Exec>
  </Actions>
</Task>

<!-- \TenantServe -->

<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Actions Context="Author">
    <Exec>
      <Command>C:\Tenant\tenant.exe</Command>
      <Arguments>serve --config &quot;C:\Tenant\cfg&quot; --hidden</Arguments>
    </Exec>
  </Actions>
</Task>

<!-- \TenantMCP -->

<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Actions Context="Author">
    <Exec>
      <Command>"c:\tenant\TENANT.EXE"</Command>
    </Exec>
  </Actions>
</Task>

<!-- \Wrapped -->

<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Actions Context="Author">
    <Exec>
      <Command>powershell.exe</Command>
      <Arguments>-Command &amp; 'C:\Tenant\tenant.exe' mcp-memory</Arguments>
    </Exec>
  </Actions>
</Task>

<!-- \Other\tenant.exe elsewhere -->

<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Actions Context="Author">
    <Exec>
      <Command>D:\Other\tenant.exe</Command>
      <Arguments>serve</Arguments>
    </Exec>
  </Actions>
</Task>
</Tasks>`
	got := tasksNaming(listing, `C:\Tenant\tenant.exe`)
	want := []string{
		`\TenantServe  serve --config "C:\Tenant\cfg" --hidden`,
		`\TenantMCP`,
		`\Wrapped  powershell.exe -Command & 'C:\Tenant\tenant.exe' mcp-memory`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := tasksNaming("", `C:\Tenant\tenant.exe`); len(got) != 0 {
		t.Errorf("an empty listing: %q", got)
	}
}
