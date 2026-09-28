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
	"tenant/internal/logs/catalog"
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
// random bytes, written once with secrets.CreateExclusive (owner-only; on a
// Windows service install also the machine's Administrators) and sealed with
// the user protector (DPAPI, machine-scoped on such an install, so serve and
// its operator both open it) where the platform has one. Racing processes
// agree on the first key.
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

// readClaimed reads a file secrets.CreateExclusive creates, through read:
// secrets.ReadFile, which hardens the file, or secrets.Peek for a report that
// changes nothing. A file claimed but not yet written (CreateExclusive's
// fallback without hard links) is read again for up to a second while it is
// empty.
func readClaimed(path string, read func(string) ([]byte, error)) (b []byte, err error) {
	for range 10 {
		if b, err = read(path); err != nil || len(bytes.TrimSpace(b)) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return b, err
}

// readChainKey reads the Security log's key; fs.ErrNotExist when there is
// none yet.
func readChainKey(cfgDir string) ([]byte, error) { return readChainKeyWith(cfgDir, secrets.ReadFile) }

// readChainKeyWith reads the key through read (readClaimed's).
func readChainKeyWith(cfgDir string, read func(string) ([]byte, error)) ([]byte, error) {
	b, err := readClaimed(chainKeyPath(cfgDir), read)
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
	switch w := witnessOf(lc); {
	case w.Kind == "":
		return nil
	case w.Kind == "discord":
		dm := witnessDM.Load()
		if dm == nil {
			return errors.New("witness discord: no Discord relay runs in this process")
		}
		return (*dm)(fmt.Sprintf("Tenant Security log anchor: record %d, hash %s, at %s", head.Rec, head.Hash, head.Time.Format(time.RFC3339)))
	case w.Kind == "peer":
		// ponytail: no peer call stores a head yet; add one to the peering API (TEN-189/190) to allow peer witnesses.
		return fmt.Errorf("witness %s: a peer can't be a witness yet; use discord or a file", w.Target)
	case w.Problem != "":
		return fmt.Errorf("witness %q: want discord or an absolute file path", w.Target)
	default:
		b, _ := json.Marshal(head)
		return atomicWrite(w.Target, append(b, '\n'))
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

// chainVerifyOptions reads what the Security log is verified against: the
// anchor and, when w is a usable file, the witness's head.
func chainVerifyOptions(cfgDir string, w witnessAudit) (store.VerifyOptions, error) {
	var opts store.VerifyOptions
	var err error
	if opts.Anchor, err = readChainHead(chainAnchorPath(cfgDir)); err == nil && w.Kind == "file" && w.Problem == "" {
		opts.Witness, err = readChainHead(w.Target)
	}
	return opts, err
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
	lc, _ := loadLaunchConfig(c.cfgDir)
	opts, err := chainVerifyOptions(c.cfgDir, witnessOf(lc))
	if err != nil {
		return store.Verification{}, err
	}
	v, err := st.Verify(ctx, opts)
	if err == nil {
		d, details := catalog.LogVerified, fmt.Sprintf("%d records, %d archives, %d restores", v.Records, v.Archives, len(v.Restores))
		if v.Break != nil {
			d, details = catalog.LogVerifyFailed, v.Break.Error()
		}
		_ = catalog.AuditLogAdmin(ctx, d, store.SecurityLog, details, actorOperator, surfaceCLI) // tenant logs verify
	}
	return v, err
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
	if err := st.ExportSecurity(ctx, w, store.ExportOptions{Since: since, Until: until, Host: host, Version: version.String(), Anchor: anchor}); err != nil {
		return err
	}
	details := "audit bundle"
	if !since.IsZero() {
		details += ", since " + since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		details += ", until " + until.UTC().Format(time.RFC3339)
	}
	_ = catalog.AuditLogAdmin(ctx, catalog.LogExported, store.SecurityLog, details, actorOperator, surfaceCLI) // tenant logs export
	return nil
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
