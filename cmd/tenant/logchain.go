package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/store"
	"tenant/internal/secrets"
	"tenant/internal/version"
)

// The Security log's key, anchor and witness (docs/design/logging-v2.md
// §10.2, TEN-396). internal/logs never imports internal/secrets: this
// composition root reads or creates the key, hands it to the store, and
// keeps the anchor beside it in the config directory, outside the data
// directory a restore or a rollback replaces.

const (
	anchorEvery = 1000      // records between anchors
	anchorAge   = time.Hour // or this long, when the head has moved
	chainKeyLen = 32
	dpapiPrefix = "dpapi:" // a key file sealed with the user protector: dpapi:<base64>
)

func chainKeyPath(cfgDir string) string    { return filepath.Join(cfgDir, "logbook.key") }
func chainAnchorPath(cfgDir string) string { return filepath.Join(cfgDir, "logbook.anchor") }

// chainKey reads the Security log's key, creating it on first use: 32
// random bytes, written once with secrets.CreateExclusive (owner-only, a
// current-user ACL on Windows) and sealed with the user protector (DPAPI)
// where the platform has one. Racing processes agree on the first key.
func chainKey(cfgDir string) ([]byte, error) {
	for range 10 {
		key, err := readChainKey(cfgDir)
		if !errors.Is(err, fs.ErrNotExist) {
			return key, err
		}
		key = make([]byte, chainKeyLen)
		_, _ = rand.Read(key) // crypto/rand never fails (Go 1.24+)
		text := []byte(hex.EncodeToString(key) + "\n")
		if p, perr := secrets.NewUserProtector(); perr == nil {
			sealed, err := p.Protect(key)
			if err != nil {
				return nil, fmt.Errorf("seal the Security log key: %w", err)
			}
			text = []byte(dpapiPrefix + base64.StdEncoding.EncodeToString(sealed) + "\n")
		} else if !errors.Is(perr, secrets.ErrProtectorUnavailable) {
			return nil, perr
		}
		created, err := secrets.CreateExclusive(chainKeyPath(cfgDir), text)
		if err != nil {
			return nil, fmt.Errorf("create the Security log key: %w", err)
		}
		if created {
			return key, nil
		}
		// Another process created it first: read its key.
	}
	return nil, errors.New("the Security log key keeps disappearing")
}

// readChainKey reads the Security log's key; fs.ErrNotExist when there is
// none yet. A file claimed but not yet written (secrets.CreateExclusive's
// fallback without hard links) is read again for up to a second.
func readChainKey(cfgDir string) ([]byte, error) {
	var b []byte
	var err error
	for range 10 {
		if b, err = secrets.ReadFile(chainKeyPath(cfgDir)); err != nil || len(bytes.TrimSpace(b)) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(b))
	var key []byte
	if sealed, ok := strings.CutPrefix(text, dpapiPrefix); ok {
		var p secrets.Protector
		if key, err = base64.StdEncoding.DecodeString(sealed); err == nil {
			if p, err = secrets.NewUserProtector(); err == nil {
				key, err = p.Unprotect(key)
			}
		}
	} else {
		key, err = hex.DecodeString(text)
	}
	if err == nil && len(key) != chainKeyLen {
		err = fmt.Errorf("%d bytes, want %d", len(key), chainKeyLen)
	}
	if err != nil {
		return nil, fmt.Errorf("the Security log key %s is unreadable: %w", chainKeyPath(cfgDir), err)
	}
	return key, nil
}

// readChainHead reads a head file (the anchor, a file witness): nil when
// there is none.
func readChainHead(path string) (*store.Head, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var h store.Head
	if err == nil {
		err = json.Unmarshal(b, &h)
	}
	if err != nil {
		return nil, fmt.Errorf("read the chain head in %s: %w", path, err)
	}
	return &h, nil
}

// writeChainAnchor writes h as the anchor: atomically, synced, owner-only.
func writeChainAnchor(cfgDir string, h store.Head) error {
	b, _ := json.Marshal(h)
	return secrets.WriteFile(chainAnchorPath(cfgDir), append(b, '\n'))
}

// witnessDM sends a text to the operator's Discord DM: the relay sets it when
// it is built, for a "discord" witness.
var witnessDM atomic.Pointer[func(string) error]

func setWitnessDM(fn func(string) error) { witnessDM.Store(&fn) }

// witnessChain also sends head to the witness logs.security.witness names,
// read live: "" none, "discord" the operator's DM, or a file path (best on
// another machine). A witness is what makes truncation and rollback visible
// to someone holding the operator's own privileges (§10.2).
func witnessChain(cfgDir string, head store.Head) error {
	lc, err := loadLaunchConfig(cfgDir)
	if err != nil {
		return err
	}
	switch w := strings.TrimSpace(lc.Logs.Security.Witness); {
	case w == "":
		return nil
	case w == "discord":
		dm := witnessDM.Load()
		if dm == nil {
			return errors.New("witness discord: no Discord relay runs in this process")
		}
		return (*dm)(fmt.Sprintf("Tenant Security log anchor: record %d, hash %s, at %s", head.Rec, head.Hash, head.Time.Format(time.RFC3339)))
	case strings.HasPrefix(w, "peer:"):
		// ponytail: no peer call stores a head yet; add one to the peering API (TEN-189/190) to allow peer witnesses.
		return fmt.Errorf("witness %s: a peer can't be a witness yet; use discord or a file", w)
	default:
		path := expandPath(w)
		if !filepath.IsAbs(path) {
			return fmt.Errorf("witness %q: want discord or an absolute file path", w)
		}
		b, _ := json.Marshal(head)
		return atomicWrite(path, append(b, '\n'))
	}
}

// anchorChain is the housekeeper task that anchors the Security log's head
// outside the database: to <config>/logbook.anchor every anchorEvery records
// or every anchorAge, and each time to the witness. The anchor only moves on
// from a head the chain still contains; when it doesn't, the database was
// truncated or rolled back, and the anchor stays where it is for verify to
// find, with one critical event.
func anchorChain(h *logs.Hub, st *store.Store, cfgDir string) func(context.Context) error {
	var alerted bool
	return func(ctx context.Context) error {
		head, err := st.Head(ctx)
		if err != nil || head.Rec == 0 {
			return err
		}
		old, err := readChainHead(chainAnchorPath(cfgDir))
		if err != nil {
			return err
		}
		if old != nil {
			if head.Rec >= old.Rec && head.Rec-old.Rec < anchorEvery && time.Since(old.Time) < anchorAge || old.Rec == head.Rec && old.Hash == head.Hash {
				return nil
			}
			ok, err := st.Contains(ctx, *old)
			if err != nil {
				return err
			}
			if !ok {
				if !alerted {
					h.Logf("logbook", logs.LevelCritical, "logbook: the Security log no longer holds its anchor: it was truncated or rolled back; run `tenant logs verify security`",
						"anchor_rec", old.Rec, "head_rec", head.Rec)
				}
				alerted = true
				return nil
			}
		}
		alerted = false
		head.Time = time.Now().UTC()
		if err := writeChainAnchor(cfgDir, head); err != nil {
			return err
		}
		return witnessChain(cfgDir, head)
	}
}

// reanchorAfterRestore writes the re-anchor record into the restored log book
// at db (the restored head, the head the anchor held before the restore, and
// the backup it came from) and moves the anchor and the witness to it, so the
// restore verifies as a restore rather than as tampering.
func reanchorAfterRestore(ctx context.Context, cfgDir, db string, replaced *store.Head, backup string) error {
	key, err := chainKey(cfgDir)
	if err != nil {
		return err
	}
	st, err := store.Open(db, store.Options{ChainKey: key})
	if err != nil {
		return err
	}
	defer st.Close()
	var was store.Head
	if replaced != nil {
		was = *replaced
	}
	head, err := st.Reanchor(ctx, was, filepath.Base(backup))
	if err != nil {
		return err
	}
	head.Time = time.Now().UTC()
	if err := writeChainAnchor(cfgDir, head); err != nil {
		return err
	}
	return witnessChain(cfgDir, head)
}

// openSecurityLog opens the install's log book with its Security log key,
// for verification and export. It never creates a key.
func openSecurityLog(c *commonFlags) (*store.Store, error) {
	key, err := readChainKey(c.cfgDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("no Security log key: this install has never chained a security record")
	}
	if err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(c.logsRoot(), "logbook.db"), store.Options{ChainKey: key})
}

// verifySecurityLog verifies the install's Security log: its archives and
// database against the key, the anchor and, when logs.security.witness is a
// file, the witness. `tenant logs verify security` (TEN-377) reports it.
func verifySecurityLog(ctx context.Context, c *commonFlags) (store.Verification, error) {
	st, err := openSecurityLog(c)
	if err != nil {
		return store.Verification{}, err
	}
	defer st.Close()
	var opts store.VerifyOptions
	if opts.Anchor, err = readChainHead(chainAnchorPath(c.cfgDir)); err != nil {
		return store.Verification{}, err
	}
	if lc, _ := loadLaunchConfig(c.cfgDir); lc != nil {
		if w := expandPath(strings.TrimSpace(lc.Logs.Security.Witness)); filepath.IsAbs(w) {
			if opts.Witness, err = readChainHead(w); err != nil {
				return store.Verification{}, err
			}
		}
	}
	return st.Verify(ctx, opts)
}

// exportSecurityLog writes the install's audit export bundle to w: the
// Security log from since to until (zero: unbounded) with its chain and the
// anchor, verifiable offline with the key (verifySecurityBundle).
func exportSecurityLog(ctx context.Context, c *commonFlags, w io.Writer, since, until time.Time) error {
	st, err := openSecurityLog(c)
	if err != nil {
		return err
	}
	defer st.Close()
	anchor, err := readChainHead(chainAnchorPath(c.cfgDir))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	return st.ExportSecurity(ctx, w, store.ExportOptions{Since: since, Until: until, Host: host, Version: version.String(), Anchor: anchor})
}

// verifySecurityBundle checks an audit export bundle with this install's key.
func verifySecurityBundle(c *commonFlags, r io.Reader) (store.BundleHeader, store.Verification, error) {
	key, err := readChainKey(c.cfgDir)
	if err != nil {
		return store.BundleHeader{}, store.Verification{}, err
	}
	return store.VerifyBundle(r, key)
}

// securityLogAPI hands the log book's API (internal/logs/logapi) the
// Security log's verify and audit export, which need this install's key,
// anchor and witness.
func securityLogAPI(c *commonFlags) logapi.SecurityLog {
	return logapi.SecurityLog{
		Verify: func(ctx context.Context) (store.Verification, error) { return verifySecurityLog(ctx, c) },
		Export: func(ctx context.Context, w io.Writer, since, until time.Time) error {
			return exportSecurityLog(ctx, c, w, since, until)
		},
	}
}
