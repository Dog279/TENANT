package fleet

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/datalock"
)

// A child keeps its own record of what its master had it do: the audit log,
// in <child dir>/audit. Every tool call is written there twice, when it
// arrives and when it has ended, by the gate (gate.go), and each record goes
// to the operating system's log as one line as well.
//
// The file is JSON, one record per line. A record carries the SHA-256 of the
// line before it (prev), and a record's own hash is the SHA-256 of its line.
// So a line that is changed, removed or put in after the fact breaks the
// chain from there on, which VerifyAudit reports. That is tamper-evident and
// no more: whoever can write the file can write a whole new chain, and can
// cut records off its end. What stands against that is outside the file: the
// master keeps the hash of each call's record with its own record of the
// call, and the operating system's log has a line per record.
//
// A file that has grown past maxAuditFile is closed and kept under the
// number of its last record (audit-<seq>.jsonl), and the chain goes on in a
// new audit.jsonl.
// ponytail: closed files are never deleted here; the operator archives or
// removes them. Add a retention setting if a child's disk ever shows it.
const (
	auditDirName  = "audit"
	auditFileName = "audit.jsonl"
	auditLockName = "audit.lock"
	auditOldPre   = "audit-"
	auditOldSuf   = ".jsonl"

	maxAuditFile = 8 << 20  // a file past this is closed at the next record
	maxAuditLine = 64 << 10 // no record is longer; a longer line is damage
	maxAuditArgs = 2 << 10  // how much of a call's arguments a record keeps
	maxAuditAsks = 32       // approvals listed per call; the rest are counted
)

// auditStart is the prev of the first record ever written.
var auditStart = strings.Repeat("0", 2*sha256.Size)

// The two records of a call.
const (
	// AuditCall: a call arrived. Written before anything runs.
	AuditCall = "call"
	// AuditDone: how the call ended.
	AuditDone = "done"
	// AuditRehome: someone on this machine gave it another master
	// (tenant child rehome). No call goes with it.
	AuditRehome = "rehome"
	// AuditUpdates: someone on this machine changed whose word an update is
	// taken on (tenant child updates): From and To are the rules.
	AuditUpdates = "updates"
)

// How a call ended, in its AuditDone record.
const (
	AuditOK      = "ok"
	AuditError   = "error"   // the tool ran and reported a failure
	AuditRefused = "refused" // this machine did not run it
)

// What became of one approval a tool asked for.
const (
	AskAllowed = "allowed" // the master said yes
	AskDenied  = "denied"  // the master did not say yes, or could not be asked
	AskRefused = "refused" // this machine refused without asking: see Why
)

// AuditAsk is one approval a tool asked for during a call.
type AuditAsk struct {
	Action string `json:"action"`
	Answer string `json:"answer"`
	Why    string `json:"why,omitempty"`
}

// AuditRecord is one line of the audit log.
type AuditRecord struct {
	Seq   uint64    `json:"seq"` // counts every record from 1, across files
	Time  time.Time `json:"time"`
	Event string    `json:"event"`
	Call  string    `json:"call"` // the master's id for the call
	Tool  string    `json:"tool"`
	// AuditCall: the start of the arguments, and the SHA-256 of all of them.
	Args       string `json:"args,omitempty"`
	ArgsSHA256 string `json:"args_sha256,omitempty"`
	// AuditDone.
	Asked        []AuditAsk `json:"asked,omitempty"`
	AskedMore    int        `json:"asked_more,omitempty"` // approvals asked beyond those listed
	Outcome      string     `json:"outcome,omitempty"`
	Note         string     `json:"note,omitempty"` // why a call was refused
	ResultSHA256 string     `json:"result_sha256,omitempty"`
	ResultBytes  int        `json:"result_bytes,omitempty"`
	// AuditRehome: the account that did it, and the certificate fingerprints
	// of the master it left and of the one it trusts now.
	By   string `json:"by,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Prev is the hash of the line before this one.
	Prev string `json:"prev"`
}

// parseAuditRecord reads one line of the log strictly: a line that is not
// exactly a record is not one.
func parseAuditRecord(line []byte) (AuditRecord, error) {
	var r AuditRecord
	if len(line) > maxAuditLine {
		return r, errors.New("longer than a record")
	}
	if err := decodeStrict(line, &r); err != nil {
		return AuditRecord{}, err
	}
	if _, err := parseFingerprint(r.Prev); err != nil || r.Seq == 0 || r.Time.IsZero() ||
		(r.Event != AuditCall && r.Event != AuditDone && r.Event != AuditRehome && r.Event != AuditUpdates) {
		return AuditRecord{}, errors.New("not an audit record")
	}
	return r, nil
}

// auditOSLine is a record as it goes to the operating system's log: what
// happened and the record's hash, and nothing a tool was given or gave back.
// Those logs are read by more accounts than the audit log is.
func auditOSLine(r AuditRecord, hash string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet audit seq=%d event=%s call=%s tool=%s", r.Seq, r.Event, osLogWord(r.Call), osLogWord(r.Tool))
	if r.Outcome != "" {
		fmt.Fprintf(&b, " outcome=%s", osLogWord(r.Outcome))
	}
	fmt.Fprintf(&b, " sha256=%s", hash)
	return b.String()
}

// osLogWord keeps a value one word of plain ASCII, so a line of the system
// log cannot be split or forged by what a master named a tool.
func osLogWord(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > maxToolLen {
		s = s[:maxToolLen]
	}
	return strings.Map(func(r rune) rune {
		if r <= ' ' || r >= 0x7f || r == '=' || r == '"' {
			return '_'
		}
		return r
	}, s)
}

// AuditDir is where the child in dir keeps its audit log.
func AuditDir(dir string) string { return filepath.Join(dir, auditDirName) }

// AuditLog is a child's audit log, open for writing. One process holds it.
type AuditLog struct {
	dir   string
	oslog func(line string)
	now   func() time.Time
	max   int64 // a file past this size is closed at the next record

	mu     sync.Mutex
	closed bool
	lock   *datalock.Lock
	f      *os.File // nil until the log could be opened, and after a failed write
	size   int64
	seq    uint64
	prev   string
}

// ErrAuditHeld: another process is writing this child's audit log, which
// means another child is running on the same directory.
var ErrAuditHeld = errors.New("fleet: another child process is running on this directory: it holds the audit log")

// OpenAudit opens the audit log of the child in dir for writing, creating
// it, and takes it for this process: a second child on the same directory is
// refused with ErrAuditHeld, since two writers would break the chain. oslog,
// when not nil, is given each record as a line for the operating system's
// log.
//
// A log that cannot be opened for any other reason is returned all the same:
// Ready says why, every record is refused for as long as that lasts, and
// each one tries again. A child in that state refuses its master's calls and
// says so, which tells more than a child that will not start.
func OpenAudit(dir string, oslog func(line string)) (*AuditLog, error) {
	l := &AuditLog{dir: AuditDir(dir), oslog: oslog, now: time.Now, max: maxAuditFile}
	// AuditHeld, asked by a `tenant child status` at this very moment, holds
	// the lock for an instant; a child is not kept from starting by that.
	for try := 0; ; try++ {
		err := l.open()
		if !errors.Is(err, ErrAuditHeld) {
			return l, nil
		}
		if try == 5 {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AuditHeld reports whether a process holds the audit log of the child in
// dir for writing: whether a child is running there. It creates nothing.
func AuditHeld(dir string) bool {
	path := filepath.Join(AuditDir(dir), auditLockName)
	if !fileExists(path) {
		return false
	}
	lock, err := datalock.ExclusiveFile(path)
	if err != nil {
		return errors.Is(err, datalock.ErrBusy)
	}
	_ = lock.Release()
	return false
}

// Ready reports whether a record can be written now, opening the log if it
// is not open.
func (l *AuditLog) Ready() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return nil
	}
	return l.open()
}

// Close ends writing and lets another process have the log.
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	var err error
	if l.f != nil {
		err, l.f = l.f.Close(), nil
	}
	_ = l.lock.Release() // a nil lock releases nothing
	l.lock = nil
	return err
}

// open takes the log for this process if it has not, opens audit.jsonl and
// finds where the chain stands: the number of the last record and the hash
// of the last line. A file with no record of its own goes on counting from
// the newest closed one. Caller holds l.mu or is OpenAudit.
func (l *AuditLog) open() error {
	if l.closed {
		return errors.New("fleet: the audit log is closed")
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("fleet: audit log: %w", err)
	}
	if l.lock == nil {
		lock, err := datalock.ExclusiveFile(filepath.Join(l.dir, auditLockName))
		if errors.Is(err, datalock.ErrBusy) {
			return ErrAuditHeld
		}
		if err != nil {
			return fmt.Errorf("fleet: audit log: %w", err)
		}
		l.lock = lock
	}
	f, err := os.OpenFile(filepath.Join(l.dir, auditFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fleet: audit log: %w", err)
	}
	fail := func(err error) error {
		_ = f.Close()
		return fmt.Errorf("fleet: audit log: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	size := fi.Size()
	// Reading the file through leaves it at its end, where records go. It is
	// not opened for appending only: Windows would then refuse to take back a
	// write that failed half way.
	seq, prev, torn, err := auditEnd(f)
	if err == nil {
		_, err = f.Seek(0, io.SeekEnd)
	}
	if err != nil {
		return fail(err)
	}
	if old := auditFiles(l.dir, false); seq == 0 && len(old) > 0 {
		of, err := os.Open(old[len(old)-1])
		if err != nil {
			return fail(err)
		}
		oldSeq, oldPrev, _, err := auditEnd(of)
		_ = of.Close()
		if err != nil {
			return fail(err)
		}
		if seq = oldSeq; size == 0 {
			prev = oldPrev
		}
	}
	if torn {
		// A write that a crash cut short. The line stays as it is, ended here,
		// and the next record chains to it.
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return fail(err)
		}
		size++
	}
	l.f, l.size, l.seq, l.prev = f, size, seq, prev
	return nil
}

// auditEnd reads a log file through: the number of its last record, the
// hash of its last line, and whether that line lacks its newline.
func auditEnd(r io.Reader) (seq uint64, prev string, torn bool, err error) {
	prev = auditStart
	torn, err = auditLines(r, func(line []byte, hash string) error {
		if rec, err := parseAuditRecord(line); err == nil {
			seq = rec.Seq
		}
		prev = hash
		return nil
	})
	return seq, prev, torn, err
}

// auditLines reads a log file line by line, giving fn each line without its
// newline, and the line's hash. A line longer than a record is given as nil,
// its hash still that of all its bytes, so no line of any length is held in
// memory. torn reports that the last line has no newline.
func auditLines(r io.Reader, fn func(line []byte, hash string) error) (torn bool, err error) {
	br := bufio.NewReaderSize(r, 32<<10)
	for {
		h := sha256.New()
		var line []byte
		long, some, ended := false, false, false
		for !ended {
			piece, err := br.ReadSlice('\n')
			some = some || len(piece) > 0
			switch err {
			case nil:
				piece, ended = piece[:len(piece)-1], true
			case bufio.ErrBufferFull:
			case io.EOF:
				if !some {
					return false, nil
				}
				torn, ended = true, true
			default:
				return false, err
			}
			h.Write(piece)
			if long = long || len(line)+len(piece) > maxAuditLine; !long {
				line = append(line, piece...)
			}
		}
		if long {
			line = nil
		} else if line == nil {
			line = []byte{}
		}
		if err := fn(line, hex.EncodeToString(h.Sum(nil))); err != nil || torn {
			return torn, err
		}
	}
}

// auditFiles lists the log's files in dir, oldest first: the closed ones by
// the number in their name, then, with current, audit.jsonl if it is there.
func auditFiles(dir string, current bool) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), auditOldPre)
		if n, ok2 := strings.CutSuffix(n, auditOldSuf); ok && ok2 && len(n) == 20 && strings.Trim(n, "0123456789") == "" {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(out) // the numbers are padded, so names sort as they count
	if current {
		if p := filepath.Join(dir, auditFileName); fileExists(p) {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// append writes one record and returns its hash. The record is on the disk
// when it returns: a call is not run, and a result not handed back, on the
// strength of a record a power cut could still lose. After a failure nothing
// is written until the log can be opened again, which every later append
// tries.
func (l *AuditLog) append(rec AuditRecord) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := l.open(); err != nil {
			return "", err
		}
	}
	if l.size >= l.max {
		if err := l.rotate(); err != nil {
			return "", err
		}
	}
	rec.Seq, rec.Time, rec.Prev = l.seq+1, l.now().UTC().Truncate(time.Millisecond), l.prev
	line, err := encode(rec)
	if err == nil && len(line) > maxAuditLine {
		err = errors.New("the record is longer than a record may be")
	}
	if err != nil {
		return "", fmt.Errorf("fleet: audit log: %w", err)
	}
	if _, err = l.f.Write(append(line, '\n')); err == nil {
		err = l.f.Sync()
	}
	if err != nil {
		// What reached the file, if anything, is not known. Take the piece back
		// and read the file again before the next record.
		_ = l.f.Truncate(l.size)
		_ = l.f.Close()
		l.f = nil
		return "", fmt.Errorf("fleet: audit log: %w", err)
	}
	hash := fingerprintOf(line) // the SHA-256 of the line, in hex
	l.size, l.seq, l.prev = l.size+int64(len(line))+1, rec.Seq, hash
	if l.oslog != nil {
		l.oslog(auditOSLine(rec, hash))
	}
	return hash, nil
}

// Rehome writes the record of this machine being given another master: by
// is the account that ran the command, from and to the certificate
// fingerprints of the old master and the new one. It is on the disk, and in
// the system log, when it returns.
func (l *AuditLog) Rehome(by, from, to string) error {
	_, err := l.append(AuditRecord{Event: AuditRehome, By: by, From: from, To: to})
	return err
}

// UpdatesChanged writes the record of a change of the update rule: by is the
// account that made it.
func (l *AuditLog) UpdatesChanged(by, from, to string) error {
	_, err := l.append(AuditRecord{Event: AuditUpdates, By: by, From: from, To: to})
	return err
}

// rotate closes the full file under the number of its last record and
// starts a new one. The chain goes on: l.seq and l.prev stay.
func (l *AuditLog) rotate() error {
	cur := filepath.Join(l.dir, auditFileName)
	old := filepath.Join(l.dir, fmt.Sprintf("%s%020d%s", auditOldPre, l.seq, auditOldSuf))
	if err := l.f.Close(); err != nil {
		l.f = nil
		return fmt.Errorf("fleet: audit log: %w", err)
	}
	l.f = nil
	if err := os.Rename(cur, old); err != nil {
		return fmt.Errorf("fleet: audit log: %w", err)
	}
	seq, prev := l.seq, l.prev
	if err := l.open(); err != nil {
		return err
	}
	l.seq, l.prev = seq, prev
	return nil
}

// AuditSummary is what VerifyAudit found in a log whose chain holds.
type AuditSummary struct {
	Files    int
	Records  int
	First    uint64 // the number of the first record still there; 0 for an empty log
	Last     uint64
	LastHash string // the hash of the last line: what the next record will chain to
	// Torn counts lines that are not records but that the chain runs through:
	// writes a crash cut short.
	Torn int
}

// auditCheck checks a log's chain line by line.
type auditCheck struct {
	sum  AuditSummary
	prev string // the hash of the line before; "" before the first line
}

// line takes the next line of the log. where names it in an error.
func (c *auditCheck) line(where string, line []byte, hash string) error {
	rec, err := parseAuditRecord(line)
	if err != nil {
		// A line that is not a record is chained to like any other, so the
		// record after it tells whether it was here when that one was written.
		c.sum.Torn++
		c.prev = hash
		return nil
	}
	switch {
	case c.prev == "" && rec.Seq == 1 && rec.Prev != auditStart:
		return fmt.Errorf("fleet: audit log: %s: the first record does not start a chain", where)
	case c.prev != "" && rec.Prev != c.prev:
		return fmt.Errorf("fleet: audit log: %s: record %d does not follow the line before it: the log was changed here", where, rec.Seq)
	case c.sum.Last != 0 && rec.Seq != c.sum.Last+1:
		return fmt.Errorf("fleet: audit log: %s: record %d follows record %d: records are missing or were put in", where, rec.Seq, c.sum.Last)
	}
	if c.sum.First == 0 {
		c.sum.First = rec.Seq
	}
	c.sum.Records++
	c.sum.Last, c.prev = rec.Seq, hash
	return nil
}

// VerifyAudit reads the whole audit log of the child in dir and checks its
// chain: every record names the hash of the line before it, and the records
// count up by one. The error names the first place where that fails. A log
// whose oldest files the operator removed still verifies from its first
// record on.
//
// It only reads, so it may run beside the child that is writing: a record
// being written at that moment can show as a torn last line.
func VerifyAudit(dir string) (AuditSummary, error) {
	var c auditCheck
	for _, path := range auditFiles(AuditDir(dir), true) {
		f, err := os.Open(path)
		if err != nil {
			return c.sum, fmt.Errorf("fleet: audit log: %w", err)
		}
		c.sum.Files++
		n := 0
		_, err = auditLines(f, func(line []byte, hash string) error {
			n++
			return c.line(fmt.Sprintf("%s line %d", filepath.Base(path), n), line, hash)
		})
		_ = f.Close()
		if err != nil {
			return c.sum, err
		}
	}
	c.sum.LastHash = c.prev
	return c.sum, nil
}
