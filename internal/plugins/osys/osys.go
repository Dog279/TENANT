package osys

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"  // register decoders for DecodeConfig
	_ "image/jpeg" // register decoders for DecodeConfig
	_ "image/png"  // register decoders for DecodeConfig
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	defaultExecTimeout = 60 * time.Second
	maxOutputBytes     = 16000 // cap tool output fed back to the model
	maxFileBytes       = 64000
	maxDirEntries      = 500
	maxWriteBytes      = 4 << 20 // 4MB cap on a single write
)

// runner executes a shell command and returns combined stdout+stderr.
// Injectable so tests don't spawn real processes.
type runner func(ctx context.Context, command string, timeout time.Duration) (string, error)

// Config opens a Service.
type Config struct {
	ExecTimeout time.Duration // 0 ⇒ 60s
}

// Service is the opened OS connector.
type Service struct {
	run     runner
	timeout time.Duration
}

// Open builds a Service backed by the real OS shell.
func Open(cfg Config) (*Service, error) {
	to := cfg.ExecTimeout
	if to <= 0 {
		to = defaultExecTimeout
	}
	return &Service{run: realRunner, timeout: to}, nil
}

// shellArgs picks the platform shell. PowerShell on Windows, sh on
// Unix. Non-interactive, no profile, so it behaves predictably.
func shellArgs(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	return "sh", []string{"-c", command}
}

func realRunner(ctx context.Context, command string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name, args := shellArgs(command)
	out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("command timed out after %s", timeout)
	}
	return string(out), err
}

// Exec runs a shell command (the dispatcher must have gated it) and
// returns combined output (capped). A non-zero exit is returned as
// output + error, not swallowed.
func (s *Service) Exec(ctx context.Context, command string) (string, error) {
	out, err := s.run(ctx, command, s.timeout)
	out = clip(out, maxOutputBytes)
	if err != nil {
		return out, err
	}
	return out, nil
}

// SysInfo returns a readable summary of the host.
func (s *Service) SysInfo() string {
	var b strings.Builder
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	uname := ""
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}
	home, _ := os.UserHomeDir()
	fmt.Fprintf(&b, "os: %s\narch: %s\nhostname: %s\ncpus: %d\nuser: %s\nhome: %s\ncwd: %s\ngo: %s",
		runtime.GOOS, runtime.GOARCH, host, runtime.NumCPU(), uname, home, cwd, runtime.Version())
	return b.String()
}

// ReadFile returns a file's contents (capped). Path is whatever the
// operator's machine exposes — enabling this plugin is the consent.
func (s *Service) ReadFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: file path required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("osys: read %s: %w", path, err)
	}
	return clip(string(b), maxFileBytes), nil
}

// Exists reports whether a path already exists (used to flag overwrites).
func (s *Service) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WriteFile creates or overwrites a text file. When createDirs is true, any
// missing parent directories are created. Returns a short confirmation.
func (s *Service) WriteFile(path, content string, createDirs bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: file path required")
	}
	if len(content) > maxWriteBytes {
		return "", fmt.Errorf("osys: content too large (%d bytes, max %d)", len(content), maxWriteBytes)
	}
	existed := s.Exists(path)
	if createDirs {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("osys: mkdir %s: %w", dir, err)
			}
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("osys: write %s: %w", path, err)
	}
	verb := "created"
	if existed {
		verb = "overwrote"
	}
	return fmt.Sprintf("%s %s (%d bytes)", verb, path, len(content)), nil
}

// AppendFile appends content to a file, creating it (and parents) if absent.
func (s *Service) AppendFile(path, content string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: file path required")
	}
	if len(content) > maxWriteBytes {
		return "", fmt.Errorf("osys: content too large (%d bytes, max %d)", len(content), maxWriteBytes)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("osys: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("osys: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("osys: append %s: %w", path, err)
	}
	return fmt.Sprintf("appended %d bytes to %s", len(content), path), nil
}

// EditFile replaces occurrences of oldStr with newStr in an existing file.
// With replaceAll false, oldStr must be unique (a safety check that mirrors
// the editor's exact-match contract) — otherwise it errors rather than guess.
func (s *Service) EditFile(path, oldStr, newStr string, replaceAll bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: file path required")
	}
	if oldStr == "" {
		return "", fmt.Errorf("osys: old_string required (use os_write_file to create a file)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("osys: read %s: %w", path, err)
	}
	content := string(b)
	n := strings.Count(content, oldStr)
	if n == 0 {
		return "", fmt.Errorf("osys: old_string not found in %s", path)
	}
	if n > 1 && !replaceAll {
		return "", fmt.Errorf("osys: old_string appears %d times in %s — make it unique or set replace_all", n, path)
	}
	if replaceAll {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		content = strings.Replace(content, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("osys: write %s: %w", path, err)
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", path, n), nil
}

// MakeDir creates a directory (and any missing parents).
func (s *Service) MakeDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: directory path required")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("osys: mkdir %s: %w", path, err)
	}
	return "created directory " + path, nil
}

// ReadImage reports an image's format and dimensions without decoding the
// full pixel data. A text LLM can't "see" pixels, but this lets it reason
// about images it's working with (and confirm a file is a valid image).
func (s *Service) ReadImage(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: image path required")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("osys: open %s: %w", path, err)
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return "", fmt.Errorf("osys: %s is not a decodable image (png/jpeg/gif): %w", path, err)
	}
	size := int64(0)
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	return fmt.Sprintf("image %s: format=%s width=%d height=%d size=%d bytes",
		path, format, cfg.Width, cfg.Height, size), nil
}

// ListDir lists a directory's entries (capped, sorted).
func (s *Service) ListDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("osys: list %s: %w", path, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var b strings.Builder
	fmt.Fprintf(&b, "%d entr(ies) in %s:\n", len(entries), path)
	shown := 0
	for _, e := range entries {
		if shown >= maxDirEntries {
			fmt.Fprintf(&b, "…[truncated at %d entries]\n", maxDirEntries)
			break
		}
		kind := "f"
		size := int64(0)
		if e.IsDir() {
			kind = "d"
		} else if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		fmt.Fprintf(&b, "%s %10d  %s\n", kind, size, e.Name())
		shown++
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// Processes lists running processes. Runs a FIXED, benign command (no
// model input, no shell) so it's safe regardless of the exec gate.
func (s *Service) Processes(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cctx, "tasklist")
	} else {
		cmd = exec.CommandContext(cctx, "ps", "aux")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("osys: list processes: %w", err)
	}
	return clip(string(out), maxOutputBytes), nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[output truncated]"
}
