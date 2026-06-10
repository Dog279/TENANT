package imessage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// watch.go is the anti-loop layer ported from openclaw's design — the
// "don't talk to yourself" patch. It is build-tag-free and depends only
// on small interfaces, so the full loop-prevention logic is unit-testable
// on any OS with synthetic rows.
//
// The four primitives (see WatchConfig / Watcher.Poll):
//
//  1. is_from_me filter — never surface our own / the operator's sends.
//  2. Monotonic ROWID cursor — only rows with ROWID greater than the last
//     processed one, persisted so a restart doesn't replay history.
//  3. Echo/dedup cache — every text we send is recorded briefly; a
//     matching inbound row is dropped even if Apple surfaces it back.
//  4. Allowlist (optional) — when set, only handles on the list pass;
//     empty means read-only observation of everyone.
//
// Layer 1 ships these primitives with a NON-autonomous consumer: Poll
// surfaces actionable inbound messages but never auto-replies. The
// autonomous responder (auto-reply + approval routing) is the documented
// follow-up.

// messageSource is the read primitive the Watcher polls. *chatReader and
// the darwin *nativeService both satisfy it.
type messageSource interface {
	MessagesSince(ctx context.Context, afterRowID int64, limit int) ([]InboundMessage, error)
}

// cursorStore persists the monotonic ROWID cursor across restarts.
// *improve.Meta satisfies it (GetInt64/SetInt64 over the tenant_meta KV
// table) — no new DB file.
type cursorStore interface {
	GetInt64(ctx context.Context, key string) (int64, bool, error)
	SetInt64(ctx context.Context, key string, n int64) error
}

// WatchConfig configures a Watcher.
type WatchConfig struct {
	Source    messageSource // required: where rows come from
	Store     cursorStore   // optional: cursor persistence (nil ⇒ in-memory only)
	Account   string        // cursor key suffix, e.g. an account/handle id
	AllowFrom []string      // optional allowlist of sender handles (normalized internally)
	DedupTTL  time.Duration // echo-cache TTL (default 2m)
}

// Watcher yields only actionable inbound messages, applying the four
// anti-loop primitives. It is safe for concurrent Poll + RecordSent use.
type Watcher struct {
	src       messageSource
	store     cursorStore
	cursorKey string
	allowFrom map[string]bool
	dedup     *echoCache

	mu     sync.Mutex
	cursor int64
	loaded bool
}

// cursorKeyPrefix namespaces the watcher cursor in the shared
// tenant_meta KV store.
const cursorKeyPrefix = "imessage_cursor:"

// NewWatcher builds a Watcher. Source is required.
func NewWatcher(cfg WatchConfig) (*Watcher, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("imessage: watcher requires a message source")
	}
	acct := cfg.Account
	if acct == "" {
		acct = "default"
	}
	allow := map[string]bool{}
	for _, h := range cfg.AllowFrom {
		if n := normalizeHandle(h); n != "" {
			allow[n] = true
		}
	}
	return &Watcher{
		src:       cfg.Source,
		store:     cfg.Store,
		cursorKey: cursorKeyPrefix + acct,
		allowFrom: allow,
		dedup:     newEchoCache(cfg.DedupTTL),
	}, nil
}

// RecordSent registers a text we just sent so its echo (should Apple
// surface our own outbound row to the reader) is dropped by Poll.
func (w *Watcher) RecordSent(chatGUID, text string) {
	w.dedup.record(chatGUID, text)
}

// Cursor returns the current ROWID cursor (for diagnostics/tests).
func (w *Watcher) Cursor() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursor
}

// Poll fetches new rows past the cursor and returns the actionable ones,
// advancing (and persisting) the cursor past everything it saw. limit
// bounds the batch size.
func (w *Watcher) Poll(ctx context.Context, limit int) ([]InboundMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureCursorLocked(ctx); err != nil {
		return nil, err
	}
	rows, err := w.src.MessagesSince(ctx, w.cursor, limit)
	if err != nil {
		return nil, err
	}

	out := make([]InboundMessage, 0, len(rows))
	maxRow := w.cursor
	for _, m := range rows {
		if m.RowID > maxRow {
			maxRow = m.RowID
		}
		if m.IsFromMe { // (1) is_from_me
			continue
		}
		if w.dedup.seen(m.ChatGUID, m.Text) { // (3) echo/dedup
			continue
		}
		if len(w.allowFrom) > 0 && !w.allowFrom[normalizeHandle(m.From)] { // (4) allowlist
			continue
		}
		out = append(out, m)
	}

	if maxRow > w.cursor { // (2) monotonic cursor
		w.cursor = maxRow
		if w.store != nil {
			if err := w.store.SetInt64(ctx, w.cursorKey, w.cursor); err != nil {
				return out, fmt.Errorf("imessage: persist cursor: %w", err)
			}
		}
	}
	return out, nil
}

// ensureCursorLocked lazily loads the persisted cursor on first use.
func (w *Watcher) ensureCursorLocked(ctx context.Context) error {
	if w.loaded {
		return nil
	}
	if w.store != nil {
		v, ok, err := w.store.GetInt64(ctx, w.cursorKey)
		if err != nil {
			return fmt.Errorf("imessage: load cursor: %w", err)
		}
		if ok {
			w.cursor = v
		}
	}
	w.loaded = true
	return nil
}

// echoCache records recently-sent (chat,text) pairs with a TTL so the
// Watcher can drop our own echoes.
type echoCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
	now func() time.Time // injectable for tests
}

func newEchoCache(ttl time.Duration) *echoCache {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &echoCache{ttl: ttl, m: map[string]time.Time{}, now: time.Now}
}

func echoKey(chatGUID, text string) string {
	return strings.TrimSpace(chatGUID) + "\x00" + text
}

func (c *echoCache) record(chatGUID, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gcLocked()
	c.m[echoKey(chatGUID, text)] = c.now().Add(c.ttl)
}

func (c *echoCache) seen(chatGUID, text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gcLocked()
	k := echoKey(chatGUID, text)
	exp, ok := c.m[k]
	if !ok {
		return false
	}
	if c.now().After(exp) {
		delete(c.m, k)
		return false
	}
	return true
}

func (c *echoCache) gcLocked() {
	now := c.now()
	for k, exp := range c.m {
		if now.After(exp) {
			delete(c.m, k)
		}
	}
}
