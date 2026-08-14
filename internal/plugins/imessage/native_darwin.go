//go:build darwin

package imessage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go, CGO-free SQLite driver
)

// nativeService is the macOS-native iMessage transport: it reads
// ~/Library/Messages/chat.db (via the tag-free chatReader) and sends via
// osascript→Messages.app. It satisfies the transport interface and the
// Native interface (transport + Close).
type nativeService struct {
	db *sql.DB
	r  *chatReader
}

// OpenNative opens the local Messages chat.db read-only and returns a
// native transport. It maps the macOS TCC "can't open" failure to an
// actionable Full Disk Access error.
//
// CRITICAL: we open with mode=ro ONLY (plus query_only) — NOT
// immutable=1. chat.db runs in WAL mode; immutable=1 makes SQLite ignore
// the -wal sidecar and read a stale snapshot, so a watcher would never
// see new messages until a checkpoint. A plain read-only WAL reader does
// not block the live Messages writer.
func OpenNative(cfg NativeConfig) (Native, error) {
	path := strings.TrimSpace(cfg.DBPath)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("imessage: locate home dir: %w", err)
		}
		path = filepath.Join(home, "Library", "Messages", "chat.db")
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("imessage: chat.db not found at %s (is this a Mac with Messages set up?)", path)
		}
		return nil, mapOpenError(path, err)
	}

	dsn := "file:" + path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("imessage: open chat.db: %w", err)
	}

	// Force a real read so a TCC/FDA denial surfaces here (sql.Open is
	// lazy and won't touch the file). A bounded context avoids hanging if
	// the FS is wedged.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "SELECT 1 FROM sqlite_master LIMIT 1"); err != nil {
		_ = db.Close()
		return nil, mapOpenError(path, err)
	}

	return &nativeService{db: db, r: &chatReader{db: db}}, nil
}

// mapOpenError turns a permission/TCC failure into a friendly,
// actionable Full Disk Access message; otherwise wraps the raw error.
func mapOpenError(path string, err error) error {
	if isPermissionErr(err) {
		return fmt.Errorf("imessage: cannot read %s — grant Full Disk Access to your terminal "+
			"(or the tenant binary) in System Settings → Privacy & Security → Full Disk Access, "+
			"then retry. (underlying: %v)", path, err)
	}
	return fmt.Errorf("imessage: open chat.db: %w", err)
}

// isPermissionErr recognizes the several ways macOS surfaces a TCC denial
// on chat.db: os.ErrPermission, SQLite's "unable to open database file",
// and the literal "authorization denied".
func isPermissionErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "authorization denied") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "unable to open database")
}

// --- reads delegate to the tag-free chatReader ---

func (n *nativeService) ListChats(ctx context.Context, limit int) ([]Chat, error) {
	return n.r.ListChats(ctx, limit)
}

func (n *nativeService) ChatMessages(ctx context.Context, chatGUID string, limit int) ([]Message, error) {
	return n.r.ChatMessages(ctx, chatGUID, limit)
}

func (n *nativeService) SearchMessages(ctx context.Context, text string, limit int) ([]Message, error) {
	return n.r.SearchMessages(ctx, text, limit)
}

// MessagesSince exposes the watcher's read primitive.
func (n *nativeService) MessagesSince(ctx context.Context, afterRowID int64, limit int) ([]InboundMessage, error) {
	return n.r.MessagesSince(ctx, afterRowID, limit)
}

// LatestRowID exposes the current max ROWID for the watcher's seed-to-now.
func (n *nativeService) LatestRowID(ctx context.Context) (int64, error) {
	return n.r.LatestRowID(ctx)
}

// Close releases the chat.db handle.
func (n *nativeService) Close() error {
	if n.db == nil {
		return nil
	}
	err := n.db.Close()
	n.db = nil
	return err
}

// --- sends via osascript (gated upstream by Policy) ---

// SendText sends to an existing conversation by chat guid. AppleScript
// gives no message guid back, so it returns "" on success — the
// dispatcher reports "message sent" for an empty id.
func (n *nativeService) SendText(ctx context.Context, chatGUID, text string) (string, error) {
	if strings.TrimSpace(chatGUID) == "" {
		return "", fmt.Errorf("imessage: chat guid required")
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("imessage: message text is empty")
	}
	text = sanitizeOutbound(text) // dedup layer 3: strip edge BOM/zero-width
	if text == "" {
		return "", fmt.Errorf("imessage: message text is empty")
	}
	script, argv := sendToChatScript(chatGUID, text)
	return "", runOsascript(ctx, script, argv)
}

// NewChat starts/uses a direct conversation with an address and sends the
// first message.
func (n *nativeService) NewChat(ctx context.Context, address, text string) (string, error) {
	if strings.TrimSpace(address) == "" {
		return "", fmt.Errorf("imessage: recipient address (phone/email) required")
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("imessage: message text is empty")
	}
	text = sanitizeOutbound(text) // dedup layer 3: strip edge BOM/zero-width
	if text == "" {
		return "", fmt.Errorf("imessage: message text is empty")
	}
	script, argv := sendToBuddyScript(address, text)
	return "", runOsascript(ctx, script, argv)
}

// runOsascript executes an AppleScript with positional argv. The text is
// passed as argv (never interpolated) — see applescript.go. The first
// send triggers an Automation TCC prompt that can hang, so we bound it
// with a timeout and translate that into an actionable message.
func runOsascript(ctx context.Context, script string, argv []string) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// `osascript -e <script> arg1 arg2 …` passes the trailing args to the
	// script's `on run argv` handler.
	args := append([]string{"-e", script}, argv...)
	cmd := exec.CommandContext(cctx, "osascript", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("imessage: osascript timed out — grant Automation access to control "+
				"Messages in System Settings → Privacy & Security → Automation, then retry (%s)", msg)
		}
		return fmt.Errorf("imessage: osascript send failed: %s", msg)
	}
	return nil
}
