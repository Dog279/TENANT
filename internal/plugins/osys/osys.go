package osys

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"  // register decoders for DecodeConfig
	_ "image/jpeg" // register decoders for DecodeConfig
	_ "image/png"  // register decoders for DecodeConfig
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"tenant/internal/logs/capture"
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
	// Runner, when non-nil, replaces realRunner as the shell executor. It lets
	// cmd/tenant inject a sandbox-backed runner (the execution-backend seam)
	// without any osys call site changing. nil ⇒ today's realRunner behaviour.
	Runner runner
}

// Service is the opened OS connector. Its file methods take paths as
// Roots.Resolve returns them: absolute, with no symlink left on the way. On
// macOS and Linux they open such a path refusing a symlink anywhere on it
// (openFile), so a link that appears after the check fails the open.
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
	run := cfg.Runner
	if run == nil {
		run = realRunner
	}
	return &Service{run: run, timeout: to}, nil
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
// operator's machine exposes — enabling this plugin is the consent. vet, when
// non-nil, inspects the file once it is open and before anything is read; an
// error from it aborts the read.
func (s *Service) ReadFile(path string, vet func(fs.FileInfo) error) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: file path required")
	}
	f, err := openVetted(path, os.O_RDONLY, vet)
	if err != nil {
		return "", fmt.Errorf("osys: read %s: %w", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
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

// openVetted opens path with openFile (so a symlink swapped onto it fails the
// open where the OS can refuse one) and lets vet (nil = none) inspect the OPEN
// file before the caller reads or writes it. Checking the open file rather
// than its path is what makes the check race-free (see Roots.checkOpened). A
// write's flag must not include O_TRUNC, so a refusal leaves the contents as
// they were.
func openVetted(path string, flag int, vet func(fs.FileInfo) error) (*os.File, error) {
	f, err := openFile(path, flag, 0o644)
	if err != nil || vet == nil {
		return f, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = vet(fi)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// overwrite replaces an open file's contents and closes it. Truncating here,
// after vetting, instead of with O_TRUNC at open, is what keeps a refused file
// intact.
func overwrite(f *os.File, content string) error {
	err := f.Truncate(0)
	if err == nil {
		_, err = f.WriteAt([]byte(content), 0)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// WriteFile creates or overwrites a text file. When createDirs is true, any
// missing parent directories are created. vet, when non-nil, inspects the file
// once it is open and before anything is written; an error from it aborts the
// write. Returns a short confirmation.
func (s *Service) WriteFile(path, content string, createDirs bool, vet func(fs.FileInfo) error) (string, error) {
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
	f, err := openVetted(path, os.O_WRONLY|os.O_CREATE, vet)
	if err == nil {
		err = overwrite(f, content)
	}
	if err != nil {
		return "", fmt.Errorf("osys: write %s: %w", path, err)
	}
	verb := "created"
	if existed {
		verb = "overwrote"
	}
	return fmt.Sprintf("%s %s (%d bytes)", verb, path, len(content)), nil
}

// AppendFile appends content to a file, creating it (and parents) if absent.
// vet is as for WriteFile.
func (s *Service) AppendFile(path, content string, vet func(fs.FileInfo) error) (string, error) {
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
	f, err := openVetted(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, vet)
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
// vet is as for WriteFile.
func (s *Service) EditFile(path, oldStr, newStr string, replaceAll bool, vet func(fs.FileInfo) error) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: file path required")
	}
	if oldStr == "" {
		return "", fmt.Errorf("osys: old_string required (use os_write_file to create a file)")
	}
	// One handle for the read and the write, so the file vetted is the file
	// read and the file changed.
	f, err := openVetted(path, os.O_RDWR, vet)
	if err != nil {
		return "", fmt.Errorf("osys: read %s: %w", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
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
	if err := overwrite(f, content); err != nil {
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
// about images it's working with (and confirm a file is a valid image). vet
// is as for ReadFile.
func (s *Service) ReadImage(path string, vet func(fs.FileInfo) error) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("osys: image path required")
	}
	f, err := openVetted(path, os.O_RDONLY, vet)
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
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := capture.New(cctx, "plugin.os", "child", cmd.Args[0]).Run(cmd); err != nil {
		return "", fmt.Errorf("osys: list processes: %w", err)
	}
	return clip(out.String(), maxOutputBytes), nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[output truncated]"
}
