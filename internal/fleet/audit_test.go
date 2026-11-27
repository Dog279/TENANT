package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openAudit opens the audit log of the child in dir and closes it when the
// test ends. Its lines for the system log go to the returned slice: no test
// writes to this machine's own.
func openAudit(t *testing.T, dir string) (*AuditLog, *[]string) {
	t.Helper()
	lines := new([]string)
	l, err := OpenAudit(dir, func(line string) { *lines = append(*lines, line) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, lines
}

// fill writes n call records and returns the hash of each.
func fill(t *testing.T, l *AuditLog, n int) []string {
	t.Helper()
	var hashes []string
	for i := range n {
		h, err := l.append(AuditRecord{Event: AuditCall, Call: fmt.Sprintf("c%d", i), Tool: "os_exec", Args: `{"command":"uptime"}`})
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	return hashes
}

func auditFile(dir string) string { return filepath.Join(AuditDir(dir), auditFileName) }

// readLines is the current audit file's lines, without the newlines.
func readLines(t *testing.T, dir string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(auditFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

func writeLines(t *testing.T, dir string, lines [][]byte) {
	t.Helper()
	if err := os.WriteFile(auditFile(dir), append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- what must be found ---

// Every way of changing a log in the middle breaks its chain at that place.
func TestAuditTamperingIsFound(t *testing.T) {
	for name, tamper := range map[string]func(lines [][]byte) [][]byte{
		"a record changed": func(l [][]byte) [][]byte {
			l[2] = bytes.Replace(l[2], []byte("uptime"), []byte("upside"), 1)
			return l
		},
		"a record removed": func(l [][]byte) [][]byte { return append(l[:2], l[3:]...) },
		"two records swapped": func(l [][]byte) [][]byte {
			l[2], l[3] = l[3], l[2]
			return l
		},
		"a record put in": func(l [][]byte) [][]byte {
			forged := must(encode(AuditRecord{Seq: 3, Time: time.Now().UTC(), Event: AuditCall, Call: "x", Tool: "os_exec", Prev: auditStart}))(t)
			return append(l[:2], append([][]byte{forged}, l[2:]...)...)
		},
		"a record put in with the right prev": func(l [][]byte) [][]byte {
			// It chains to the line before it, and so the record after it no
			// longer does.
			forged := must(encode(AuditRecord{Seq: 3, Time: time.Now().UTC(), Event: AuditCall, Call: "x", Tool: "os_exec", Prev: sum256(l[1])}))(t)
			return append(l[:2], append([][]byte{forged}, l[2:]...)...)
		},
		"a record replaced by another line": func(l [][]byte) [][]byte {
			l[2] = []byte("nothing happened here")
			return l
		},
		"the start cut off and renumbered": func(l [][]byte) [][]byte {
			first := must(parseAuditRecord(l[2]))(t)
			first.Seq = 1
			l[2] = must(encode(first))(t)
			return l[2:]
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l, _ := openAudit(t, dir)
			fill(t, l, 6)
			_ = l.Close()
			writeLines(t, dir, tamper(readLines(t, dir)))
			if sum, err := VerifyAudit(dir); err == nil {
				t.Fatalf("the log verifies after tampering: %+v", sum)
			}
		})
	}
}

// What the chain cannot show, written down: records cut off the end. The
// hash of the last record, which the master holds, is what gives it away.
func TestAuditTailCutIsNotFoundByTheChainAlone(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAudit(t, dir)
	hashes := fill(t, l, 6)
	_ = l.Close()
	writeLines(t, dir, readLines(t, dir)[:4])
	sum, err := VerifyAudit(dir)
	if err != nil || sum.Records != 4 {
		t.Fatalf("a log cut at its end = %+v, %v; want four records that verify", sum, err)
	}
	if sum.LastHash == hashes[5] || sum.LastHash != hashes[3] {
		t.Errorf("last hash %s: the master's copy of %s is what shows the cut", sum.LastHash, hashes[5])
	}
}

// A second child on the same directory would write into the same chain.
func TestAuditHasOneWriter(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAudit(t, dir)
	if _, err := OpenAudit(dir, nil); !errors.Is(err, ErrAuditHeld) {
		t.Fatalf("a second writer got %v, want ErrAuditHeld", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.append(AuditRecord{Event: AuditCall}); err == nil {
		t.Error("a closed log took a record")
	}
	again, err := OpenAudit(dir, nil)
	if err != nil {
		t.Fatalf("after the first writer closed: %v", err)
	}
	_ = again.Close()
}

// A log that cannot be opened is not a reason to stay down, and it is not a
// log either: every record is refused, with the reason, until it opens.
func TestAuditThatCannotBeOpenedRefusesEveryRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(AuditDir(dir), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenAudit(dir, nil)
	if err != nil || l == nil {
		t.Fatalf("OpenAudit = %v, %v; want a log that says why it is not ready", l, err)
	}
	defer l.Close()
	if l.Ready() == nil {
		t.Fatal("a log under a file is ready")
	}
	if _, err := l.append(AuditRecord{Event: AuditCall, Call: "c", Tool: "t"}); err == nil {
		t.Fatal("a record was taken by a log that could not be opened")
	}
	if err := os.Remove(AuditDir(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.append(AuditRecord{Event: AuditCall, Call: "c", Tool: "t"}); err != nil {
		t.Fatalf("after the way was cleared: %v", err)
	}
	if sum, err := VerifyAudit(dir); err != nil || sum.Records != 1 {
		t.Errorf("verify = %+v, %v", sum, err)
	}
}

// A write that fails takes nothing into the chain, and the log goes on from
// what is on the disk.
func TestAuditGoesOnAfterAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAudit(t, dir)
	fill(t, l, 2)
	_ = l.f.Close() // the disk goes away under the log
	if _, err := l.append(AuditRecord{Event: AuditCall, Call: "lost", Tool: "t"}); err == nil {
		t.Fatal("a record was taken with nowhere to write it")
	}
	fill(t, l, 2)
	sum, err := VerifyAudit(dir)
	if err != nil || sum.Records != 4 || sum.Last != 4 || sum.Torn != 0 {
		t.Fatalf("verify = %+v, %v; want four records and none lost or torn", sum, err)
	}
	if bytes.Contains(must(os.ReadFile(auditFile(dir)))(t), []byte("lost")) {
		t.Error("the record that was refused is in the log")
	}
}

// --- the chain ---

func TestAuditChain(t *testing.T) {
	dir := t.TempDir()
	if sum, err := VerifyAudit(dir); err != nil || sum != (AuditSummary{}) {
		t.Fatalf("a child with no log = %+v, %v", sum, err)
	}
	l, oslog := openAudit(t, dir)
	l.now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 987654321, time.FixedZone("x", 3600)) }
	hashes := fill(t, l, 3)
	lines := readLines(t, dir)
	if len(lines) != 3 {
		t.Fatalf("%d lines for three records", len(lines))
	}
	prev := auditStart
	for i, line := range lines {
		rec := must(parseAuditRecord(line))(t)
		if rec.Seq != uint64(i+1) || rec.Prev != prev || sum256(line) != hashes[i] {
			t.Errorf("line %d: seq %d, prev %s; its hash is %s and append said %s", i+1, rec.Seq, rec.Prev, sum256(line), hashes[i])
		}
		if got := rec.Time.Format(time.RFC3339Nano); got != "2026-11-18T11:00:00.987Z" {
			t.Errorf("time %s: want UTC to the millisecond", got)
		}
		prev = hashes[i]
	}
	sum, err := VerifyAudit(dir)
	if err != nil || sum != (AuditSummary{Files: 1, Records: 3, First: 1, Last: 3, LastHash: hashes[2]}) {
		t.Fatalf("verify = %+v, %v", sum, err)
	}
	// Each record went to the system log as one line that names its hash.
	if len(*oslog) != 3 || !strings.HasSuffix((*oslog)[2], "sha256="+hashes[2]) {
		t.Errorf("system log lines: %q", *oslog)
	}
	// A new process goes on where this one stopped.
	_ = l.Close()
	l2, _ := openAudit(t, dir)
	fill(t, l2, 1)
	if sum, err := VerifyAudit(dir); err != nil || sum.Records != 4 || sum.Last != 4 {
		t.Fatalf("after a restart: %+v, %v", sum, err)
	}
}

// A crash in the middle of a write leaves half a line. The log keeps it,
// chains over it and counts on.
func TestAuditSurvivesATornWrite(t *testing.T) {
	for name, torn := range map[string]string{
		"half a record":                `{"seq":4,"time":"2026-11-18T12:00:00Z","event":"ca`,
		"a record without its newline": "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l, _ := openAudit(t, dir)
			fill(t, l, 3)
			_ = l.Close()
			f := must(os.OpenFile(auditFile(dir), os.O_WRONLY|os.O_APPEND, 0o600))(t)
			wantTorn := 1
			if torn == "" { // cut exactly before the newline: the last record, whole
				b := must(os.ReadFile(auditFile(dir)))(t)
				_ = f.Close()
				if err := os.WriteFile(auditFile(dir), bytes.TrimSuffix(b, []byte("\n")), 0o600); err != nil {
					t.Fatal(err)
				}
				wantTorn = 0
			} else {
				_, _ = f.WriteString(torn)
				_ = f.Close()
			}
			l2, _ := openAudit(t, dir)
			fill(t, l2, 2)
			sum, err := VerifyAudit(dir)
			if err != nil || sum.Records != 5 || sum.Last != 5 || sum.Torn != wantTorn {
				t.Fatalf("verify = %+v, %v; want five records counted through, %d torn", sum, err, wantTorn)
			}
		})
	}
}

// A full file is closed under the number of its last record and the chain
// runs on through the files, also when a crash tore the first line of the
// new one.
func TestAuditRotates(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAudit(t, dir)
	l.max = 600
	hashes := fill(t, l, 12)
	files := auditFiles(AuditDir(dir), true)
	if len(files) < 3 || filepath.Base(files[len(files)-1]) != auditFileName {
		t.Fatalf("files: %v", files)
	}
	for _, f := range files[:len(files)-1] {
		last := must(parseAuditRecord(bytes.Split(bytes.TrimSpace(must(os.ReadFile(f))(t)), []byte("\n"))[0]))(t)
		if !strings.HasPrefix(filepath.Base(f), auditOldPre) || last.Seq == 0 {
			t.Errorf("closed file %s", f)
		}
	}
	sum, err := VerifyAudit(dir)
	if err != nil || sum.Records != 12 || sum.Files != len(files) || sum.LastHash != hashes[11] {
		t.Fatalf("verify over %d files = %+v, %v", len(files), sum, err)
	}
	// Removing the oldest file leaves a log that verifies from where it starts.
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if sum, err := VerifyAudit(dir); err != nil || sum.First == 1 || sum.Last != 12 {
		t.Fatalf("without the oldest file: %+v, %v", sum, err)
	}
	// Removing one from the middle does not.
	if len(files) > 3 {
		if err := os.Remove(files[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyAudit(dir); err == nil {
			t.Error("a log with a file missing from its middle verifies")
		}
	}
}

func TestAuditCountsOnFromAClosedFileAfterATornStart(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAudit(t, dir)
	l.max = 1
	fill(t, l, 2) // the second record closed the first one's file
	_ = l.Close()
	// The current file holds record 2. Close it by hand and tear the new one.
	cur := auditFile(dir)
	if err := os.Rename(cur, filepath.Join(AuditDir(dir), fmt.Sprintf("%s%020d%s", auditOldPre, 2, auditOldSuf))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cur, []byte(`{"seq":3,"ti`), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, _ := openAudit(t, dir)
	l2.max = 1 << 20
	fill(t, l2, 1)
	sum, err := VerifyAudit(dir)
	if err != nil || sum.Records != 3 || sum.Last != 3 || sum.Torn != 1 {
		t.Fatalf("verify = %+v, %v; want record 3 after the torn line", sum, err)
	}
}

// What goes to the system log is one line of plain words: a tool's name
// cannot split it, and nothing a tool was given is in it.
func TestAuditOSLine(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	got := auditOSLine(AuditRecord{Seq: 7, Event: AuditDone, Call: "c1", Tool: "os_exec", Outcome: AuditOK,
		Args: "rm -rf /secret", Note: "x"}, hash)
	if want := "fleet audit seq=7 event=done call=c1 tool=os_exec outcome=ok sha256=" + hash; got != want {
		t.Errorf("line:\n%s\nwant:\n%s", got, want)
	}
	got = auditOSLine(AuditRecord{Seq: 8, Event: AuditCall, Tool: "x\nfleet audit seq=9 event=done outcome=ok é\"=" + strings.Repeat("y", 500)}, hash)
	if strings.ContainsAny(got, "\n\"é") || strings.Count(got, "seq=") != 1 || strings.Count(got, "=") != 5 || len(got) > 400 ||
		!strings.Contains(got, " call=- ") {
		t.Errorf("a hostile tool name made the line %q", got)
	}
	// A missing process protection: the control's id, not the system's reason.
	got = auditOSLine(AuditRecord{Seq: 9, Event: AuditUnprotected, Control: "linux.dumpable", Note: "operation not permitted"}, hash)
	if want := "fleet audit seq=9 event=unprotected control=linux.dumpable sha256=" + hash; got != want {
		t.Errorf("line:\n%s\nwant:\n%s", got, want)
	}
}

func TestParseAuditRecordIsStrict(t *testing.T) {
	good := `{"seq":1,"time":"2026-11-18T12:00:00Z","event":"call","call":"c","tool":"t","prev":"` + auditStart + `"}`
	if _, err := parseAuditRecord([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"empty":             ``,
		"not json":          `seq=1`,
		"an unknown key":    strings.Replace(good, `"seq":1`, `"seq":1,"extra":1`, 1),
		"a key twice":       strings.Replace(good, `"seq":1`, `"seq":1,"seq":2`, 1),
		"trailing data":     good + `{}`,
		"no sequence":       strings.Replace(good, `"seq":1`, `"seq":0`, 1),
		"no time":           strings.Replace(good, `"time":"2026-11-18T12:00:00Z",`, ``, 1),
		"another event":     strings.Replace(good, `"call",`, `"other",`, 1),
		"a short prev":      strings.Replace(good, auditStart, "00", 1),
		"an uppercase prev": strings.Replace(good, auditStart, strings.Repeat("A", 64), 1),
		"too long":          strings.Replace(good, `"tool":"t"`, `"tool":"`+strings.Repeat("t", maxAuditLine)+`"`, 1),
	} {
		if rec, err := parseAuditRecord([]byte(line)); err == nil {
			t.Errorf("%s: parsed as %+v", name, rec)
		}
	}
}
