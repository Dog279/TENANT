package store

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Verification is what a walk of the Security log's chain found.
type Verification struct {
	Records    int64     // records verified
	Head       Head      // the last of them
	Archives   int       // archive files read
	Tombstones int64     // records a privacy request emptied: still chained, their content gone
	Restores   []Restore // re-anchor records
	Notes      []string  // what the walk could not check, such as a missing anchor
	Break      *Break    // the first break; nil when the chain verifies
}

// Restore is a re-anchor record: a restore the chain accounts for.
type Restore struct {
	Rec      int64 // the re-anchor record's number
	Backup   string
	Restored Head // the head the restored database had
	Replaced Head // the anchor from before the restore
}

// Break is where the chain stops verifying, by record number.
type Break struct {
	Rec    int64
	Where  string // "logbook.db", "archive <file>", "anchor", "witness" or "bundle"
	Reason string
}

func (b *Break) Error() string {
	return fmt.Sprintf("record %d (%s): %s", b.Rec, b.Where, b.Reason)
}

// VerifyOptions are the heads kept outside the database that Verify checks
// the chain against.
type VerifyOptions struct {
	Anchor  *Head // <config>/logbook.anchor; nil when there is none
	Witness *Head // the witness's head, when the witness is a file
}

// Verify walks the Security log's chain: the archives, oldest first, then the
// database; then it checks the anchor and the witness against it. It stops at
// the first break and reports it by record number. A tombstone verifies (its
// data_hash is kept); a re-anchor record is reported; a witness that names
// the head a re-anchor replaced is a restore, not a break (the anchor itself
// moves with the restore). An error means Verify could not look, not that it
// found tampering.
//
// ponytail: one pass over every record in a single read transaction (a second
// or two at the 256 MB default); verify from a checkpoint if it ever runs often.
func (s *Store) Verify(ctx context.Context, opts VerifyOptions) (Verification, error) {
	if len(s.key) == 0 {
		return Verification{}, ErrNoChainKey
	}
	c := newChecker(s.key, opts)
	err := s.walk(ctx, c.check)
	var b *Break
	if errors.As(err, &b) {
		if b.Rec == 0 { // a row or a file that can't be read: where the walk stopped
			b.Rec = c.v.Head.Rec + 1
		}
		c.brk = b
	} else if err != nil {
		return c.v, err
	}
	c.finish(opts, false)
	return c.v, nil
}

// archiveDir is <logs root>/archive, beside the database.
func (s *Store) archiveDir(ctx context.Context) (string, error) {
	path, err := s.file(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "archive"), nil
}

// walk calls fn with every Security log record in chain order: the archives,
// by first record, then the database's rows, oldest first. where names the
// source. A record may come twice where an archive overlaps what follows it
// (a retention pass stopped between writing it and deleting its rows). An
// archive or a row that can't be read (a BLOB where text belongs) stops the
// walk with a *Break whose Rec is 0.
func (s *Store) walk(ctx context.Context, fn func(where string, l *line) error) error {
	// One snapshot of the database, taken before the archives are listed: an
	// archive written meanwhile holds rows the snapshot still has, which then
	// come twice rather than going missing.
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: verify: %w", err)
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meta`).Scan(&n); err != nil {
		return fmt.Errorf("store: verify: %w", err)
	}
	dir, err := s.archiveDir(ctx)
	if err != nil {
		return err
	}
	files, err := securityArchives(dir)
	if err != nil {
		return fmt.Errorf("store: verify: %w", err)
	}
	for _, f := range files {
		where := "archive " + filepath.Base(f.path)
		err := readLines(f.path, parsed(func(l *line) error { return fn(where, l) }))
		var b *Break
		if err != nil && !errors.As(err, &b) {
			return &Break{Where: where, Reason: "unreadable: " + err.Error()}
		}
		if err != nil {
			return err
		}
	}
	// A security row never chained comes first, then the chain in record
	// order, read along events_rec so a row that can't be read stops the walk
	// where it is.
	for _, q := range []string{`e.rec IS NULL AND e.log_id = (SELECT id FROM names WHERE kind = 'log' AND name = '` + SecurityLog + `') LIMIT 1`,
		`e.rec IS NOT NULL ORDER BY e.rec`} {
		if err := s.walkRows(ctx, tx, q, fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) walkRows(ctx context.Context, tx *sql.Tx, where string, fn func(where string, l *line) error) error {
	unreadable := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Break{Where: "logbook.db", Reason: "a row can't be read: " + err.Error()}
	}
	rows, err := tx.QueryContext(ctx, archiveSelect+` WHERE `+where)
	if err != nil {
		return unreadable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var l line
		if err := rows.Scan(new(int64), new(int64), &raw); err != nil {
			return unreadable(err)
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			return unreadable(err)
		}
		if err := fn("logbook.db", &l); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return unreadable(err)
	}
	return nil
}

type archiveFile struct {
	path        string
	first, last int64
}

// securityArchives lists the Security log's archives, security-<first
// rec>-<last rec>.ndjson.gz, by first record.
func securityArchives(dir string) ([]archiveFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []archiveFile
	for _, e := range entries {
		if log, ok := archiveLog(e.Name()); !ok || log != SecurityLog {
			continue
		}
		first, last, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(e.Name(), SecurityLog+"-"), ".ndjson.gz"), "-")
		f := archiveFile{path: filepath.Join(dir, e.Name())}
		var err1, err2 error
		f.first, err1 = strconv.ParseInt(first, 10, 64)
		f.last, err2 = strconv.ParseInt(last, 10, 64)
		if err1 == nil && err2 == nil {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b archiveFile) int { return cmp.Or(cmp.Compare(a.first, b.first), cmp.Compare(a.last, b.last)) })
	return out, nil
}

// readLines calls fn with each line of a gzip NDJSON archive, as it is: the
// bytes are good until fn returns. parsed reads them as lines.
func readLines(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	return scanLines(z, fn)
}

func scanLines(r io.Reader, fn func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// parsed is fn reading each line's JSON as a line.
func parsed(fn func(l *line) error) func([]byte) error {
	return func(b []byte) error {
		var l line
		if err := json.Unmarshal(b, &l); err != nil {
			return err
		}
		return fn(&l)
	}
}

// checker verifies records in chain order.
type checker struct {
	key  []byte
	v    Verification
	prev []byte // the hash of v.Head
	brk  *Break

	where  string
	closed bool             // the source's last line was a closing (rec, hash) line
	seen   map[int64][]byte // the archives' hashes, for records that come twice
	at     map[int64]string // the anchor's and witness's records: the hash the chain has
}

func newChecker(key []byte, opts VerifyOptions) *checker {
	c := &checker{key: key, prev: genesis, seen: map[int64][]byte{}, at: map[int64]string{}}
	for _, h := range []*Head{opts.Anchor, opts.Witness} {
		if h != nil {
			c.at[h.Rec] = ""
		}
	}
	return c
}

// check verifies one line from where; the first break stops the walk.
func (c *checker) check(where string, l *line) error {
	if where != c.where {
		if err := c.endSource(); err != nil {
			return err
		}
		c.where, c.closed = where, false
		if strings.HasPrefix(where, "archive ") {
			c.v.Archives++
		}
		for r := range c.seen { // a source overlaps only the one before it
			if r < l.rec()-1 {
				delete(c.seen, r)
			}
		}
	}
	brk := func(rec int64, format string, args ...any) error {
		return &Break{Rec: rec, Where: where, Reason: fmt.Sprintf(format, args...)}
	}
	rec := l.rec()
	if l.Log == "" && where != "logbook.db" { // a closing line
		if seen, ok := c.seen[rec]; !(ok && hex.EncodeToString(seen) == l.Hash || rec == c.v.Head.Rec && l.Hash == c.v.Head.Hash) {
			return brk(rec, "the closing line names record %d with a hash the chain doesn't have", rec)
		}
		c.closed = true
		return nil
	}
	c.closed = false
	if l.Rec == nil {
		return brk(c.v.Head.Rec+1, "a security record (seq %d) has no record number: it was never chained", l.Seq)
	}
	prev, want := c.prev, []byte(nil)
	switch {
	case rec == c.v.Head.Rec+1:
	case rec >= 1 && rec <= c.v.Head.Rec: // again: it must be the record verified before
		var ok1, ok2 bool
		if prev, ok1 = c.seen[rec-1]; rec == 1 {
			prev, ok1 = genesis, true
		}
		want, ok2 = c.seen[rec]
		if !ok1 || !ok2 {
			return brk(rec, "record %d comes again after record %d, out of order", rec, c.v.Head.Rec)
		}
	case rec > c.v.Head.Rec+1:
		return brk(c.v.Head.Rec+1, "records %d to %d are missing", c.v.Head.Rec+1, rec-1)
	default:
		return brk(rec, "record number %d is not valid", rec)
	}
	dh, err := hex.DecodeString(l.DataHash)
	if err != nil || len(dh) != len(genesis) {
		return brk(rec, "its data hash is missing or malformed")
	}
	tomb := false
	switch salt, err := hex.DecodeString(l.Salt); {
	case l.Salt != "":
		if err != nil || len(salt) != saltSize || !bytes.Equal(dataHash(salt, l.Msg, l.Data, l.Dtypes), dh) {
			return brk(rec, "its message or data was changed")
		}
	case l.Msg == Tombstone && l.Data == "" && l.Dtypes == "":
		tomb = true
	default:
		return brk(rec, "its salt is gone but it is not a privacy tombstone")
	}
	h := link(c.key, prev, l.header(rec), dh)
	if hex.EncodeToString(h) != l.Hash {
		return brk(rec, "its hash does not match: the record was changed, moved or forged, or the key is not the one that wrote it")
	}
	if want != nil {
		if !bytes.Equal(h, want) {
			return brk(rec, "it differs from the copy of record %d verified before", rec)
		}
		return nil
	}
	c.prev, c.v.Head = h, Head{Rec: rec, Hash: l.Hash}
	c.v.Records++
	if tomb {
		c.v.Tombstones++
	}
	if strings.HasPrefix(where, "archive ") {
		c.seen[rec] = h
	}
	if _, ok := c.at[rec]; ok {
		c.at[rec] = l.Hash
	}
	if l.Provider == ReanchorProvider && l.EventID == ReanchorID {
		var d struct {
			Backup       string `json:"backup"`
			RestoredRec  int64  `json:"restored_rec"`
			RestoredHash string `json:"restored_hash"`
			ReplacedRec  int64  `json:"replaced_rec"`
			ReplacedHash string `json:"replaced_hash"`
		}
		_ = json.Unmarshal([]byte(l.Data), &d)
		c.v.Restores = append(c.v.Restores, Restore{Rec: rec, Backup: d.Backup,
			Restored: Head{Rec: d.RestoredRec, Hash: d.RestoredHash}, Replaced: Head{Rec: d.ReplacedRec, Hash: d.ReplacedHash}})
	}
	return nil
}

// endSource checks that an archive or a bundle ended with its closing line.
func (c *checker) endSource() error {
	if c.where != "" && c.where != "logbook.db" && !c.closed {
		return &Break{Rec: c.v.Head.Rec + 1, Where: c.where, Reason: "it has no closing (rec, hash) line: it was cut short"}
	}
	return nil
}

// finish ends the walk and checks the anchor and the witness. Within a
// bundle, only a head inside it can be checked.
func (c *checker) finish(opts VerifyOptions, bundle bool) {
	if c.brk == nil {
		var b *Break
		if errors.As(c.endSource(), &b) {
			c.brk = b
		}
	}
	defer func() { c.v.Break = c.brk }()
	if c.brk != nil {
		return
	}
	if opts.Anchor == nil && !bundle {
		c.v.Notes = append(c.v.Notes, "no anchor: truncation and rollback of the database can't be detected")
	}
	for _, w := range []struct {
		name string
		h    *Head
	}{{"anchor", opts.Anchor}, {"witness", opts.Witness}} {
		h := w.h
		if h == nil || h.Rec == 0 {
			continue
		}
		if got := c.at[h.Rec]; got == h.Hash {
			continue
		}
		// A restore moves the anchor with its re-anchor record; a witness it
		// couldn't reach then may still name the head it replaced.
		if i := slices.IndexFunc(c.v.Restores, func(r Restore) bool { return r.Replaced.Rec == h.Rec && r.Replaced.Hash == h.Hash }); i >= 0 && w.name == "witness" {
			c.v.Notes = append(c.v.Notes, fmt.Sprintf("the %s names record %d, which the restore at record %d replaced", w.name, h.Rec, c.v.Restores[i].Rec))
			continue
		}
		switch {
		case bundle && (h.Rec > c.v.Head.Rec || c.at[h.Rec] == ""):
			c.v.Notes = append(c.v.Notes, fmt.Sprintf("the %s, at record %d, is outside this bundle", w.name, h.Rec))
		case h.Rec > c.v.Head.Rec:
			c.brk = &Break{Rec: c.v.Head.Rec + 1, Where: w.name, Reason: fmt.Sprintf(
				"the chain ends at record %d, but the %s says record %d exists: the log was truncated or rolled back", c.v.Head.Rec, w.name, h.Rec)}
			return
		default:
			c.brk = &Break{Rec: h.Rec, Where: w.name, Reason: fmt.Sprintf(
				"record %d's hash differs from the %s's: the log was rolled back and written again", h.Rec, w.name)}
			return
		}
	}
}

// An audit export bundle is the Security log with its chain, self-describing
// and verifiable offline with the key: a header line, the records as archive
// lines, and a closing (rec, hash) line.
const bundleFormat = "tenant.security-log.bundle/1"

// BundleHeader is an export bundle's first line.
type BundleHeader struct {
	Format  string    `json:"format"`
	Chain   string    `json:"chain"` // how the records are linked
	Schema  string    `json:"schema"`
	Version string    `json:"version,omitempty"` // Tenant's, whose catalog the event IDs are from
	Time    time.Time `json:"time"`
	Host    string    `json:"host,omitempty"`
	StoreID string    `json:"store_id"`
	Since   time.Time `json:"since,omitzero"` // the filter
	Until   time.Time `json:"until,omitzero"`
	Prev    Head      `json:"prev"` // the head before the first record: where verification starts
	Anchor  *Head     `json:"anchor,omitempty"`
}

// ExportOptions selects what ExportSecurity writes. The records run from the
// first at or after Since to the last before the first at or after Until
// (zero: unbounded): a contiguous stretch of the chain, so it verifies.
type ExportOptions struct {
	Since, Until  time.Time
	Host, Version string
	Anchor        *Head
}

// ExportSecurity writes an audit export bundle of the Security log to w,
// sensitive fields included (the data hash commits to them). It does not
// verify the chain; VerifyBundle does, with the key.
func (s *Store) ExportSecurity(ctx context.Context, w io.Writer, opts ExportOptions) error {
	version, id := meta(ctx, s.r)
	hdr := BundleHeader{Format: bundleFormat, Schema: version, Version: opts.Version, Time: time.Now().UTC(), Host: opts.Host,
		StoreID: id, Since: opts.Since, Until: opts.Until, Anchor: opts.Anchor,
		Chain: "hash = HMAC-SHA256(key, prev hash ‖ header ‖ data_hash); data_hash = SHA-256(salt ‖ msg ‖ data ‖ dtypes); layout in docs/LOGGING.md"}
	enc := json.NewEncoder(w)
	started, done := false, false
	var last Head
	err := s.walk(ctx, func(_ string, l *line) error {
		t := time.Unix(0, l.TimeNS)
		switch {
		case l.Log == "" || l.rec() <= last.Rec || done: // a closing line, or a record again
			return nil
		case !started && !opts.Since.IsZero() && t.Before(opts.Since):
			last = Head{Rec: l.rec(), Hash: l.Hash}
			return nil
		case !opts.Until.IsZero() && !t.Before(opts.Until):
			done = true
			return nil
		}
		if !started {
			started, hdr.Prev = true, last
			if err := enc.Encode(hdr); err != nil {
				return err
			}
		}
		last = Head{Rec: l.rec(), Hash: l.Hash}
		return enc.Encode(l)
	})
	if err != nil {
		return err
	}
	if !started {
		hdr.Prev = last
		if err := enc.Encode(hdr); err != nil {
			return err
		}
	}
	return enc.Encode(Head{Rec: last.Rec, Hash: last.Hash})
}

// VerifyBundle checks an audit export bundle with the key: every record's
// data hash and link from the header's prev head on, the closing line, and
// the anchor when it falls inside the bundle.
func VerifyBundle(r io.Reader, key []byte) (BundleHeader, Verification, error) {
	var hdr BundleHeader
	br := bufio.NewReader(r)
	first, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return hdr, Verification{}, err
	}
	if err := json.Unmarshal(first, &hdr); err != nil || hdr.Format != bundleFormat {
		return hdr, Verification{}, fmt.Errorf("store: not a Security log bundle (want format %s)", bundleFormat)
	}
	opts := VerifyOptions{Anchor: hdr.Anchor}
	c := newChecker(key, opts)
	if c.prev, err = prevHash(hdr.Prev); err != nil {
		return hdr, Verification{}, err
	}
	c.v.Head = hdr.Prev
	c.seen[hdr.Prev.Rec] = c.prev // an empty bundle's closing line names it
	err = scanLines(br, parsed(func(l *line) error { return c.check("bundle", l) }))
	var b *Break
	switch {
	case errors.As(err, &b):
		c.brk = b
	case err != nil:
		c.brk = &Break{Rec: c.v.Head.Rec + 1, Where: "bundle", Reason: "unreadable: " + err.Error()}
	case c.where == "": // not even a closing line
		c.brk = &Break{Rec: c.v.Head.Rec + 1, Where: "bundle", Reason: "it has no closing (rec, hash) line: it was cut short"}
	}
	c.finish(opts, true)
	return hdr, c.v, nil
}
