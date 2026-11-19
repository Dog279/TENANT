package fleet

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// auditSeed is a log of four records that verifies.
func auditSeed(tb testing.TB) []byte {
	var out []byte
	prev := auditStart
	for i, rec := range []AuditRecord{
		{Event: AuditCall, Call: "c1", Tool: "os_exec", Args: `{"command":"uptime"}`, ArgsSHA256: fingerprintOf([]byte("x"))},
		{Event: AuditDone, Call: "c1", Tool: "os_exec", Outcome: AuditOK, Asked: []AuditAsk{{Action: "os_exec", Answer: AskAllowed}},
			ResultSHA256: fingerprintOf([]byte("y")), ResultBytes: 1},
		{Event: AuditDone, Call: "c2", Tool: "os_write_file", Outcome: AuditRefused, Note: "not served",
			Asked: []AuditAsk{{Action: "os_write", Answer: AskRefused, Why: "ceiling read-only"}}, AskedMore: 2},
		{Event: AuditRehome, By: "root", From: fingerprintOf([]byte("a")), To: fingerprintOf([]byte("b"))},
	} {
		rec.Seq, rec.Time, rec.Prev = uint64(i+1), time.Date(2026, 11, 18, 12, 0, i, 0, time.UTC), prev
		line := must(encode(rec))(tb)
		prev = fingerprintOf(line)
		out = append(append(out, line...), '\n')
	}
	return out
}

// A line of the audit log is read from the disk, where anything may have
// been written. What parses is a record and writes back as the same record;
// its line for the system log is one line of plain ASCII.
func FuzzAuditRecord(f *testing.F) {
	for _, line := range bytes.Split(bytes.TrimSpace(auditSeed(f)), []byte("\n")) {
		f.Add(line)
	}
	for _, s := range []string{
		``, `{}`, `null`, `[]`, `{"seq":1}`, `{"seq":-1,"time":"x"}`,
		`{"seq":18446744073709551615,"time":"2026-11-18T12:00:00+14:00","event":"call","call":"","tool":"","prev":"` + auditStart + `"}`,
		`{"seq":1,"seq":2,"time":"2026-11-18T12:00:00Z","event":"done","call":"c","tool":"t","prev":"` + auditStart + `"}`,
		`{"seq":1,"time":"2026-11-18T12:00:00Z","event":"done","call":"c\n","tool":"a b=\"c\"","outcome":"ok\u0000","prev":"` + auditStart + `"}`,
		`{"seq":1,"time":"2026-11-18T12:00:00Z","event":"call","asked":[{"action":"a","answer":"b","why":"c","more":1}],"prev":"` + auditStart + `"}`,
		strings.Repeat(`[`, 5000),
		// Found by this fuzzer: keys in another case, an empty list, an hour of one digit.
		`{"SEQ":1,"time":"0000-01-01T0:00:00Z","event":"done","Asked":[],"prev":"` + auditStart + `"}`,
		`{"seq":1,"time":"2026-11-18T12:00:00Z","event":"call","call":"c","tool":"` + strings.Repeat("t", maxAuditLine) + `","prev":"` + auditStart + `"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		rec, err := parseAuditRecord(line)
		if err != nil {
			if !reflect.DeepEqual(rec, AuditRecord{}) {
				t.Fatalf("a line that is not a record left %+v behind", rec)
			}
			return
		}
		if _, perr := parseFingerprint(rec.Prev); perr != nil || rec.Seq == 0 || rec.Time.IsZero() ||
			(rec.Event != AuditCall && rec.Event != AuditDone && rec.Event != AuditRehome) || len(line) > maxAuditLine {
			t.Fatalf("parsed as a record: %+v", rec)
		}
		back, err := encode(rec)
		if err != nil {
			t.Fatalf("a parsed record does not encode: %v", err)
		}
		// A record written back may be longer than a record (escapes), which
		// only this log's own writer has to care about.
		if len(back) <= maxAuditLine {
			again, err := parseAuditRecord(back)
			if err != nil || !again.Time.Equal(rec.Time) {
				t.Fatalf("written back as %s: %+v, %v", back, again, err)
			}
			again.Time = rec.Time
			if len(rec.Asked) == 0 {
				rec.Asked = nil // an empty list is written as no list
			}
			if !reflect.DeepEqual(again, rec) {
				t.Fatalf("written back and read again it is %+v, not %+v", again, rec)
			}
		}
		os := auditOSLine(rec, fingerprintOf(line))
		if len(os) > 600 || strings.IndexFunc(os, func(r rune) bool { return r < ' ' || r >= 0x7f }) >= 0 ||
			strings.Count(os, "=") > 6 || strings.Count(os, " ") > 7 {
			t.Fatalf("the system log line is %q", os)
		}
	})
}

// A whole log file from the disk. The line reader, which holds no line
// longer than a record in memory, must agree with a plain split of the
// bytes; and where the verifier finds nothing wrong, the opener stands at
// the same place and the record it writes next keeps the log verifying.
func FuzzAuditLog(f *testing.F) {
	seed := auditSeed(f)
	lines := bytes.Split(bytes.TrimSuffix(seed, []byte("\n")), []byte("\n"))
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte("\n"))
	f.Add([]byte("\n\n\n"))
	f.Add(seed[:len(seed)-1])                                      // the last newline missing
	f.Add(append(bytes.Clone(seed), `{"seq":4,"time":"2026-`...))  // a torn write
	f.Add(append(append(bytes.Clone(seed), "torn\n"...), seed...)) // a second chain after a line
	f.Add(bytes.Join([][]byte{lines[0], lines[2]}, []byte("\n")))  // a record removed
	f.Add(bytes.Join([][]byte{lines[1], lines[0], lines[2]}, []byte("\n")))
	f.Add(bytes.ReplaceAll(seed, []byte("\n"), []byte("\r\n")))
	f.Add(bytes.Replace(seed, []byte("uptime"), []byte("upside"), 1))
	f.Add(append(bytes.Repeat([]byte("x"), maxAuditLine+1), '\n'))
	f.Add(append(append(bytes.Repeat([]byte("x"), 3*maxAuditLine), '\n'), seed...))
	f.Add(bytes.Repeat([]byte("y"), 33<<10)) // longer than the reader's buffer, no newline
	f.Fuzz(func(t *testing.T, data []byte) {
		type read struct {
			line []byte
			hash string
		}
		var got []read
		torn, err := auditLines(bytes.NewReader(data), func(line []byte, hash string) error {
			got = append(got, read{bytes.Clone(line), hash})
			return nil
		})
		if err != nil {
			t.Fatalf("reading from memory failed: %v", err)
		}
		want := bytes.Split(data, []byte("\n"))
		wantTorn := len(data) > 0 && data[len(data)-1] != '\n'
		if !wantTorn {
			want = want[:len(want)-1]
		}
		if torn != wantTorn || len(got) != len(want) {
			t.Fatalf("%d lines, torn %v; a split gives %d, torn %v", len(got), torn, len(want), wantTorn)
		}
		var c auditCheck
		var broken error
		for i, w := range want {
			if got[i].hash != fingerprintOf(w) {
				t.Fatalf("line %d: hash %s, want %s", i+1, got[i].hash, fingerprintOf(w))
			}
			if len(w) > maxAuditLine {
				if got[i].line != nil {
					t.Fatalf("line %d of %d bytes was held in memory", i+1, len(w))
				}
			} else if got[i].line == nil || !bytes.Equal(got[i].line, w) {
				t.Fatalf("line %d: %q, want %q", i+1, got[i].line, w)
			}
			if broken == nil {
				broken = c.line("line", got[i].line, got[i].hash)
			}
		}
		seq, prev, endTorn, err := auditEnd(bytes.NewReader(data))
		if err != nil || endTorn != wantTorn {
			t.Fatalf("auditEnd: torn %v, %v", endTorn, err)
		}
		if broken != nil {
			return
		}
		if at := c.prev; seq != c.sum.Last || (at != "" && prev != at) || (at == "" && prev != auditStart) {
			t.Fatalf("the verifier ends at record %d, line %s; the opener at %d, %s", c.sum.Last, at, seq, prev)
		}
		if seq == ^uint64(0) {
			return // no number is left for a next record
		}
		next := must(encode(AuditRecord{Seq: seq + 1, Time: time.Unix(1, 0).UTC(), Event: AuditCall, Call: "c", Tool: "t", Prev: prev}))(t)
		if err := c.line("next", next, fingerprintOf(next)); err != nil {
			t.Fatalf("the record written after a log that verifies does not: %v", err)
		}
	})
}

// child.json again (FuzzParseChild has the rest of it), seeded for the list
// this stage added: secret_paths, the operator's own additions to what the
// file tools refuse. What is accepted is a list of distinct absolute paths
// that survives a save; anything else refuses the whole file, since a child
// that misreads the list would read a file its operator closed.
func FuzzParseChildSecretPaths(f *testing.F) {
	good := `{"version":1,"id":"m0123456789abcdef","name":"box","master_addresses":["10.0.0.1:7443"],"master_fingerprint":"` + fuzzPrint +
		`","sandbox":"host","ceiling":"read-only","secret_paths":["/srv/vault","/home/*/app/token.json"]}`
	for _, seed := range []string{
		good,
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `["C:\Vault","D:\Keys\*.pem"]`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `[]`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `null`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `["vault"]`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `["/a","/a"]`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `["/a\n/b"]`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `"/srv/vault"`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `[""]`, 1),
		strings.Replace(good, `["/srv/vault","/home/*/app/token.json"]`, `["/`+strings.Repeat("x", maxPathLen)+`"]`, 1),
		strings.Replace(good, `"secret_paths"`, `"secret_paths":["/x"],"SECRET_PATHS"`, 1),
		strings.Replace(good, `"secret_paths"`, `"paused":true,"secret_paths"`, 1),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		c, err := parseChild(in)
		if err != nil {
			if !reflect.DeepEqual(c, Child{}) || c.Serves("os_read_file") {
				t.Fatalf("refused and returned %+v", c)
			}
			return
		}
		if c.SecretPaths != nil && (len(c.SecretPaths) == 0 || len(c.SecretPaths) > maxSecretPaths) {
			t.Fatalf("accepted a list of %d secret paths", len(c.SecretPaths))
		}
		for i, p := range c.SecretPaths {
			if !filepath.IsAbs(p) || len(p) > maxPathLen || !printable(p) || slices.Contains(c.SecretPaths[:i], p) {
				t.Fatalf("accepted the secret path %q", p)
			}
		}
		out, err := json.Marshal(childFile{Version: childVersion, Child: c})
		if err != nil {
			t.Fatal(err)
		}
		if again, err := parseChild(out); err != nil || !reflect.DeepEqual(again, c) {
			t.Fatalf("%+v does not survive a save: %+v, %v", c, again, err)
		}
	})
}
