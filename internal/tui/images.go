package tui

// Pasting images into the TUI (TEN-493): Ctrl+V reads the OS clipboard's
// image (Alt+V on Windows, where Windows Terminal keeps Ctrl+V), a pasted run
// of image file paths (a dragged file) attaches them, and /image <path>
// attaches by path. Each image is saved through the agent's attachment store
// and shows in the input as a removable [image N: name W×H] placeholder; the
// images whose placeholder is still in the text go with the message.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/attach"
	modelpkg "tenant/internal/model"
)

// Seams for tests.
var (
	clipGOOS        = runtime.GOOS
	runClipboardCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
)

// errNoClipboardTool means the OS has no tool Tenant can read an image with.
var errNoClipboardTool = errors.New("install wl-clipboard (Wayland) or xclip (X11) to paste images")

// imagesAttachedMsg carries images saved off the UI goroutine back to Update,
// with notes to show and the command to run after (a text paste when the
// clipboard held no image).
type imagesAttachedMsg struct {
	imgs  []modelpkg.Image
	notes []string
	then  tea.Cmd
}

// isImagePasteKey reports the keys that try the clipboard image first.
func isImagePasteKey(k string) bool {
	return k == "ctrl+v" || (k == "alt+v" && clipGOOS == "windows")
}

func (m *model) attachStore() *attach.Store {
	if m.cfg.Attachments != nil {
		return m.cfg.Attachments
	}
	if m.cfg.Agent == nil {
		return nil
	}
	return m.cfg.Agent.Attachments()
}

// pasteImageCmd reads the clipboard image and saves it, or falls back to
// bubbles' own text paste (what Ctrl+V did before) when there is none.
func (m *model) pasteImageCmd() tea.Cmd {
	store := m.attachStore()
	ctx := m.ctx
	return func() tea.Msg {
		if store == nil {
			return textarea.Paste()
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		data, err := clipboardImage(cctx)
		if err != nil {
			return imagesAttachedMsg{notes: []string{"image paste: " + err.Error()}, then: textarea.Paste}
		}
		if data == nil {
			return textarea.Paste()
		}
		img, err := store.Add(cctx, data, "clipboard")
		if err != nil {
			return imagesAttachedMsg{notes: []string{"image paste: " + err.Error()}}
		}
		return imagesAttachedMsg{imgs: []modelpkg.Image{img}}
	}
}

// attachFilesCmd reads and saves image files the operator named (dragged in,
// or /image), off the UI goroutine.
func (m *model) attachFilesCmd(paths []string) tea.Cmd {
	store := m.attachStore()
	ctx := m.ctx
	return func() tea.Msg {
		if store == nil {
			return imagesAttachedMsg{notes: []string{"images can't be attached in this session"}}
		}
		var out imagesAttachedMsg
		for _, p := range paths {
			data, err := readImageFile(p)
			if err == nil {
				var img modelpkg.Image
				if img, err = store.Add(ctx, data, filepath.Base(p)); err == nil {
					out.imgs = append(out.imgs, img)
					continue
				}
			}
			out.notes = append(out.notes, fmt.Sprintf("%s: %v", filepath.Base(p), err))
		}
		return out
	}
}

// readImageFile reads a regular file of at most the store's source limit.
func readImageFile(p string) ([]byte, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a file")
	}
	if fi.Size() > attach.MaxSourceBytes {
		return nil, attach.ErrTooLarge
	}
	return os.ReadFile(p)
}

// addAttachments inserts a placeholder per image at the cursor. Placeholder
// text doesn't count against the input's character limit.
func (m *model) addAttachments(imgs []modelpkg.Image) {
	for _, img := range imgs {
		m.attachments = append(m.attachments, img)
		ph := placeholder(len(m.attachments), img)
		m.ta.CharLimit += len(ph) + 1
		m.ta.InsertString(ph + " ")
	}
}

// placeholder is how attachment n shows in the input and in the sent text.
func placeholder(n int, img modelpkg.Image) string {
	return fmt.Sprintf("[image %d: %s]", n, img.Label())
}

// takeAttachments returns the attachments whose placeholder is still in q,
// in order, and clears the list (a deleted placeholder drops its image).
func (m *model) takeAttachments(q string) []modelpkg.Image {
	var out []modelpkg.Image
	for i, img := range m.attachments {
		if strings.Contains(q, placeholder(i+1, img)) {
			out = append(out, img)
		}
	}
	m.attachments = nil
	return out
}

// pastedImagePaths returns the image files a bracketed paste names, when the
// paste is nothing but existing image file paths (a dragged file); otherwise
// nil, and the paste stays text.
func pastedImagePaths(s string) []string {
	toks := splitPastedPaths(s, clipGOOS == "windows")
	if len(toks) == 0 {
		return nil
	}
	var out []string
	for _, t := range toks {
		if strings.HasPrefix(t, "file://") {
			u, err := url.Parse(t)
			if err != nil {
				return nil
			}
			t = u.Path
			if clipGOOS == "windows" {
				t = strings.TrimPrefix(filepath.FromSlash(t), `\`)
			}
		}
		if strings.HasPrefix(t, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				t = filepath.Join(home, t[2:])
			}
		}
		if !isImageFile(t) {
			return nil
		}
		out = append(out, t)
	}
	return out
}

// splitPastedPaths splits a paste into path tokens: whitespace separates
// them, quotes group them, and outside Windows a backslash escapes the next
// character (macOS terminals drag "my\ file.png"). On Windows a backslash is
// a path separator.
func splitPastedPaths(s string, windows bool) []string {
	var toks []string
	var cur strings.Builder
	var quote rune
	inTok := false
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
		}
		cur.Reset()
		inTok = false
	}
	rs := []rune(strings.TrimSpace(s))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inTok = r, true
		case r == '\\' && !windows && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if quote != 0 {
		return nil // unbalanced quote: not a path list
	}
	flush()
	return toks
}

// isImageFile reports an existing regular file whose content sniffs as an
// image Tenant takes.
func isImageFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return attach.Sniff(head[:n]) != ""
}

// clipboardImage returns the clipboard's image as PNG (or JPEG/TIFF-converted
// PNG) bytes, nil when the clipboard holds no image, or an error when the OS
// has no tool to read it.
func clipboardImage(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "tenant-clip-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	switch clipGOOS {
	case "darwin":
		return clipboardImageMac(ctx, dir)
	case "windows":
		return clipboardImageWindows(ctx, dir)
	default:
		return clipboardImageLinux(ctx)
	}
}

// macClipScript writes the clipboard's PNG (or, failing that, TIFF: what
// Preview and some apps copy) to the file named by its argument, and prints
// png, tiff or none.
const macClipScript = `on run argv
	set p to POSIX file (item 1 of argv)
	try
		set d to the clipboard as «class PNGf»
		set k to "png"
	on error
		try
			set d to the clipboard as «class TIFF»
			set k to "tiff"
		on error
			return "none"
		end try
	end try
	set f to open for access p with write permission
	set eof f to 0
	write d to f
	close access f
	return k
end run`

func clipboardImageMac(ctx context.Context, dir string) ([]byte, error) {
	out := filepath.Join(dir, "clip")
	res, err := runClipboardCmd(ctx, "osascript", "-e", macClipScript, out)
	if err != nil {
		return nil, fmt.Errorf("osascript: %v", err)
	}
	switch strings.TrimSpace(string(res)) {
	case "png":
		return os.ReadFile(out)
	case "tiff":
		png := filepath.Join(dir, "clip.png")
		if _, err := runClipboardCmd(ctx, "sips", "-s", "format", "png", out, "--out", png); err != nil {
			return nil, fmt.Errorf("sips: %v", err)
		}
		return os.ReadFile(png)
	}
	return nil, nil
}

func clipboardImageWindows(ctx context.Context, dir string) ([]byte, error) {
	out := filepath.Join(dir, "clip.png")
	script := "Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; " +
		"$i = [System.Windows.Forms.Clipboard]::GetImage(); " +
		"if ($i) { $i.Save('" + strings.ReplaceAll(out, "'", "''") + "', [System.Drawing.Imaging.ImageFormat]::Png); 'ok' } else { 'none' }"
	res, err := runClipboardCmd(ctx, "powershell", "-NoProfile", "-STA", "-Command", script)
	if err != nil {
		return nil, fmt.Errorf("powershell: %v", err)
	}
	if strings.TrimSpace(string(res)) != "ok" {
		return nil, nil
	}
	return os.ReadFile(out)
}

func clipboardImageLinux(ctx context.Context) ([]byte, error) {
	type tool struct{ list, get []string }
	var tools []tool
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		tools = append(tools, tool{[]string{"wl-paste", "--list-types"}, []string{"wl-paste", "--type", "image/png"}})
	}
	tools = append(tools, tool{
		[]string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"},
		[]string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
	})
	missing := 0
	for _, t := range tools {
		types, err := runClipboardCmd(ctx, t.list[0], t.list[1:]...)
		if errors.Is(err, exec.ErrNotFound) {
			missing++
			continue
		}
		if err != nil || !bytes.Contains(types, []byte("image/png")) {
			return nil, nil
		}
		return runClipboardCmd(ctx, t.get[0], t.get[1:]...)
	}
	if missing == len(tools) {
		return nil, errNoClipboardTool
	}
	return nil, nil
}

// slashImage runs /image <path>: attach an image file to the next message.
func slashImage(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	paths := splitPastedPaths(arg, clipGOOS == "windows")
	if len(paths) == 0 {
		m.sysChat("usage: /image <path>   (or press Ctrl+V to paste an image; Alt+V on Windows)")
		return nil
	}
	m.ta.Reset()
	return m.attachFilesCmd(paths)
}
