package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/attach"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
)

// --- filtering ------------------------------------------------------------

func TestAttachmentAcceptable(t *testing.T) {
	ai := newAttachmentIngestor(t.TempDir())
	cdn := "https://cdn.discordapp.com/attachments/1/2/"
	cases := []struct {
		name   string
		att    discord.Attachment
		wantOK bool
	}{
		{"png ok", discord.Attachment{Filename: "shot.PNG", Size: 100, URL: cdn + "shot.png"}, true},
		{"heic ok", discord.Attachment{Filename: "img.heic", Size: 100, URL: cdn + "img.heic"}, true},
		{"csv ok", discord.Attachment{Filename: "q3.csv", Size: 100, URL: cdn + "q3.csv"}, true},
		{"mp4 ok", discord.Attachment{Filename: "clip.mp4", Size: 100, URL: cdn + "clip.mp4"}, true},
		{"exe rejected", discord.Attachment{Filename: "virus.exe", Size: 100, URL: cdn + "v.exe"}, false},
		{"no ext rejected", discord.Attachment{Filename: "README", Size: 100, URL: cdn + "README"}, false},
		{"oversize rejected", discord.Attachment{Filename: "big.mp4", Size: maxAttachmentBytes + 1, URL: cdn + "big.mp4"}, false},
		{"empty rejected", discord.Attachment{Filename: "a.txt", Size: 0, URL: cdn + "a.txt"}, false},
		{"http rejected", discord.Attachment{Filename: "a.txt", Size: 5, URL: "http://cdn.discordapp.com/a.txt"}, false},
		{"foreign host rejected", discord.Attachment{Filename: "a.txt", Size: 5, URL: "https://evil.example.com/a.txt"}, false},
	}
	for _, c := range cases {
		reason := ai.acceptable(c.att)
		if (reason == "") != c.wantOK {
			t.Errorf("%s: acceptable=%q, wantOK=%v", c.name, reason, c.wantOK)
		}
	}
}

// --- name handling --------------------------------------------------------

func TestSanitizeAttachmentName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "passwd",
		"..\\..\\win\\boot.txt":  "boot.txt",
		"we ird  näme!!.csv":     "we_ird__n_me__.csv",
		"...":                    "attachment",
		"":                       "attachment",
		"IMG_0042 (edited).jpeg": "IMG_0042__edited_.jpeg",
	}
	for in, want := range cases {
		if got := sanitizeAttachmentName(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	first := uniquePath(dir, "a.csv")
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := uniquePath(dir, "a.csv")
	if second == first || !strings.HasSuffix(second, "a-2.csv") {
		t.Fatalf("collision suffix wrong: %s", second)
	}
}

// --- download + manifest --------------------------------------------------

func TestIngestDownloadsAndManifests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("date,amount\n2026-08-13,42\n"))
	}))
	defer srv.Close()

	ai := newAttachmentIngestor(t.TempDir())
	ai.anyHost = true // httptest is not a Discord CDN host

	saved, rejected := ai.Ingest(context.Background(), []discord.Attachment{
		{Filename: "spend.csv", Size: 26, ContentType: "text/csv", URL: srv.URL + "/spend.csv"},
		{Filename: "malware.scr", Size: 10, URL: srv.URL + "/malware.scr"},
	})
	if len(saved) != 1 || len(rejected) != 1 {
		t.Fatalf("saved=%d rejected=%d, want 1/1 (%v / %v)", len(saved), len(rejected), saved, rejected)
	}
	b, err := os.ReadFile(saved[0].Path)
	if err != nil || !strings.HasPrefix(string(b), "date,amount") {
		t.Fatalf("saved file wrong: %v %q", err, b)
	}
	if !strings.Contains(saved[0].Path, time.Now().Format("2006-01-02")) {
		t.Fatalf("expected dated dir in path: %s", saved[0].Path)
	}

	m := attachmentManifest(saved, rejected)
	for _, want := range []string{"[files received via Discord", saved[0].Path, "text/csv", "not saved", "malware.scr"} {
		if !strings.Contains(m, want) {
			t.Fatalf("manifest missing %q:\n%s", want, m)
		}
	}
}

func TestIngestRejectsLyingContentLength(t *testing.T) {
	// The server claims a small file in metadata but streams > cap: the local
	// limit must catch it. Use a tiny synthetic cap via a large body without
	// actually allocating 100MB — stream zeros.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < (maxAttachmentBytes>>20)+2; i++ { // cap + 2 MB of zeros
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ai := newAttachmentIngestor(t.TempDir())
	ai.anyHost = true
	saved, rejected := ai.Ingest(context.Background(), []discord.Attachment{
		{Filename: "honest.mp4", Size: 1000, URL: srv.URL + "/honest.mp4"},
	})
	if len(saved) != 0 || len(rejected) != 1 {
		t.Fatalf("oversized body must reject: saved=%v rejected=%v", saved, rejected)
	}
}

// --- retention ------------------------------------------------------------

func TestInboxSweep(t *testing.T) {
	dataDir := t.TempDir()
	ai := newAttachmentIngestor(dataDir)
	oldDir := filepath.Join(ai.dir, "2020-01-01")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(oldDir, "stale.txt")
	if err := os.WriteFile(oldFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-relayInboxRetention - time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}
	ai.sweep()
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("stale file must be pruned")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("emptied date dir must be removed")
	}
}

// The configured relay_inbox retention (TEN-309) replaces the 30-day default:
// a shorter period prunes sooner, and 0 (forever) prunes nothing.
func TestInboxSweepHonorsConfiguredRetention(t *testing.T) {
	ai := newAttachmentIngestor(t.TempDir())
	dir := filepath.Join(ai.dir, "2026-09-20")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "week-old.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-7 * 24 * time.Hour)
	if err := os.Chtimes(file, past, past); err != nil {
		t.Fatal(err)
	}
	ai.keep = func() time.Duration { return 0 }
	ai.sweep()
	if _, err := os.Stat(file); err != nil {
		t.Fatal("a relay file was pruned while retention is forever")
	}
	ai.keep = func() time.Duration { return 3 * 24 * time.Hour }
	ai.sweep()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a relay file outlived a 3-day retention")
	}
}

// --- relay flow -----------------------------------------------------------

// Operator DM with an attachment: the turn text carries both the operator's
// words and the saved-file manifest.
func TestRelay_AttachmentIngestFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello file"))
	}))
	defer srv.Close()

	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, "op1")
	ai := newAttachmentIngestor(t.TempDir())
	ai.anyHost = true
	r.files = ai
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{
		ChannelID: "dm1", AuthorID: "op1", Content: "ingest this",
		Attachments: []discord.Attachment{
			{Filename: "note.txt", Size: 10, ContentType: "text/plain", URL: srv.URL + "/note.txt"},
		},
	})

	q, ok := recv(fr.turns, 3*time.Second)
	if !ok {
		t.Fatal("no turn started for attachment message")
	}
	if !strings.Contains(q, "ingest this") || !strings.Contains(q, "[files received via Discord") ||
		!strings.Contains(q, "note.txt") {
		t.Fatalf("turn text missing pieces:\n%s", q)
	}
}

// Attachment-only DM (no text) still drives a turn — previously an empty
// Content meant the message was dropped entirely.
func TestRelay_AttachmentOnlyMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("csv,data"))
	}))
	defer srv.Close()

	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, "op1")
	ai := newAttachmentIngestor(t.TempDir())
	ai.anyHost = true
	r.files = ai
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{
		ChannelID: "dm1", AuthorID: "op1",
		Attachments: []discord.Attachment{
			{Filename: "spend.csv", Size: 8, ContentType: "text/csv", URL: srv.URL + "/spend.csv"},
		},
	})
	q, ok := recv(fr.turns, 3*time.Second)
	if !ok {
		t.Fatal("attachment-only message must drive a turn")
	}
	if !strings.Contains(q, "spend.csv") {
		t.Fatalf("turn text missing manifest:\n%s", q)
	}
}

// Attachment-only DM where everything is rejected: reply directly, no turn.
func TestRelay_AttachmentAllRejected(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, "op1")
	ai := newAttachmentIngestor(t.TempDir())
	ai.anyHost = true
	r.files = ai
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{
		ChannelID: "dm1", AuthorID: "op1",
		Attachments: []discord.Attachment{
			{Filename: "setup.exe", Size: 10, URL: "https://cdn.discordapp.com/x/setup.exe"},
		},
	})
	if !waitSends(fs, 1, 3*time.Second) {
		t.Fatal("expected a rejection reply")
	}
	if got := fs.all()[0]; !strings.Contains(got, "no attachments accepted") {
		t.Fatalf("unexpected reply: %s", got)
	}
	select {
	case q := <-fr.turns:
		t.Fatalf("no turn should start, got %q", q)
	case <-time.After(200 * time.Millisecond):
	}
}

// Non-operator guild mention: text still routes, attachments are dropped.
func TestRelay_NonOperatorAttachmentsDropped(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, "op1")
	ai := newAttachmentIngestor(t.TempDir())
	ai.anyHost = true
	r.files = ai
	r.Start(context.Background())
	r.setBotID("bot9")

	r.handleInbound(discord.Inbound{
		ChannelID: "g-chan", GuildID: "g1", AuthorID: "rando",
		Content: "<@bot9> summarize this",
		Attachments: []discord.Attachment{
			{Filename: "sneaky.txt", Size: 5, URL: "https://cdn.discordapp.com/x/sneaky.txt"},
		},
	})
	q, ok := recv(fr.turns, 3*time.Second)
	if !ok {
		t.Fatal("mention turn should start")
	}
	if strings.Contains(q, "sneaky") || strings.Contains(q, "[files received") {
		t.Fatalf("non-operator attachments must not be ingested:\n%s", q)
	}
}

// imageRunner records the images each turn carried.
type imageRunner struct{ got chan []model.Image }

func (r *imageRunner) Turn(_ context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	r.got <- req.Images
	return &agent.TurnResult{Response: "ok"}, nil
}
func (r *imageRunner) Interject(string) {}

// TEN-493: an operator's photo on Discord reaches the model as an image (the
// text manifest still lists it); a webp is accepted.
func TestRelay_ImageAttachmentReachesTheModel(t *testing.T) {
	var pic bytes.Buffer
	_ = png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 12, 9)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pic.Bytes()) }))
	defer srv.Close()

	ir := &imageRunner{got: make(chan []model.Image, 1)}
	r := newRelay(ir, &fakeSender{}, "op1")
	dir := t.TempDir()
	ai := newAttachmentIngestor(dir)
	ai.anyHost = true
	r.files = ai
	r.images = attach.NewStore(dir)
	r.Start(context.Background())
	r.handleInbound(discord.Inbound{ChannelID: "dm1", AuthorID: "op1", Content: "what's this?",
		Attachments: []discord.Attachment{{Filename: "photo.png", Size: int64(pic.Len()), ContentType: "image/png", URL: srv.URL + "/photo.png"}}})
	select {
	case imgs := <-ir.got:
		if len(imgs) != 1 || imgs[0].Width != 12 || imgs[0].Name != "photo.png" {
			t.Fatalf("turn images = %+v", imgs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no turn")
	}
	if !allowedAttachmentExts[".webp"] || !allowedAttachmentExts[".gif"] {
		t.Fatal("gif and webp should be accepted")
	}
}
