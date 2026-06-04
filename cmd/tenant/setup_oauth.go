package main

// `tenant oauth-setup <skill> <path>` installs a maintainer-owned
// OAuth client JSON at the well-known location so the interactive
// /configure flow can skip the "paste your JSON" step entirely.
//
// Today this is gsuite-only. The pattern generalizes: future skills
// that use OAuth (Atlassian, GitHub, etc.) will each have their own
// embedded-creds slot under cfgDir.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"tenant/internal/plugins/gsuite"
)

func cmdOAuthSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("oauth-setup", flag.ContinueOnError)
	c := bindCommon(fs)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "tenant oauth-setup — install a maintainer-owned OAuth client so end users skip Cloud Console setup")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  tenant oauth-setup <skill>                       # interactive walkthrough")
		fmt.Fprintln(fs.Output(), "  tenant oauth-setup <skill> <path-to-client.json> # one-shot install")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Supported skills:")
		fmt.Fprintln(fs.Output(), "  gsuite   Google (Gmail, Calendar, Drive)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fs.Usage()
		return errors.New("need <skill>")
	}
	skill := rest[0]

	if err := c.resolve(); err != nil {
		return err
	}

	switch skill {
	case "gsuite":
		if len(rest) >= 2 {
			return installGSuiteOAuth(c.cfgDir, rest[1])
		}
		return interactiveGSuiteOAuth(ctx, c.cfgDir)
	default:
		return fmt.Errorf("unsupported skill %q (only 'gsuite' supports oauth-setup today)", skill)
	}
}

// interactiveGSuiteOAuth walks the maintainer through the Cloud Console
// dance step by step. Opens the right page in the browser, waits for
// them to download the JSON, prompts for the path, installs it.
func interactiveGSuiteOAuth(ctx context.Context, cfgDir string) error {
	fmt.Fprintln(os.Stderr, "tenant oauth-setup gsuite — interactive walkthrough")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Goal: create an OAuth client in Google Cloud Console (~5 min, one-time).")
	fmt.Fprintln(os.Stderr, "After this, anyone using this tenant binary just clicks 'Sign in with")
	fmt.Fprintln(os.Stderr, "Google' — no Cloud Console for them.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Press Enter to open Google Cloud Console, or Ctrl-C to abort.")
	reader := bufio.NewReader(os.Stdin)
	_, _ = reader.ReadString('\n')

	_ = openOAuthBrowser("https://console.cloud.google.com/apis/credentials")

	fmt.Fprintln(os.Stderr, "Follow these steps in your browser:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  1. (First-time only) Create a Google Cloud project: top bar > Create Project.")
	fmt.Fprintln(os.Stderr, "  2. (First-time only) APIs & Services > Enabled APIs > + Enable APIs:")
	fmt.Fprintln(os.Stderr, "     enable Gmail API, Google Calendar API, Google Drive API.")
	fmt.Fprintln(os.Stderr, "  3. (First-time only) APIs & Services > OAuth consent screen:")
	fmt.Fprintln(os.Stderr, "     External, app name 'tenant', add yourself as a Test User. Save.")
	fmt.Fprintln(os.Stderr, "  4. Back at Credentials: + Create Credentials > OAuth client ID >")
	fmt.Fprintln(os.Stderr, "     Application type: Desktop app. Name: anything. Click Create.")
	fmt.Fprintln(os.Stderr, "  5. On the new client, click the download icon (right side).")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Full guide with screenshots: docs/SETUP-GSUITE-OAUTH.md")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "When the download finishes, paste the full path here > ")
	pathLine, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read path: %w", err)
	}
	pathLine = strings.TrimSpace(pathLine)
	// Strip surrounding quotes (Finder/Explorer often wraps paths in quotes).
	pathLine = strings.Trim(pathLine, `"'`)
	if pathLine == "" {
		return errors.New("no path provided — re-run when ready")
	}
	return installGSuiteOAuth(cfgDir, pathLine)
}

// openOAuthBrowser opens a URL in the OS-default browser. Best-effort.
func openOAuthBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

// installGSuiteOAuth validates the source JSON looks like a Desktop
// App OAuth client, copies it to <cfgDir>/oauth_client.json, and
// prints a confirmation suitable for CLI use. Idempotent.
func installGSuiteOAuth(cfgDir, srcPath string) error {
	if err := installGSuiteOAuthSilent(cfgDir, srcPath); err != nil {
		return err
	}
	dstPath := gsuite.EmbeddedOAuthPath(cfgDir)
	fmt.Println("✓ installed OAuth client to", dstPath)
	fmt.Println()
	fmt.Println("End users running /configure gsuite can now pick 'oauth' and")
	fmt.Println("will sign in directly — no Cloud Console step. Note: signing")
	fmt.Println("users will see a 'Google hasn't verified this app' warning")
	fmt.Println("until you complete Google OAuth verification, which is normal")
	fmt.Println("for unpublished apps in 'Testing' mode.")
	return nil
}

// installGSuiteOAuthSilent does the install without any stdout/stderr
// output. Used by the TUI's auto-install path (where the user sees
// status via sysChat, not stdout). Single source of truth for the
// validate + copy logic — installGSuiteOAuth wraps this.
func installGSuiteOAuthSilent(cfgDir, srcPath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("source file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("source must be a file, not a directory: %s", srcPath)
	}
	b, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	if _, err := gsuite.ParseOAuthClientFile(b, []string{"placeholder"}); err != nil {
		return fmt.Errorf("invalid OAuth client JSON: %w", err)
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return fmt.Errorf("ensure cfg dir: %w", err)
	}
	dstPath := gsuite.EmbeddedOAuthPath(cfgDir)
	dst, err := os.CreateTemp(cfgDir, ".oauth_client.json.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	dstName := dst.Name()
	cleanup := func() {
		dst.Close()
		_ = os.Remove(dstName)
	}
	if _, err := io.Copy(dst, fileBytes(b)); err != nil {
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := dst.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(dstName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(dstName, dstPath); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// fileBytes wraps a byte slice as an io.Reader. Tiny helper so the
// io.Copy above stays readable.
func fileBytes(b []byte) io.Reader {
	return &byteReader{b: b}
}

type byteReader struct {
	b   []byte
	pos int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
