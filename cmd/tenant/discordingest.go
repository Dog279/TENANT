package main

// discordingest.go: attachment ingestion for the Discord relay. The operator
// DMs the bot a file (screenshot, CSV, PDF, clip); the relay downloads it to
// a local inbox under the data dir and hands the agent a manifest of saved
// paths, so file-reading tools (OS plugin, future wealthd CSV ingest) can
// operate on it. Storage only — no conversion, no parsing: HEIC stays HEIC,
// MP4 is archival (nothing watches video); the agent reads what its tools can
// read.
//
// Trust boundary: ingestion is OPERATOR-ONLY regardless of surface (the relay
// nils attachments from anyone else before calling Ingest) — guild members
// may drive text turns via @mention, but nobody except the operator writes
// files onto the host. Downloads are further restricted to Discord's own CDN
// hosts, size-capped locally (Discord's Size field is a hint, not a promise),
// and filenames are sanitized before touching the filesystem. Discord CDN
// URLs are signed and expire (~24h), so ingestion happens at message time.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tenant/internal/plugins/discord"
)

// allowedAttachmentExts is the operator-curated common-type allowlist:
// images (png/jpg/heic — heic because iPhones default to it), docs
// (pdf/csv/txt), and mp4. Everything else is rejected by name in the reply
// so the operator knows to convert or rename.
var allowedAttachmentExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".heic": true,
	".pdf": true, ".csv": true, ".txt": true, ".mp4": true,
}

const (
	// maxAttachmentBytes caps one file. Above every current Discord upload
	// tier's free limit; the point is guarding the host disk against
	// Nitro-sized uploads, not policing Discord's own rules.
	maxAttachmentBytes = 100 << 20 // 100 MiB

	// maxAttachmentsPerMsg mirrors Discord's own per-message ceiling.
	maxAttachmentsPerMsg = 10

	// attachmentIngestTimeout bounds the whole download batch for one
	// message (runs off the gateway goroutine, so this protects the turn
	// pipeline, not the socket).
	attachmentIngestTimeout = 90 * time.Second

	// relayInboxRetention is how long saved files live before the sweep
	// prunes them. The inbox is a hand-off buffer, not an archive — anything
	// worth keeping should be moved by the agent or the operator.
	relayInboxRetention = 30 * 24 * time.Hour

	relayInboxDirName = "relay-inbox"
)

// savedAttachment describes one successfully stored file.
type savedAttachment struct {
	Path        string
	ContentType string
	Size        int64
}

// attachmentIngestor downloads relay attachments into
// <dataDir>/relay-inbox/<yyyy-mm-dd>/. Safe for use from one relay goroutine
// at a time (the relay serializes per message); the retention sweep runs once
// per process in the background.
type attachmentIngestor struct {
	dir       string
	client    *http.Client
	log       *slog.Logger
	anyHost   bool // tests only: permit non-Discord-CDN URLs (httptest)
	sweepOnce sync.Once
	// keep is the configured retention (privacy.retention_days.relay_inbox,
	// TEN-309); nil means relayInboxRetention, and 0 keeps files forever.
	keep func() time.Duration
}

func newAttachmentIngestor(dataDir string, log *slog.Logger) *attachmentIngestor {
	if log == nil {
		log = slog.Default()
	}
	return &attachmentIngestor{
		dir:    filepath.Join(dataDir, relayInboxDirName),
		client: &http.Client{}, // per-call deadline via ctx
		log:    log,
	}
}

// Ingest filters and downloads atts, returning what was saved plus
// operator-readable reject reasons. Failures are per-file, never fatal to the
// batch.
func (ai *attachmentIngestor) Ingest(ctx context.Context, atts []discord.Attachment) (saved []savedAttachment, rejected []string) {
	ai.sweepOnce.Do(func() { go ai.sweep() })
	if len(atts) > maxAttachmentsPerMsg {
		atts = atts[:maxAttachmentsPerMsg]
	}
	for _, a := range atts {
		if reason := ai.acceptable(a); reason != "" {
			rejected = append(rejected, fmt.Sprintf("%s — %s", displayFilename(a.Filename), reason))
			continue
		}
		sa, err := ai.download(ctx, a)
		if err != nil {
			ai.log.Warn("relay attachment download failed", "file", a.Filename, "err", err)
			rejected = append(rejected, fmt.Sprintf("%s — download failed", displayFilename(a.Filename)))
			continue
		}
		saved = append(saved, sa)
	}
	return saved, rejected
}

// acceptable returns "" for an ingestable attachment, else the reject reason.
func (ai *attachmentIngestor) acceptable(a discord.Attachment) string {
	ext := strings.ToLower(filepath.Ext(a.Filename))
	if !allowedAttachmentExts[ext] {
		return "file type not allowed (png/jpg/heic/pdf/csv/txt/mp4)"
	}
	if a.Size <= 0 {
		return "empty file"
	}
	if a.Size > maxAttachmentBytes {
		return fmt.Sprintf("too large (max %d MB)", maxAttachmentBytes>>20)
	}
	if !ai.anyHost {
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || !isDiscordCDNHost(u.Host) {
			return "unexpected download host"
		}
	}
	return ""
}

// isDiscordCDNHost restricts downloads to Discord's own CDN domains — the
// gateway only hands us Discord-supplied URLs, but the relay is an internet-
// facing surface and defense-in-depth is cheap.
func isDiscordCDNHost(host string) bool {
	host = strings.ToLower(host)
	return host == "cdn.discordapp.com" || host == "media.discordapp.net" ||
		strings.HasSuffix(host, ".discordapp.com") || strings.HasSuffix(host, ".discordapp.net")
}

// download fetches one attachment to the dated inbox dir via a temp file, so
// a partial download never masquerades as a complete one.
func (ai *attachmentIngestor) download(ctx context.Context, a discord.Attachment) (savedAttachment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return savedAttachment{}, err
	}
	resp, err := ai.client.Do(req)
	if err != nil {
		return savedAttachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return savedAttachment{}, fmt.Errorf("cdn status %d", resp.StatusCode)
	}

	dayDir := filepath.Join(ai.dir, time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		return savedAttachment{}, err
	}
	dest := uniquePath(dayDir, sanitizeAttachmentName(a.Filename))
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return savedAttachment{}, err
	}
	// +1 so a body larger than the cap is detectable rather than silently
	// truncated (the Discord-reported Size was already checked, but the
	// body is what actually counts).
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxAttachmentBytes+1))
	cerr := f.Close()
	if err != nil || cerr != nil {
		_ = os.Remove(tmp)
		if err == nil {
			err = cerr
		}
		return savedAttachment{}, err
	}
	if n > maxAttachmentBytes {
		_ = os.Remove(tmp)
		return savedAttachment{}, fmt.Errorf("body exceeds %d bytes", int64(maxAttachmentBytes))
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return savedAttachment{}, err
	}
	return savedAttachment{Path: dest, ContentType: a.ContentType, Size: n}, nil
}

// sanitizeAttachmentName reduces an untrusted filename to a safe basename:
// path components stripped, runes outside [A-Za-z0-9._-] replaced, and a
// fallback name when nothing survives.
func sanitizeAttachmentName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "attachment"
	}
	return out
}

// uniquePath returns dir/name, suffixing -2, -3, … before the extension when
// the name is taken (same screenshot name DMed twice in one day).
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
}

// sweep prunes inbox files older than the retention window and removes
// emptied date dirs. Best-effort, but not silent: the retention window is a
// privacy setting, so what it could not delete is logged (once per sweep).
func (ai *attachmentIngestor) sweep() {
	keep := relayInboxRetention
	if ai.keep != nil {
		keep = ai.keep()
	}
	if keep <= 0 {
		return
	}
	cutoff := time.Now().Add(-keep)
	days, err := os.ReadDir(ai.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			ai.log.Warn("discord relay: inbox retention sweep skipped", "dir", ai.dir, "err", err)
		}
		return
	}
	var failed int
	var firstErr error
	fail := func(err error) {
		failed++
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, d := range days {
		if !d.IsDir() {
			continue
		}
		dayPath := filepath.Join(ai.dir, d.Name())
		files, err := os.ReadDir(dayPath)
		if err != nil {
			fail(err)
			continue
		}
		remaining := len(files)
		for _, f := range files {
			info, err := f.Info()
			if err != nil || info.IsDir() {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(dayPath, f.Name())); err != nil {
					fail(err)
				} else {
					remaining--
				}
			}
		}
		if remaining == 0 {
			if err := os.Remove(dayPath); err != nil {
				fail(err)
			}
		}
	}
	if failed > 0 {
		ai.log.Warn("discord relay: inbox retention sweep could not delete everything past retention", "failures", failed, "err", firstErr)
	}
}

// displayFilename clips an untrusted name for echo in replies/manifests.
func displayFilename(name string) string {
	name = sanitizeAttachmentName(name)
	if len(name) > 48 {
		name = name[:45] + "..."
	}
	return name
}

// attachmentManifest renders saved/rejected files as the text block appended
// to the turn, so the agent knows exactly which local paths arrived.
func attachmentManifest(saved []savedAttachment, rejected []string) string {
	if len(saved) == 0 && len(rejected) == 0 {
		return ""
	}
	var b strings.Builder
	if len(saved) > 0 {
		b.WriteString("[files received via Discord — saved on the host]\n")
		for _, s := range saved {
			ct := s.ContentType
			if ct == "" {
				ct = "unknown type"
			}
			fmt.Fprintf(&b, "- %s (%s, %s)\n", s.Path, ct, humanBytes(s.Size))
		}
	}
	if len(rejected) > 0 {
		fmt.Fprintf(&b, "(not saved: %s)", strings.Join(rejected, "; "))
	}
	return strings.TrimSpace(b.String())
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
