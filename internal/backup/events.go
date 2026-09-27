package backup

import (
	"context"

	"tenant/internal/logs/event"
)

// Backup is the backup provider (docs/EVENTS.md): each backup, verify and
// restore is an activity, with its manifest's counts and its result. The
// Security log's records of backups and restores are separate (TEN-398).
var Backup = event.NewProvider("backup", "backup")

// Backups (1000s).

var CreateRun = Backup.Event(1001, event.Info, "Backup", "Back up to {path}", "path").
	Doc("A backup of the config and data directories: its start, then its stop with the duration, or its failure with the error.",
		"The operator ran tenant backup create.",
		"None when it stops. On a failure, read the error; nothing is left at the path.")

var Written = Backup.Event(1002, event.Info, "Backup", "Backup written to {path}: {files} files, {bytes} bytes", "path", "bytes", "files", "databases", "logs", "secrets", "skipped", "encrypted", "version").
	Doc("A backup file was written. skipped counts what was left out, such as credentials in an unencrypted backup.",
		"A backup finished.",
		"Check it now and then with tenant backup verify, and keep it somewhere other than this machine.")

var CreateWarning = Backup.Event(2001, event.Warning, "Backup", "Backup {path}: {warning}", "path", "warning").
	Doc("A backup was written, but something about it needs attention.",
		"Usually the destination cannot restrict access to the file (a FAT or exFAT drive), and the backup is not encrypted.",
		"Keep the file somewhere private, or back up again with --encrypt.")

// Verification (1000s, 3000s).

var VerifyRun = Backup.Event(1003, event.Info, "Verify", "Verify backup {path}", "path").
	Doc("A backup file was checked: its start, then its stop with the duration, or its failure with the error. Its findings follow as their own event.",
		"The operator ran tenant backup verify.",
		"None when it stops. A failure means the file could not be read at all: a wrong path, or a missing or wrong passphrase or key.")

var Checked = Backup.Event(1004, event.Info, "Verify", "Backup {path} checks out: {files} files, made {created} by Tenant {version}", "path", "files", "created", "version", "encrypted", "newer", "notes").
	Doc("Every checksum in the backup matches, every database passes its integrity check and every log parses. newer counts schema from a newer Tenant, which a restore refuses unless forced.",
		"A verify or a restore read the backup.",
		"None.")

var CheckFailed = Backup.Event(3001, event.Error, "Verify", "Backup {path} failed verification with {count} problems, the first: {problem}", "path", "count", "problem", "problems").
	Doc("The backup is damaged: a checksum does not match, the file is cut short, a database fails its integrity check or a log does not parse. It cannot be restored.",
		"The file was changed or cut short after it was written, or the disk or the copy damaged it.",
		"Use another backup, and make a fresh one with tenant backup create.")

// Restores (1000s).

var RestoreRun = Backup.Event(1005, event.Info, "Restore", "Restore backup {path}", "path", "dry_run").
	Doc("A restore of a backup over the config and data directories: its start, then its stop with the duration, or its failure with the error. A dry run only verifies.",
		"The operator ran tenant backup restore.",
		"None when it stops. On a failure nothing was changed; read the error.")

var Restored = Backup.Event(1006, event.Info, "Restore", "Restored backup {path}", "path", "aside", "kept_secrets").
	Doc("The backup replaced the config and data directories. aside names where the previous ones are kept.",
		"A restore finished.",
		"Start Tenant and check it; delete the directories named in aside once you are happy. Don't run this install alongside the machine the backup came from.")

// logReport files what reading the backup at path found.
func logReport(ctx context.Context, path string, rep *Report) {
	if !rep.OK() {
		CheckFailed.Log(ctx, "path", path, "count", len(rep.Problems), "problem", rep.Problems[0], "problems", rep.Problems)
		return
	}
	m := rep.Manifest
	Checked.Log(ctx, "path", path, "files", len(m.Entries), "created", m.CreatedAt, "version", m.TenantVersion,
		"encrypted", rep.Encrypted, "newer", len(rep.Newer), "notes", len(rep.Notes))
}
