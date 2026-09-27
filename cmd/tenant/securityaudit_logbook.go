package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/secrets"
)

// The log book's part of `tenant security audit` (TEN-375): its files'
// protection, the scrubber, a scan for known secrets in what it holds, the
// Security log's chain and its witness. Like the rest of the audit it changes
// nothing: logbook.db is opened read-only and the key is read as it is.

// logbookAudit is the report's logbook section. Its files are in
// file_protection with the others.
type logbookAudit struct {
	Root     string          `json:"root"`
	Scrubber scrubberAudit   `json:"scrubber"`
	Scan     secretScanAudit `json:"secret_scan"`
	Verify   verifyAudit     `json:"security_log_verify"`
	Witness  witnessAudit    `json:"witness"`
}

// scrubberAudit is the scrubber every Tenant process runs on its log lines
// (TEN-361): the shapes of credentials always, and the values Tenant holds.
type scrubberAudit struct {
	On          bool `json:"on"`
	KnownValues int  `json:"known_values"`        // the credentials it replaces wherever they appear
	TooShort    int  `json:"too_short,omitempty"` // credentials under its 8-character floor, left alone
}

// secretScanAudit is where the known values turned up in the log book: the
// text files and the newest events of logbook.db.
type secretScanAudit struct {
	MirrorFiles int         `json:"mirror_files"`
	StoreEvents int         `json:"store_events"`
	Hits        []secretHit `json:"hits,omitempty"`
	Errors      []string    `json:"errors,omitempty"`
}

// secretHit is one known secret in one place, by name: never its value.
type secretHit struct {
	Secret string  `json:"secret"`
	Where  string  `json:"where"` // a text file, relative to the logs root, or logbook.db
	Count  int     `json:"count"`
	At     []int64 `json:"at"` // line numbers, or seqs in logbook.db; the first few
}

// verifyAudit is the Security log's chain as the audit found it.
type verifyAudit struct {
	Result    string    `json:"result"` // ok, broken, unverified, no_key or no_store
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Records   int64     `json:"records"`
	Break     string    `json:"break,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// witnessAudit is logs.security.witness.
type witnessAudit struct {
	Configured bool   `json:"configured"`
	Kind       string `json:"kind,omitempty"` // discord, file or peer
	Target     string `json:"target,omitempty"`
	Problem    string `json:"problem,omitempty"`
}

const (
	scanSample = 10_000 // logbook.db's newest events the scan reads
	scanAt     = 10     // places kept per hit
)

// auditLogbook builds the logbook section and the log book's file checks.
func auditLogbook(ctx context.Context, c *commonFlags) (logbookAudit, []fileProtectionAudit) {
	root := c.logsRoot()
	a := logbookAudit{Root: root, Scrubber: scrubberAudit{On: true}, Witness: witnessOf(c.lc)}
	named := logSecretValues(c.cfgDir, c.dataDir, fixedLogSecrets(c), nil)
	seen := map[string]bool{}
	for _, v := range named {
		switch {
		case v == "" || seen[v]:
		case len(v) >= logs.MinSecretLen:
			a.Scrubber.KnownValues++
		default:
			a.Scrubber.TooShort++
		}
		seen[v] = true
	}
	key, keyErr := readChainKeyWith(c.cfgDir, secrets.Peek)
	st, stErr := store.Open(filepath.Join(root, "logbook.db"), store.Options{ReadOnly: true, ChainKey: key})
	if st != nil {
		defer st.Close()
	}
	a.Scan = scanLogSecrets(ctx, root, st, named)
	a.Verify = verifyLogbook(ctx, c.cfgDir, st, stErr, keyErr, a.Witness)
	return a, logbookFileProtection(root, c.cfgDir)
}

// logbookFileProtection checks logbook.db and its -wal and -shm, every
// folder of the root (the text files, archive, spool, crash, boots) and the
// chain's anchor; the key is a managed secret, checked with the others. On
// Windows fsperm is a no-op until TEN-325, so the log book's files inherit
// the data directory's ACL, and the report says so.
func logbookFileProtection(root, cfgDir string) []fileProtectionAudit {
	var out []fileProtectionAudit
	for _, name := range []string{"logbook.db", "logbook.db-wal", "logbook.db-shm"} {
		out = append(out, logbookPathProtection(filepath.Join(root, name), false))
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, logbookPathProtection(filepath.Join(root, e.Name()), true))
		}
	}
	return append(out, checkFileProtection(chainAnchorPath(cfgDir), true))
}

func logbookPathProtection(path string, tree bool) fileProtectionAudit {
	check := fileProtectionAudit{Path: path, Sensitive: true}
	if runtime.GOOS == "windows" {
		check.Status = "not_found"
		if _, err := os.Lstat(path); err == nil {
			check.Status, check.Detail = "inherited", "fsperm is a no-op on Windows until TEN-325: it inherits the data directory's ACL"
		}
		return check
	}
	if !tree {
		return checkFileProtection(path, true)
	}
	issues, err := fsperm.AuditTree(path)
	switch {
	case err != nil:
		check.Status, check.Detail = "warning", fmt.Sprintf("cannot inspect %s: %v", path, err)
	case len(issues) > 0:
		check.Status = "warning"
		check.Detail = fmt.Sprintf("%s: %d path(s) exceed owner-only, first %s at %#o", path, len(issues), issues[0].Path, issues[0].Mode)
	default:
		check.Status = "ok"
		if info, err := os.Lstat(path); err == nil {
			check.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
		}
	}
	return check
}

// scanLogSecrets looks for the known values in the text files and in the
// newest events of logbook.db, as the scrubber would find them (raw and in
// every encoding it knows). It reports each by name and place.
//
// ponytail: reads every text file whole, a few seconds per GB; sample the
// newest days if a mirror ever grows past that.
func scanLogSecrets(ctx context.Context, root string, st *store.Store, named map[string]string) secretScanAudit {
	var sc logs.Scrubber
	sc.SetSecrets(named)
	var names []string
	for n, v := range named {
		if len(v) >= logs.MinSecretLen {
			names = append(names, n)
		}
	}
	var scan secretScanAudit
	found := map[[2]string]*secretHit{}
	look := func(where string, at int64, text string) {
		out := sc.Scrub(text)
		if out == text {
			return
		}
		for _, n := range names {
			mark := "«secret:" + n + "»"
			if k := strings.Count(out, mark) - strings.Count(text, mark); k > 0 {
				h := found[[2]string{n, where}]
				if h == nil {
					h = &secretHit{Secret: n, Where: where}
					found[[2]string{n, where}] = h
				}
				h.Count += k
				if len(h.At) < scanAt {
					h.At = append(h.At, at)
				}
			}
		}
	}
	dirs, _ := os.ReadDir(root)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, f := range files {
			if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".log") {
				continue
			}
			rel := d.Name() + "/" + f.Name()
			scan.MirrorFiles++
			if err := scanLines(filepath.Join(root, rel), func(n int64, line string) { look(rel, n, line) }); err != nil {
				scan.Errors = append(scan.Errors, rel+": "+err.Error())
			}
		}
	}
	if st != nil {
		evs, _, _, err := st.Query(ctx, query.Filter{}, store.Cursor{}, scanSample, true)
		if err != nil {
			scan.Errors = append(scan.Errors, "logbook.db: "+err.Error())
		}
		for _, e := range evs {
			data, _ := event.EncodeData(e.Data)
			look("logbook.db", int64(e.Seq), e.Msg+"\n"+data)
		}
		scan.StoreEvents = len(evs)
	}
	for _, h := range found {
		scan.Hits = append(scan.Hits, *h)
	}
	slices.SortFunc(scan.Hits, func(a, b secretHit) int {
		return strings.Compare(a.Where+"\x00"+a.Secret, b.Where+"\x00"+b.Secret)
	})
	return scan
}

// scanLines calls fn with each line of the file at path and its number.
func scanLines(path string, fn func(n int64, line string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var n int64
	for sc.Scan() {
		n++
		fn(n, sc.Text())
	}
	return sc.Err()
}

// verifyLogbook verifies the Security log read-only, against the anchor and
// a file witness, as `tenant logs verify security` does. It files no event.
func verifyLogbook(ctx context.Context, cfgDir string, st *store.Store, stErr, keyErr error, w witnessAudit) verifyAudit {
	switch {
	case errors.Is(stErr, fs.ErrNotExist):
		return verifyAudit{Result: "no_store"}
	case stErr != nil:
		return verifyAudit{Result: "unverified", Error: stErr.Error()}
	case errors.Is(keyErr, fs.ErrNotExist):
		if h, err := st.Head(ctx); err == nil && h.Rec > 0 {
			return verifyAudit{Result: "unverified", Records: h.Rec, Error: "the Security log holds records, but its key " + chainKeyPath(cfgDir) + " is missing"}
		}
		return verifyAudit{Result: "no_key"}
	case keyErr != nil:
		return verifyAudit{Result: "unverified", Error: keyErr.Error()}
	}
	opts, err := chainVerifyOptions(cfgDir, w)
	if err != nil {
		return verifyAudit{Result: "unverified", Error: err.Error()}
	}
	v, err := st.Verify(ctx, opts)
	a := verifyAudit{Result: "ok", CheckedAt: time.Now().UTC(), Records: v.Records, Notes: v.Notes}
	switch {
	case err != nil:
		a.Result, a.Error = "unverified", err.Error()
	case v.Break != nil:
		a.Result, a.Break = "broken", v.Break.Error()
	}
	return a
}

// witnessOf reads logs.security.witness, for witnessChain and the audit.
func witnessOf(lc *launchConfig) witnessAudit {
	w := ""
	if lc != nil {
		w = strings.TrimSpace(lc.Logs.Security.Witness)
	}
	switch {
	case w == "":
		return witnessAudit{}
	case w == "discord":
		return witnessAudit{Configured: true, Kind: "discord"}
	case strings.HasPrefix(w, "peer:"):
		return witnessAudit{Configured: true, Kind: "peer", Target: w, Problem: "a peer can't be a witness yet, so the head reaches no witness"}
	}
	if path := expandPath(w); filepath.IsAbs(path) {
		return witnessAudit{Configured: true, Kind: "file", Target: path}
	}
	return witnessAudit{Configured: true, Kind: "file", Target: w, Problem: "the witness file must be an absolute path, so the head reaches no witness"}
}

// logbookFindings adds the log book's findings; its files' warnings come
// from the file_protection checks.
func logbookFindings(r *securityAuditReport, add func(id, area string, severity auditSeverity, summary, remedy string)) {
	lb := r.Logbook
	switch lb.Verify.Result {
	case "broken":
		add("logbook.chain", "logbook", auditCritical, "the Security log's chain is broken: "+lb.Verify.Break,
			"run `tenant logs verify security` and find what changed the log; a restore made with `tenant backup restore` verifies as one")
	case "unverified":
		add("logbook.verify", "logbook", auditWarning, "the Security log could not be verified: "+lb.Verify.Error,
			"check logbook.db (`tenant doctor`) and the key, <config>/logbook.key")
	}
	if len(lb.Scan.Hits) > 0 {
		var names []string
		for _, h := range lb.Scan.Hits {
			if !slices.Contains(names, h.Secret) {
				names = append(names, h.Secret)
			}
		}
		add("logbook.secrets", "logbook", auditWarning,
			fmt.Sprintf("%d known credential(s) appear in the log book (%s); secret_scan says where", len(names), strings.Join(names, ", ")),
			"rotate each named credential, then remove the lines that hold it")
	}
	if lb.Witness.Problem != "" {
		add("logbook.witness", "logbook", auditWarning, "logs.security.witness "+lb.Witness.Target+": "+lb.Witness.Problem,
			`set it to "discord" or the absolute path of a file, best on another machine`)
	}
	if slices.ContainsFunc(r.Files, func(f fileProtectionAudit) bool { return f.Status == "inherited" }) {
		add("logbook.windows-acl", "file_protection", auditWarning,
			"the log book's files have only the data directory's ACL: fsperm is a no-op on Windows until TEN-325",
			"restrict the data directory (and logs.dir) to the Tenant operator's account")
	}
}

// printLogbookAudit prints the logbook section.
func printLogbookAudit(lb logbookAudit) {
	fmt.Printf("\nlog book: %s\n", lb.Root)
	fmt.Printf("  scrubber: on=%t known_values=%d too_short=%d\n", lb.Scrubber.On, lb.Scrubber.KnownValues, lb.Scrubber.TooShort)
	fmt.Printf("  secret scan: %d text file(s), %d event(s) of logbook.db", lb.Scan.MirrorFiles, lb.Scan.StoreEvents)
	if len(lb.Scan.Hits) == 0 {
		fmt.Print("; no known secret found")
	}
	fmt.Println()
	for _, h := range lb.Scan.Hits {
		unit := "line"
		if h.Where == "logbook.db" {
			unit = "seq"
		}
		fmt.Printf("    %s: %s × %d (%s %v)\n", h.Where, h.Secret, h.Count, unit, h.At)
	}
	v := lb.Verify
	fmt.Printf("  security log verify: %s", v.Result)
	if !v.CheckedAt.IsZero() {
		fmt.Printf(" (%d record(s), at %s)", v.Records, v.CheckedAt.Local().Format(time.DateTime))
	}
	if msg := firstNonEmpty(v.Break, v.Error); msg != "" {
		fmt.Printf(": %s", msg)
	}
	fmt.Println()
	w := "none"
	if lb.Witness.Configured {
		w = strings.TrimSpace(lb.Witness.Kind + " " + lb.Witness.Target)
	}
	fmt.Printf("  witness: %s\n", w)
}
