package toolmux

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"tenant/internal/model"
	"tenant/internal/procprotect"
)

// envToolWrites names the file a protected copy of this test binary is to
// write through a tool.
const envToolWrites = "TOOLMUX_TEST_TOOL_WRITES"

type writeTool struct{}

func (writeTool) Tools() []model.ToolSpec { return []model.ToolSpec{{Name: "write"}} }

func (writeTool) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	var path string
	if err := json.Unmarshal(call.Arguments, &path); err != nil {
		return "", true, err
	}
	return "written", false, os.WriteFile(path, []byte("x"), 0o644)
}

// A file a tool makes is the user's to read wherever it is made: also in a
// folder that passes no permissions on, where a new file takes the default
// of the thread that makes it, and a protected Tenant's default is the list
// for its threads (TEN-535 hardening). The tool runs in a second process,
// which protects itself first, on a goroutine that is not main's.
func TestAToolsFileIsItsOwnersToRead(t *testing.T) {
	if path := os.Getenv(envToolWrites); path != "" { // the protected copy
		// Nothing in the registry, and no console to leave: neither is what
		// this is about.
		procprotect.ForceFail("windows.wer-exclusion", "forced by the test")
		procprotect.ForceFail("windows.console-host", "forced by the test")
		if missing := procprotect.Protect().Missing(); len(missing) != 2 {
			t.Fatalf("Protect: not in force: %+v", missing)
		}
		m := New()
		m.Add("test", writeTool{})
		args, _ := json.Marshal(path)
		if out, isErr, err := m.Dispatch(context.Background(), model.ToolCall{Name: "write", Arguments: args}); err != nil || isErr {
			t.Fatalf("the tool: %q, %v", out, err)
		}
		return
	}
	if procprotect.Disabled {
		t.Skip("built with -tags debugattach: the controls are compiled out")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(t.TempDir(), "no-inherit")
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(folder), &sa); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(folder, "written-by-a-tool.txt")
	c := exec.Command(os.Args[0], "-test.run=^TestAToolsFileIsItsOwnersToRead$")
	c.Env = append(os.Environ(), envToolWrites+"="+file)
	if out, err := procprotect.CombinedOutput(c); err != nil {
		t.Fatalf("the protected copy: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "x" {
		t.Errorf("the file a tool wrote, read by its owner: %q, %v", b, err)
	}
}
