package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/attach"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func imageModel(t *testing.T) *model {
	t.Helper()
	m := newModel(context.Background(), Config{Attachments: attach.NewStore(t.TempDir())})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

// fakeMacClipboard makes osascript write pic (or report none).
func fakeMacClipboard(t *testing.T, pic []byte) {
	t.Helper()
	oldOS, oldRun := clipGOOS, runClipboardCmd
	t.Cleanup(func() { clipGOOS, runClipboardCmd = oldOS, oldRun })
	clipGOOS = "darwin"
	runClipboardCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if pic == nil {
			return []byte("none\n"), nil
		}
		return []byte("png\n"), os.WriteFile(args[len(args)-1], pic, 0o600)
	}
}

// TEN-493: Ctrl+V with an image on the clipboard inserts a placeholder (the
// read and save run as a command, off the UI goroutine), and the image goes
// with the message while its placeholder is in the text.
func TestCtrlV_PastesClipboardImage(t *testing.T) {
	fakeMacClipboard(t, pngBytes(t, 30, 20))
	m := imageModel(t)
	m.ta.SetValue("what is this? ")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	if cmd == nil {
		t.Fatal("ctrl+v returned no command")
	}
	msg := cmd()
	if _, ok := msg.(imagesAttachedMsg); !ok {
		t.Fatalf("ctrl+v with an image produced %T", msg)
	}
	m.Update(msg)
	if !strings.Contains(m.ta.Value(), "[image 1: clipboard 30×20]") || len(m.attachments) != 1 {
		t.Fatalf("input = %q, %d attachments", m.ta.Value(), len(m.attachments))
	}
	if imgs := m.takeAttachments(m.ta.Value()); len(imgs) != 1 || m.attachments != nil {
		t.Fatalf("took %d images, left %d", len(imgs), len(m.attachments))
	}
}

// With no image on the clipboard, Ctrl+V is bubbles' text paste, as before.
func TestCtrlV_NoImageIsTextPaste(t *testing.T) {
	fakeMacClipboard(t, nil)
	m := imageModel(t)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	msg := cmd()
	if _, ok := msg.(imagesAttachedMsg); ok {
		t.Fatal("no image on the clipboard, yet an image was attached")
	}
	// It is whatever textarea.Paste returns (its unexported paste message).
	if want := textarea.Paste(); reflectType(msg) != reflectType(want) {
		t.Fatalf("got %T, want bubbles' paste message %T", msg, want)
	}
}

func reflectType(v any) string { return fmt.Sprintf("%T", v) }

// Alt+V reads the clipboard image only on Windows (Option+V types "√" on a Mac).
func TestAltV_WindowsOnly(t *testing.T) {
	old := clipGOOS
	t.Cleanup(func() { clipGOOS = old })
	clipGOOS = "darwin"
	if isImagePasteKey("alt+v") {
		t.Error("alt+v pastes images on macOS")
	}
	clipGOOS = "windows"
	if !isImagePasteKey("alt+v") || !isImagePasteKey("ctrl+v") {
		t.Error("windows should take alt+v and ctrl+v")
	}
}

// A paste of image file paths (a dragged file) attaches them; anything mixed
// stays text.
func TestPastedImagePaths(t *testing.T) {
	old := clipGOOS
	t.Cleanup(func() { clipGOOS = old })
	clipGOOS = runtime.GOOS // real paths, so parse them the way this host's terminal pastes them
	dir := t.TempDir()
	a := filepath.Join(dir, "a b.png")
	c := filepath.Join(dir, "c.png")
	txt := filepath.Join(dir, "notes.txt")
	for _, p := range []string{a, c} {
		if err := os.WriteFile(p, pngBytes(t, 4, 4), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(txt, []byte("hello"), 0o600)
	fileURL := filepath.ToSlash(c)
	if !strings.HasPrefix(fileURL, "/") {
		fileURL = "/" + fileURL // file:///C:/… on Windows
	}
	cases := map[string]struct {
		paste string
		want  int
	}{
		"quoted":          {`'` + a + `'`, 1},
		"two paths":       {`"` + a + `" ` + c, 2},
		"file url":        {"file://" + strings.ReplaceAll(fileURL, " ", "%20"), 1},
		"not an image":    {txt, 0},
		"mixed with text": {"look at " + c, 0},
		"plain text":      {"hello world", 0},
	}
	if runtime.GOOS != "windows" { // a backslash escapes only off Windows
		cases["escaped space"] = struct {
			paste string
			want  int
		}{strings.ReplaceAll(a, " ", `\ `), 1}
	}
	for name, c := range cases {
		if got := pastedImagePaths(c.paste); len(got) != c.want {
			t.Errorf("%s: %q → %v, want %d paths", name, c.paste, got, c.want)
		}
	}
}

// On Windows a backslash is a path separator, not an escape.
func TestSplitPastedPaths_Windows(t *testing.T) {
	if got := splitPastedPaths(`"C:\Users\Pat\My Pictures\a.png"`, true); len(got) != 1 || got[0] != `C:\Users\Pat\My Pictures\a.png` {
		t.Fatalf("windows path split = %q", got)
	}
}

// A deleted placeholder drops its image; a placeholder-only message carries
// its image.
func TestTakeAttachments(t *testing.T) {
	m := imageModel(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "x.png")
	_ = os.WriteFile(p, pngBytes(t, 8, 8), 0o600)
	msg := m.attachFilesCmd([]string{p, p})()
	m.Update(msg)
	if len(m.attachments) != 2 {
		t.Fatalf("%d attachments, want 2", len(m.attachments))
	}
	q := placeholder(2, m.attachments[1]) // the first placeholder was deleted
	if got := m.takeAttachments(q); len(got) != 1 {
		t.Fatalf("took %d, want only the image whose placeholder remains", len(got))
	}
}

// /image attaches a file by path.
func TestSlashImage(t *testing.T) {
	m := imageModel(t)
	p := filepath.Join(t.TempDir(), "y.png")
	_ = os.WriteFile(p, pngBytes(t, 8, 8), 0o600)
	cmd := slashImage(m, "/image "+p)
	if cmd == nil {
		t.Fatal("no command")
	}
	m.Update(cmd())
	if len(m.attachments) != 1 || !strings.Contains(m.ta.Value(), "[image 1: y.png 8×8]") {
		t.Fatalf("input %q, %d attachments", m.ta.Value(), len(m.attachments))
	}
}

func TestClipboardImageLinux(t *testing.T) {
	oldOS, oldRun := clipGOOS, runClipboardCmd
	t.Cleanup(func() { clipGOOS, runClipboardCmd = oldOS, oldRun })
	t.Setenv("WAYLAND_DISPLAY", "")
	clipGOOS = "linux"
	runClipboardCmd = func(context.Context, string, ...string) ([]byte, error) { return nil, exec.ErrNotFound }
	if _, err := clipboardImage(context.Background()); err != errNoClipboardTool {
		t.Fatalf("no xclip: err = %v, want the install hint", err)
	}
	pic := pngBytes(t, 4, 4)
	runClipboardCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "TARGETS") {
			return []byte("TARGETS\nimage/png\n"), nil
		}
		return pic, nil
	}
	if got, err := clipboardImage(context.Background()); err != nil || !bytes.Equal(got, pic) {
		t.Fatalf("xclip image = %d bytes, %v", len(got), err)
	}
}

// A pasted path is judged by extension and stat only (no read on the UI
// goroutine); a .png that isn't one becomes a note when it is saved.
func TestPastedFakePNGBecomesANote(t *testing.T) {
	m := imageModel(t)
	fake := filepath.Join(t.TempDir(), "fake.png")
	_ = os.WriteFile(fake, []byte("not a picture"), 0o600)
	if got := pastedImagePaths(fake); len(got) != 1 {
		t.Fatalf("pastedImagePaths(%q) = %v", fake, got)
	}
	msg, ok := m.attachFilesCmd([]string{fake})().(imagesAttachedMsg)
	if !ok || len(msg.imgs) != 0 || len(msg.notes) != 1 {
		t.Fatalf("attach = %+v, want one note and no image", msg)
	}
}

// Placeholders raise the input limit only until the message is sent.
func TestCharLimitComesBack(t *testing.T) {
	m := imageModel(t)
	p := filepath.Join(t.TempDir(), "x.png")
	_ = os.WriteFile(p, pngBytes(t, 8, 8), 0o600)
	m.Update(m.attachFilesCmd([]string{p})())
	if m.ta.CharLimit <= inputCharLimit {
		t.Fatalf("limit %d not raised for the placeholder", m.ta.CharLimit)
	}
	m.takeAttachments("")
	if m.ta.CharLimit != inputCharLimit {
		t.Fatalf("limit %d after send, want %d", m.ta.CharLimit, inputCharLimit)
	}
}

// Linux: a JPEG-only clipboard is read as JPEG, and a wl-paste that fails
// (no compositor clipboard) falls back to xclip.
func TestClipboardImageLinux_JPEGAndFallback(t *testing.T) {
	oldOS, oldRun := clipGOOS, runClipboardCmd
	t.Cleanup(func() { clipGOOS, runClipboardCmd = oldOS, oldRun })
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	clipGOOS = "linux"
	var got []string
	runClipboardCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		got = append(got, call)
		switch {
		case name == "wl-paste":
			return nil, errors.New("no clipboard")
		case strings.Contains(call, "TARGETS"):
			return []byte("TARGETS\nimage/jpeg\n"), nil
		}
		return []byte("jpeg bytes"), nil
	}
	b, err := clipboardImage(context.Background())
	if err != nil || string(b) != "jpeg bytes" {
		t.Fatalf("= %q, %v (calls %q)", b, err, got)
	}
	if last := got[len(got)-1]; !strings.Contains(last, "xclip") || !strings.Contains(last, "image/jpeg") {
		t.Fatalf("last call %q, want xclip reading image/jpeg", last)
	}
}
