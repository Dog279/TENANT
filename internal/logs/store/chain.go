package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tenant/internal/logs/event"
)

// ErrNoChainKey refuses a commit that holds a security record when the store
// was opened without a chain key: no security record is written unchained.
var ErrNoChainKey = errors.New("store: no Security log chain key; the security record was not written")

// Head is a point in the Security log's chain: a record's number and its
// hash in lowercase hex. The meta table keeps the chain's head as one, and
// the anchor file, a witness and an archive's closing line hold one as JSON.
type Head struct {
	Rec  int64     `json:"rec"`
	Hash string    `json:"hash"`
	Time time.Time `json:"time,omitzero"` // when an anchor was taken
}

// The re-anchor record (§10.2): Reanchor writes it into the Security log after
// a restore, and Verify reports it.
const (
	ReanchorProvider = "logbook"
	ReanchorID       = 4001
)

const saltSize = 16

// line is a record as an archive line holds it (archiveSelect). The chain
// reads a row it links or verifies in the same form, so the database, the
// archives and an export bundle all hash alike. An archive's closing line
// has only rec and hash.
type line struct {
	Rec         *int64 `json:"rec"` // nil: never chained
	Seq         int64  `json:"seq"`
	TimeNS      int64  `json:"time_ns"`
	Log         string `json:"log"`
	Provider    string `json:"provider"`
	EventID     int64  `json:"event_id"`
	Version     int64  `json:"version"`
	Level       int64  `json:"level"`
	Channel     int64  `json:"channel"`
	Task        string `json:"task"`
	Opcode      string `json:"opcode"`
	Keywords    string `json:"keywords"` // the column's JSON text
	Msg         string `json:"msg"`
	Data        string `json:"data"`
	Dtypes      string `json:"dtypes"`
	Trace       string `json:"trace_id"`
	Activity    string `json:"activity_id"`
	Related     string `json:"related_id"`
	Session     string `json:"session_id"`
	Approval    string `json:"approval_id"`
	ToolCall    string `json:"tool_call_id"`
	Actor       string `json:"actor"`
	Surface     string `json:"surface"`
	PID         int64  `json:"pid"`
	Role        string `json:"role"`
	Boot        string `json:"boot_id"`
	Host        string `json:"host"`
	ProcVersion string `json:"proc_version"`
	Salt        string `json:"salt"`
	DataHash    string `json:"data_hash"`
	Hash        string `json:"hash"`
}

func (l *line) rec() int64 {
	if l.Rec == nil {
		return 0
	}
	return *l.Rec
}

// header is the canonical header rec's link covers (the package doc has the
// layout).
func (l *line) header(rec int64) []byte {
	b := make([]byte, 0, 256)
	num := func(n int64) { b = binary.BigEndian.AppendUint64(b, uint64(n)) }
	str := func(s string) { b = append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...) }
	num(rec)
	num(l.TimeNS)
	str(l.Log)
	str(l.Provider)
	num(l.EventID)
	num(l.Version)
	num(l.Level)
	num(l.Channel)
	for _, s := range []string{l.Task, l.Opcode, l.Keywords, l.Trace, l.Activity, l.Related, l.Session, l.Approval, l.ToolCall, l.Actor, l.Surface} {
		str(s)
	}
	num(l.PID)
	for _, s := range []string{l.Role, l.Boot, l.Host, l.ProcVersion} {
		str(s)
	}
	return b
}

// dataHash is the salted commitment to a record's content.
func dataHash(salt []byte, msg, data, dtypes string) []byte {
	h := sha256.New()
	h.Write(salt)
	for _, s := range []string{msg, data, dtypes} {
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(s))))
		h.Write([]byte(s))
	}
	return h.Sum(nil)
}

// link is a record's hash.
func link(key, prev, header, dataHash []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(prev)
	m.Write(header)
	m.Write(dataHash)
	return m.Sum(nil)
}

// genesis is record 1's previous hash.
var genesis = make([]byte, sha256.Size)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// chainHead reads the chain's head from meta: the zero Head before the first
// record.
func chainHead(ctx context.Context, q queryer) (Head, error) {
	var h Head
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'chain_head'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(v), &h)
	}
	if err != nil {
		return h, fmt.Errorf("chain head: %w", err)
	}
	return h, nil
}

// prevHash is h's hash as bytes: genesis for the zero Head.
func prevHash(h Head) ([]byte, error) {
	if h.Rec == 0 {
		return genesis, nil
	}
	b, err := hex.DecodeString(h.Hash)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("chain head %d: bad hash %q", h.Rec, h.Hash)
	}
	return b, nil
}

// chain links the security record at seq into the chain, inside the
// transaction that commits it: its record number from the head in meta, a
// fresh salt, its data hash and its HMAC; then the head moves to it.
func (s *Store) chain(ctx context.Context, tx *sql.Tx, seq int64, _ *row) error {
	if len(s.key) == 0 {
		return ErrNoChainKey
	}
	var l line
	var raw []byte
	if err := tx.QueryRowContext(ctx, archiveSelect+` WHERE e.seq = ?`, seq).Scan(&seq, new(int64), &raw); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	head, err := chainHead(ctx, tx)
	if err != nil {
		return err
	}
	prev, err := prevHash(head)
	if err != nil {
		return err
	}
	rec := head.Rec + 1
	salt := make([]byte, saltSize)
	_, _ = rand.Read(salt) // crypto/rand never fails (Go 1.24+)
	dh := dataHash(salt, l.Msg, l.Data, l.Dtypes)
	h := link(s.key, prev, l.header(rec), dh)
	if _, err := tx.ExecContext(ctx, `UPDATE events SET rec = ?, salt = ?, data_hash = ?, hash = ? WHERE seq = ?`, rec, salt, dh, h, seq); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	next, _ := json.Marshal(Head{Rec: rec, Hash: hex.EncodeToString(h)})
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('chain_head', ?)`, string(next)); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	return nil
}

// Head is the Security log's chain head: its newest record's number and
// hash, the zero Head when it has none.
func (s *Store) Head(ctx context.Context) (Head, error) {
	h, err := chainHead(ctx, s.r)
	if err != nil {
		return h, fmt.Errorf("store: %w", err)
	}
	return h, nil
}

// Reanchor writes the re-anchor record after a restore (§10.2): the restored
// head, the head it replaced (the anchor from before the restore, the zero
// Head when there was none) and the backup it came from. It returns the new
// head, for the caller to write as the anchor, so a restore reads as a
// restore rather than as tampering.
func (s *Store) Reanchor(ctx context.Context, replaced Head, backup string) (Head, error) {
	restored, err := s.Head(ctx)
	if err != nil {
		return Head{}, err
	}
	e := event.Event{Log: SecurityLog, Provider: ReanchorProvider, ID: ReanchorID, Level: event.Warning, Task: "Integrity",
		Msg: fmt.Sprintf("The Security log was restored from %s to record %d and re-anchored; the anchor before the restore was at record %d", backup, restored.Rec, replaced.Rec),
		Data: []event.Field{{Key: "backup", Value: backup}, {Key: "restored_rec", Value: restored.Rec}, {Key: "restored_hash", Value: restored.Hash},
			{Key: "replaced_rec", Value: replaced.Rec}, {Key: "replaced_hash", Value: replaced.Hash}}}
	if err := s.Commit(ctx, []event.Event{e}); err != nil {
		return Head{}, err
	}
	return s.Head(ctx)
}

// Contains reports whether the chain holds record h.Rec with hash h.Hash: in
// the database, or else in an archive. The anchor may only move forward from
// a head the chain still contains; otherwise the database was truncated or
// rolled back.
func (s *Store) Contains(ctx context.Context, h Head) (bool, error) {
	var hash []byte
	err := s.r.QueryRowContext(ctx, `SELECT hash FROM events WHERE rec = ?`, h.Rec).Scan(&hash)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return err == nil && hex.EncodeToString(hash) == h.Hash, err
	}
	dir, err := s.archiveDir(ctx)
	if err != nil {
		return false, err
	}
	files, err := securityArchives(dir)
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	found := false
	for _, f := range files {
		if f.first > h.Rec || h.Rec > f.last {
			continue
		}
		err := readLines(f.path, func(l *line) error {
			found = found || l.Log != "" && l.rec() == h.Rec && l.Hash == h.Hash
			return nil
		})
		if err != nil {
			return false, fmt.Errorf("store: %s: %w", f.path, err)
		}
	}
	return found, nil
}
