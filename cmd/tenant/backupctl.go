package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	xterm "github.com/charmbracelet/x/term"

	"tenant/internal/backup"
	"tenant/internal/improve"
	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/store"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/sme"
	"tenant/internal/memory/soul"
	usagestore "tenant/internal/memory/usage"
	"tenant/internal/model/backend/claudecode"
	"tenant/internal/peering"
	"tenant/internal/privacy"
	"tenant/internal/runtrace"
)

const backupUsage = `usage:
  tenant backup [create] [--out FILE|DIR] [--encrypt | --passphrase-file F | --recipient AGE1… | --recipients-file F] [--include-mirror] [--json]
  tenant backup verify FILE [--passphrase-file F | --identity F] [--json]
  tenant backup restore FILE [--dry-run] [--yes] [--force] [--passphrase-file F | --identity F]

A backup is one file holding the config and data dirs: every store, the
archive, souls, settings and the log book (logbook.db and its archives; its
text files hold the same events and come along with --include-mirror). It is
age-encrypted when you pass --encrypt (a passphrase), --passphrase-file, or
--recipient (a public key from age-keygen); only encrypted backups carry your
credentials and keys. The passphrase can also come from
TENANT_BACKUP_PASSPHRASE.`

// passphraseEnv supplies a backup passphrase without a prompt or a file.
const passphraseEnv = "TENANT_BACKUP_PASSPHRASE"

// cmdBackup is `tenant backup create|verify|restore` (TEN-305).
func cmdBackup(ctx context.Context, args []string) error {
	verb := "create"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	switch verb {
	case "create":
		return cmdBackupCreate(ctx, args, os.Stdout)
	case "verify":
		return cmdBackupVerify(ctx, args, os.Stdout)
	case "restore":
		return cmdBackupRestore(ctx, args, os.Stdin, os.Stdout)
	case "help":
		fmt.Println(backupUsage)
		return nil
	}
	return fmt.Errorf("unknown backup command %q\n%s", verb, backupUsage)
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// keyFlags are the encryption flags the three verbs share.
type keyFlags struct {
	encrypt        bool
	passphraseFile string
	recipients     stringList
	recipientFiles stringList
	identities     stringList
}

func (k *keyFlags) bindEncrypt(fs *flag.FlagSet) {
	fs.BoolVar(&k.encrypt, "encrypt", false, "encrypt with a passphrase (prompted, or from "+passphraseEnv+")")
	fs.StringVar(&k.passphraseFile, "passphrase-file", "", "read the passphrase from this file (first line)")
	fs.Var(&k.recipients, "recipient", "encrypt to this age public key (age1…); repeatable")
	fs.Var(&k.recipientFiles, "recipients-file", "encrypt to the public keys in this file; repeatable")
}

func (k *keyFlags) bindDecrypt(fs *flag.FlagSet) {
	fs.StringVar(&k.passphraseFile, "passphrase-file", "", "read the passphrase from this file (first line)")
	fs.Var(&k.identities, "identity", "decrypt with the age private key in this file; repeatable")
}

// readPassphraseFile returns the first line of path.
func readPassphraseFile(path string) (string, error) {
	b, err := os.ReadFile(expandPath(path))
	if err != nil {
		return "", fmt.Errorf("passphrase file: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return "", errors.New("passphrase file is empty")
	}
	return line, nil
}

// promptPassphrase asks on the terminal without echo; confirm asks twice.
var promptPassphrase = func(out io.Writer, confirm bool) (string, error) {
	fd := os.Stdin.Fd()
	if !xterm.IsTerminal(fd) {
		return "", fmt.Errorf("no passphrase: pass --passphrase-file or set %s", passphraseEnv)
	}
	fmt.Fprint(out, "Backup passphrase: ")
	a, err := xterm.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", err
	}
	if len(a) == 0 {
		return "", errors.New("empty passphrase")
	}
	if confirm {
		fmt.Fprint(out, "Again: ")
		b, err := xterm.ReadPassword(fd)
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the passphrases do not match")
		}
	}
	return string(a), nil
}

// passphrase resolves one from, in order, the file flag and the environment.
func (k *keyFlags) passphrase() (string, error) {
	if k.passphraseFile != "" {
		return readPassphraseFile(k.passphraseFile)
	}
	return os.Getenv(passphraseEnv), nil
}

// encryptKeys resolves the keys for a new backup. Public keys win; otherwise
// a passphrase from --passphrase-file or the environment encrypts, and
// --encrypt with neither prompts for one. A passphrase in the environment
// alone is taken as a request for encryption: setting one and getting a
// plaintext backup would be a nasty surprise.
func (k *keyFlags) encryptKeys(out io.Writer) (backup.Keys, error) {
	keys := backup.Keys{Recipients: k.recipients, RecipientFiles: expandAll(k.recipientFiles)}
	if len(keys.Recipients)+len(keys.RecipientFiles) > 0 {
		if k.encrypt || k.passphraseFile != "" {
			return keys, errors.New("use a passphrase or --recipient keys, not both")
		}
		return keys, nil
	}
	pass, err := k.passphrase()
	if err != nil {
		return keys, err
	}
	if pass == "" && k.encrypt {
		if pass, err = promptPassphrase(out, true); err != nil {
			return keys, err
		}
	}
	keys.Passphrase = pass
	return keys, nil
}

// decryptKeys resolves the keys to open a backup with.
func (k *keyFlags) decryptKeys() (backup.Keys, error) {
	keys := backup.Keys{IdentityFiles: expandAll(k.identities)}
	pass, err := k.passphrase()
	if err != nil {
		return keys, err
	}
	keys.Passphrase = pass
	return keys, nil
}

func expandAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = expandPath(p)
	}
	return out
}

// parseWithFile parses flags on either side of one positional FILE argument.
func parseWithFile(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", fmt.Errorf("missing backup file\n%s", backupUsage)
	}
	file := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), backupUsage)
	}
	return expandPath(file), nil
}

// defaultBackupName is tenant-backup-<local time>.tar.gz, plus .age when
// encrypted.
func defaultBackupName(now time.Time, encrypted bool) string {
	name := "tenant-backup-" + now.Format("20060102-150405") + ".tar.gz"
	if encrypted {
		name += ".age"
	}
	return name
}

func cmdBackupCreate(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
	c := bindCommon(fs)
	var k keyFlags
	k.bindEncrypt(fs)
	outPath := fs.String("out", "", "backup file, or a directory to put it in (default: the current directory)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	mirror := fs.Bool("include-mirror", false, "also take the log book's text files (<logs>/<log>/*.log); logbook.db holds the same events")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), backupUsage)
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	lc, _ := loadLaunchConfig(c.cfgDir)
	c.lc = lc
	defer fileEvents(c)()
	keys, err := k.encryptKeys(out)
	if err != nil {
		return err
	}
	dest := expandPath(*outPath)
	if dest == "" {
		dest = "."
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		dest = filepath.Join(dest, defaultBackupName(time.Now(), keys.Encrypts()))
	}
	managed, err := managedSecrets(c.cfgDir, c.dataDir)
	if err != nil {
		return err
	}
	res, err := backup.Create(ctx, dest, backup.Options{
		ConfigDir:   c.cfgDir,
		DataDir:     c.dataDir,
		Secrets:     secretPaths(managed),
		ConfigFile:  configDirFile,
		NotIncluded: outsideTenantDirs(c),
		Keys:        keys,
		// The log book: logbook.db by VACUUM INTO, its archives, and its
		// text mirror only when asked for.
		LogsDir:       c.logsRoot(),
		IncludeMirror: *mirror,
	})
	if err != nil {
		return err
	}
	// So `tenant privacy status` can count the backups a later forget
	// leaves holding deleted data.
	if err := privacy.NoteBackup(privacy.Data{DataDir: c.dataDir}, res.Path, res.Manifest.CreatedAt); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not record this backup for `tenant privacy status`: %v", err))
	}
	_ = catalog.BackupWritten.Audit(ctx, "path", res.Path, "bytes", res.Bytes, "encrypted", res.Manifest.Encrypted, "actor", actorOperator, "surface", surfaceCLI)
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Path     string           `json:"path"`
			Bytes    int64            `json:"bytes"`
			Warnings []string         `json:"warnings,omitempty"`
			Manifest *backup.Manifest `json:"manifest"`
		}{res.Path, res.Bytes, res.Warnings, res.Manifest})
	}
	m := res.Manifest
	enc := "not encrypted; credentials and keys left out"
	if m.Encrypted {
		enc = fmt.Sprintf("encrypted; %d credential and key file(s) included", m.Count(backup.KindSecret))
	}
	fmt.Fprintf(out, "backup written: %s\n", res.Path)
	fmt.Fprintf(out, "  %s, %s\n", humanBytes(res.Bytes), enc)
	fmt.Fprintf(out, "  %d files: %d database(s), %d log(s), %d other\n", len(m.Entries),
		m.Count(backup.KindSQLite), m.Count(backup.KindLog), m.Count(backup.KindFile)+m.Count(backup.KindSecret))
	printSkipped(out, m.Skipped)
	if !m.Encrypted && lc != nil && lc.Dashboard.Auth != "" {
		fmt.Fprintln(out, "  note: config.json sets dashboard.auth, and that token is in this unencrypted backup")
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
	fmt.Fprintf(out, "  check it any time: tenant backup verify %s\n", quoteArg(res.Path))
	return nil
}

func printSkipped(out io.Writer, skipped []backup.Skipped) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintln(out, "  not included:")
	const show = 12
	for i, s := range skipped {
		if i == show {
			fmt.Fprintf(out, "    … and %d more (--json lists them all)\n", len(skipped)-show)
			break
		}
		fmt.Fprintf(out, "    %s: %s\n", s.Path, s.Reason)
	}
}

func cmdBackupVerify(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	c := bindCommon(fs) // whose log book records the verify
	var k keyFlags
	k.bindDecrypt(fs)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	file, err := parseWithFile(fs, args)
	if err != nil {
		return err
	}
	c.auditOnly = true // verifying creates and locks nothing
	if err := c.resolveDirs(); err != nil {
		return err
	}
	defer fileEvents(c)()
	keys, err := k.decryptKeys()
	if err != nil {
		return err
	}
	schemas, err := newStoreSchemas()
	if err != nil {
		return err
	}
	defer schemas.Close()
	checks := backup.Checks{ReferenceSchema: schemas.reference, CheckFile: checkBackupFile}
	rep, err := backup.Verify(ctx, file, keys, checks, "")
	if errors.Is(err, backup.ErrEncrypted) {
		if keys.Passphrase, err = promptPassphrase(out, false); err != nil {
			return err
		}
		rep, err = backup.Verify(ctx, file, keys, checks, "")
	}
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		printReport(out, file, rep)
	}
	if !rep.OK() {
		return fmt.Errorf("the backup failed verification (%d problem(s))", len(rep.Problems))
	}
	return nil
}

func printReport(out io.Writer, file string, rep *backup.Report) {
	fmt.Fprintf(out, "backup: %s\n", file)
	if m := rep.Manifest; m != nil {
		enc := "not encrypted"
		if rep.Encrypted {
			enc = "encrypted"
		}
		fmt.Fprintf(out, "  made %s by tenant %s on %s, %s\n", m.CreatedAt.Local().Format("2006-01-02 15:04:05"), m.TenantVersion, m.Platform, enc)
		fmt.Fprintf(out, "  %d files, %s unpacked: %d database(s), %d log(s)\n", len(m.Entries), humanBytes(m.TotalSize()),
			m.Count(backup.KindSQLite), m.Count(backup.KindLog))
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(out, "  PROBLEM: %s\n", p)
	}
	for _, n := range rep.Newer {
		fmt.Fprintf(out, "  newer than this Tenant: %s\n", n)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
	if rep.OK() {
		fmt.Fprintln(out, "  OK: every checksum matches, every database passes integrity_check, every log parses")
	}
}

func cmdBackupRestore(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	c := bindCommon(fs)
	var k keyFlags
	k.bindDecrypt(fs)
	dryRun := fs.Bool("dry-run", false, "verify the backup beside the live dirs and report; change nothing")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	force := fs.Bool("force", false, "restore a backup made by a newer Tenant")
	file, err := parseWithFile(fs, args)
	if err != nil {
		return err
	}
	// Restore takes the data-dir lock exclusively itself.
	c.skipDataLock = true
	if err := c.resolveDirs(); err != nil {
		return err
	}
	defer fileEvents(c)() // into the restored log book
	if lc, err := loadLaunchConfig(c.cfgDir); err == nil {
		if addr, up := hubAnswering(ctx, lc); up {
			return fmt.Errorf("a Tenant hub answers at %s; stop it (and the TUI) before restoring", addr)
		}
	}
	keys, err := k.decryptKeys()
	if err != nil {
		return err
	}
	if !*dryRun && !*yes {
		fmt.Fprintf(out, "Restore %s over\n  config: %s\n  data:   %s\nThe current directories are kept beside them as *.pre-restore-<time>. Continue? [y/N] ", file, c.cfgDir, c.dataDir)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("restore cancelled")
		}
	}
	schemas, err := newStoreSchemas()
	if err != nil {
		return err
	}
	defer schemas.Close()
	managed, _ := managedSecrets(c.cfgDir, c.dataDir)
	// The Security log's anchor from before the restore, which moves the
	// config dir aside: the head the re-anchor record says was replaced.
	replaced, _ := readChainHead(chainAnchorPath(c.cfgDir))
	opts := backup.RestoreOptions{
		ConfigDir: c.cfgDir,
		DataDir:   c.dataDir,
		Keys:      keys,
		Checks:    backup.Checks{ReferenceSchema: schemas.reference, CheckFile: checkBackupFile},
		Secrets:   secretPaths(managed),
		Force:     *force,
		DryRun:    *dryRun,
	}
	res, err := backup.Restore(ctx, file, opts)
	if errors.Is(err, backup.ErrEncrypted) {
		if opts.Keys.Passphrase, err = promptPassphrase(out, false); err != nil {
			return err
		}
		res, err = backup.Restore(ctx, file, opts)
	}
	if res != nil && res.Report != nil {
		printReport(out, file, res.Report)
	}
	if err != nil {
		return err
	}
	if len(res.KeptSecrets) > 0 {
		fmt.Fprintf(out, "  kept your current credentials and keys (the backup has none): %s\n", strings.Join(res.KeptSecrets, ", "))
	}
	if *dryRun {
		fmt.Fprintln(out, "dry run: the backup restores cleanly; nothing was changed")
		return nil
	}
	// The restored logbook.db's seq counter went back with it: a new store ID
	// makes every live-tail cursor restart instead of skipping what comes next.
	// Its Security log gets a re-anchor record (the restored head, the head it
	// replaced, the backup), so the restore verifies as one, not as tampering.
	lb := filepath.Join(c.logsRoot(), "logbook.db")
	if _, err := os.Stat(lb); err == nil {
		if err := store.Reidentify(lb); err != nil {
			fmt.Fprintf(out, "  warning: the log book was restored but not re-identified (%v); viewers left open may miss new events until they reload\n", err)
		}
		if err := reanchorAfterRestore(ctx, c.cfgDir, lb, replaced, file); err != nil {
			fmt.Fprintf(out, "  warning: the Security log's restore was not fully recorded (%v); `tenant logs verify security` may report it as a break\n", err)
		}
	}
	var made time.Time
	if res.Report != nil && res.Report.Manifest != nil {
		made = res.Report.Manifest.CreatedAt
	}
	_ = catalog.BackupRestored.Audit(ctx, "path", file, "created", made, "actor", actorOperator, "surface", surfaceCLI)
	fmt.Fprintln(out, "restored.")
	for _, a := range res.Aside {
		fmt.Fprintf(out, "  the previous directory is kept at %s; delete it once you are happy\n", a)
	}
	fmt.Fprintln(out, "  this install now carries the backed-up identity (instance id, peer key, Discord relay); don't run it alongside the machine the backup came from")
	return nil
}

// fileEvents gives a one-shot command's events (a backup's, a deletion's) a
// place in the log book, which only serve and the TUI keep open: they are held
// in memory while the command runs and appended when it ends, so the command
// never holds a log file open, not even in a data dir a restore swaps out.
// Call the func it returns when the command ends.
//
// ponytail: a crash mid-command loses its events; drop the buffer once every
// process writes logbook.db as it goes (TEN-370).
func fileEvents(c *commonFlags) func() {
	prev := logs.Default()
	mem := logs.NewMemory(0)
	logs.Install(mem)
	return func() {
		logs.Install(prev)
		entries, _ := mem.Snapshot()
		if _, err := os.Stat(c.dataDir); err != nil || len(entries) == 0 {
			return // no install here, as for a backup verified on another machine
		}
		if c.lc == nil {
			c.lc, _ = loadLaunchConfig(c.cfgDir) // after a restore, the restored one
		}
		h, err := logs.Open(c.logsRoot(), c.logsOptions())
		if err != nil {
			return
		}
		defer h.Close()
		attachCommandStore(h, c)
		for _, e := range entries {
			h.Log(e)
		}
	}
}

// hubAnswering reports whether a Tenant dashboard answers at the configured
// address: a hub started by a Tenant from before the data-dir lock would not
// hold it, so this is the second guard against restoring under a live hub.
func hubAnswering(ctx context.Context, lc *launchConfig) (string, bool) {
	dc := lc.Dashboard
	if !dc.IsEnabled() && dc.Addr == "" {
		return "", false
	}
	addr := dc.Addr
	if addr == "" {
		addr = "127.0.0.1:8770"
	}
	scheme := "http"
	if dc.TLSCert != "" {
		scheme = "https"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, scheme+"://"+addr+"/api/status", nil)
	if err != nil {
		return addr, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return addr, false
	}
	resp.Body.Close()
	// 200, or 401/403 from a dashboard that wants its token: either way a
	// hub is up.
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden:
		return addr, true
	}
	return addr, false
}

func secretPaths(managed []managedSecret) []string {
	out := make([]string, len(managed))
	for i, m := range managed {
		out[i] = m.path
	}
	return out
}

// configDirFile reports whether rel, a path inside a directory that is both
// the config and the data dir (the macOS default), belongs to the config
// dir. The names come from the same helpers that build the paths.
func configDirFile(rel string) bool {
	const base = "cfg"
	files := []string{
		launchConfigPath(base),
		credentialsPath(base),
		dashboardAuthTokenPath(base),
		chainKeyPath(base),
		chainAnchorPath(base),
		filepath.Join(base, filepath.Base(claudecode.LedgerPath())),
	}
	files = append(files, pluginConfigSecrets(base)...)
	files = append(files, pluginConfigFiles(base)...)
	files = append(files, peering.AtRestPaths(base)...)
	for _, f := range files {
		if r, err := filepath.Rel(base, f); err == nil && filepath.ToSlash(r) == rel {
			return true
		}
	}
	if strings.HasPrefix(rel, "settings.") && strings.HasSuffix(rel, ".json") && !strings.Contains(rel, "/") {
		return true
	}
	soulDir, _ := filepath.Rel(base, soul.SoulDir(base))
	mcpDir, _ := filepath.Rel(base, mcpCacheDir(base))
	for _, dir := range []string{soulDir, mcpDir} {
		if strings.HasPrefix(rel, filepath.ToSlash(dir)+"/") {
			return true
		}
	}
	return false
}

// outsideTenantDirs names what Tenant uses that a backup of its two
// directories does not reach, so the operator knows to back it up another way.
func outsideTenantDirs(c *commonFlags) []backup.Skipped {
	var out []backup.Skipped
	add := func(path, what string) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(expandPath(path))
		if err != nil || isInside(abs, c.cfgDir) || isInside(abs, c.dataDir) {
			return
		}
		if _, err := os.Stat(abs); err != nil {
			return
		}
		out = append(out, backup.Skipped{Path: abs, Reason: what + ", outside Tenant's directories; back it up separately"})
	}
	if c.lc != nil {
		for _, d := range plugins().All() {
			s := c.lc.Skills[d.ID]
			if d.External == nil || s == nil || !s.Enabled || s.Settings == nil {
				continue
			}
			for path, what := range d.External(s.Settings) {
				add(path, what)
			}
		}
	}
	add(c.logsRoot(), "log book")
	add(claudecode.LedgerPath(), "Claude Code usage ledger")
	return out
}

func isInside(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// checkBackupFile checks the backed-up files Tenant knows how to read.
func checkBackupFile(entryPath, path string) (newer, note string, err error) {
	if entryPath != "config/config.json" {
		return "", "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(b, &probe) != nil {
		return "", "not valid JSON; Tenant will start with defaults and warn", nil
	}
	if probe.SchemaVersion > currentSchemaVersion {
		return fmt.Sprintf("schema_version %d, and this Tenant writes %d", probe.SchemaVersion, currentSchemaVersion), "", nil
	}
	return "", "", nil
}

// storeSchemas builds each store's schema, as this binary creates it, in a
// scratch directory, once per run.
type storeSchemas struct {
	dir   string
	cache map[string]backup.Schema
}

func newStoreSchemas() (*storeSchemas, error) {
	dir, err := os.MkdirTemp("", "tenant-schema-")
	if err != nil {
		return nil, err
	}
	return &storeSchemas{dir: dir, cache: map[string]backup.Schema{}}, nil
}

func (s *storeSchemas) Close() { _ = os.RemoveAll(s.dir) }

// reference opens a fresh store the way Tenant does, so a table a feature
// creates later (the SME tables in facts.db) is part of the reference too.
func (s *storeSchemas) reference(ctx context.Context, entryPath string) (backup.Schema, bool, error) {
	if sc, ok := s.cache[entryPath]; ok {
		return sc, true, nil
	}
	path := filepath.Join(s.dir, filepath.Base(entryPath))
	var closeStore func() error
	switch entryPath {
	case "data/episodes.db":
		st, err := episodic.Open(path)
		if err != nil {
			return nil, false, err
		}
		closeStore = st.Close
	case "data/facts.db":
		st, err := semantic.Open(path)
		if err != nil {
			return nil, false, err
		}
		if _, err := sme.New(st.DB()); err != nil {
			st.Close()
			return nil, false, err
		}
		closeStore = st.Close
	case "data/skills.db":
		st, err := skills.Open(path)
		if err != nil {
			return nil, false, err
		}
		closeStore = st.Close
	case "data/runs.db":
		st, err := runtrace.Open(path, runtrace.Config{})
		if err != nil {
			return nil, false, err
		}
		closeStore = st.Close
	case "data/usage.db":
		st, err := usagestore.Open(path)
		if err != nil {
			return nil, false, err
		}
		closeStore = st.Close
	case "data/tenant_meta.db":
		st, err := improve.OpenMeta(path)
		if err != nil {
			return nil, false, err
		}
		closeStore = st.Close
	default:
		if filepath.Base(entryPath) != "logbook.db" { // wherever logs.dir puts it in the data dir
			return nil, false, nil
		}
		st, err := store.Open(path, store.Options{})
		if err != nil {
			return nil, false, err
		}
		closeStore = st.Close
	}
	if err := closeStore(); err != nil {
		return nil, false, err
	}
	sc, err := backup.ReadSchema(ctx, path)
	if err != nil {
		return nil, false, err
	}
	s.cache[entryPath] = sc
	return sc, true, nil
}

// quoteArg quotes a path for a copy-paste command line when it has spaces.
func quoteArg(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}
