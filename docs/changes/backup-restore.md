# Change: `tenant backup` — one-file backup, verify and restore of every store (OPS-002)

**Branch:** `feat/ten-305-backup` · **Status:** implemented and tested on Windows, with the
POSIX paths run as Linux binaries under WSL; not yet deployed.
**Ticket:** OPS-002 / TEN-305.

## What changed
Three commands back up and restore everything Tenant keeps in its two directories:

```
tenant backup [create] [--out FILE|DIR] [--encrypt | --passphrase-file F | --recipient age1…]
tenant backup verify FILE [--passphrase-file F | --identity F] [--json]
tenant backup restore FILE [--dry-run] [--yes] [--force] [--passphrase-file F | --identity F]
```

- **One file, every store.** A backup is a gzip-compressed tar holding the config dir and the
  data dir: the six SQLite stores (`episodes`, `facts`, `skills`, `runs`, `usage`,
  `tenant_meta`), the archive, souls and soul proposals, `config.json`, `settings.*.json`,
  research runs, the log book, eval artifacts, relay attachments and everything else in
  them. Its first entry, `MANIFEST.json`, lists every other file in order with its size,
  SHA-256 and kind, plus the Tenant version, platform and time it was made.
- **Consistent while Tenant runs.** A running hub can keep working through a backup.
  - Each SQLite store is captured with `VACUUM INTO` on a read-only connection. That reads
    one snapshot, WAL included, and produces a self-contained file, so `-wal`, `-shm` and
    `-journal` files are never copied. A store left mid-transaction by a crash cannot be
    read read-only; only then is it opened read-write, to run the same recovery Tenant runs
    when it next opens it.
  - Each `.jsonl` log is copied up to its last complete record. A line still being appended
    is left for the next backup, the same way the archive treats a torn tail.
  - Stores are captured before the stores they point to: facts and skills first, then the
    distill cursor, episodes, runs, and the archive last. Every reference in the backup
    therefore finds its target.
  - The snapshots are staged in a private directory inside the data dir, then streamed out.
    The finished file appears under its final name only once it is complete.
- **Secrets only when encrypted.** `--encrypt` asks for a passphrase. `--passphrase-file`
  or `TENANT_BACKUP_PASSPHRASE` supplies one without a prompt. `--recipient` /
  `--recipients-file` encrypt to [age](https://age-encryption.org) public keys, so a
  scheduled backup needs no passphrase on the machine.
  - An encrypted backup carries every file on the managed secret list (`credentials.json`,
    peer key and certificate, OAuth caches, dashboard token, X token).
  - An unencrypted one leaves those out and says so.
  - Encrypted backups are standard age files: `age -d` opens them without Tenant.
- **What a backup leaves out, and says so:**
  - the lock file, temp and partial files, `doctor --fix`'s `.corrupted-*` evidence, and
    the wiki search index (rebuilt on next use);
  - things Tenant uses outside its two directories: the wiki vault, the SQL plugin's
    database, a relocated log book, and the Claude Code usage ledger when `--config` is
    not the default.

  The manifest records all of these, and `create` prints them.
- **`verify` checks everything.** It unpacks into a private scratch dir and checks:
  - every checksum;
  - that the stream ends where the manifest says, with no extra, missing or reordered
    entries and no trailing bytes (for an age file, that its final chunk is present);
  - every database passes `PRAGMA integrity_check`;
  - every log line parses as JSON;
  - the schema.

  It exits non-zero on any problem.
- **Schema check.** None of the SQLite stores carries a version number; they migrate
  additively when opened. So `verify` builds each store the way this binary does, in a
  scratch dir, and compares tables and columns.
  - A column this Tenant does not know means the backup came from a newer Tenant. Restore
    refuses that without `--force`.
  - An unknown table is noted and left alone.
  - A backup from an older Tenant passes; the stores add what it lacks when they open.
  - `config.json`'s `schema_version` is checked the same way.
- **`restore` never touches the live directories until the backup has passed.**
  - It unpacks into `<dir>.restore-<time>` beside each live directory and runs every check
    there.
  - Only after that does it rename the live directories to `<dir>.pre-restore-<time>` and
    the restored ones into place. If a rename fails, whatever moved is put back.
  - The old directories are kept. Delete them when you are happy.
  - `--dry-run` does everything except the renames.
  - `restore` asks before it starts unless you pass `--yes`.
- **No restore under a running Tenant.** Every command now holds a shared lock on
  `<data>/.tenant.lock` for as long as it runs (`internal/datalock`: `flock` on POSIX,
  `LockFileEx` on Windows). `restore` takes it exclusively.
  - A command started during a restore is told to wait.
  - A hub started by an older binary does not hold the lock, so `restore` also refuses
    while a dashboard answers at the configured address.
  - On Windows, a directory with open files cannot be renamed at all.
- **Secrets survive a plain restore.** When the backup carries no secrets, the live
  credential and key files are copied into the restored directories, so restoring an
  unencrypted backup does not sign you out of everything. Restored secrets are re-protected
  through the secrets package: `0600`/`0700`, or a current-user-only DACL on Windows.
- **The macOS layout.** By default, config and data are one directory on macOS. A backup
  from that layout still files the config files under `config/`, using the same path
  helpers Tenant uses. It can be restored onto separate directories (the launchd layout,
  Linux, Windows) and the other way round. Nested but different directories are refused.
- **The backup file** is owner-only (`0600`, or a current-user-only DACL), set on the file
  alone and not on the folder you chose. A destination that cannot restrict access (FAT,
  exFAT) gets a warning for an unencrypted backup instead of a failed one.

## Why
The archive is the source of truth that compaction and provenance rebuild from, and the
SQLite stores hold distilled memory that cannot be reproduced. There was no way to copy them
together, and the one existing path, `scripts/migrate-to-server.ps1`, copied the live
directories file by file with `-wal`/`-shm` included and the hub still running. Restoring
needs more than copying: schema compatibility, integrity, and a guarantee that a bad backup
cannot destroy good data.

## Evidence
- **`internal/backup`:**
  - round trips: plain, passphrase, X25519, and armored input;
  - wrong or missing passphrase;
  - damage: altered, reordered, missing, extra, resized, symlinked and truncated entries,
    trailing bytes, a truncated age stream, plaintext secrets, and a tar without a
    manifest;
  - manifest traversal and portability rules;
  - SQLite snapshots while a writer commits, a source left byte-identical, a scribbled page
    failing integrity, and newer versus older schema;
  - torn-tail log copies;
  - capture order;
  - secrets only when encrypted;
  - the full restore: kept-aside dirs, secrets carried over, refusal under a running
    Tenant, dry run and damage changing nothing, the newer-schema gate and `--force`;
  - macOS-layout backups restored onto split dirs and back, and a commit still in the WAL
    reaching the backup;
  - directory swaps with rollback.
- **`internal/datalock`:** shared holders coexist, and exclusive excludes both ways.
- **`internal/secrets`:** `ProtectFile` protects the file and leaves the parent alone.
- **`cmd/tenant`:**
  - create, verify and restore over real stores;
  - the confirmation prompt;
  - refusal while a hub answers;
  - encrypted flows with a passphrase file and the environment;
  - the macOS config-file routing;
  - the `config.json` schema check;
  - the data-dir lock held by every command and skipped by read-only `doctor`.
- **POSIX:** the backup file is `0600` in a folder whose mode is left alone, and everything a
  restore writes is `0600` / `0700` even when the source was looser. Run as a Linux binary
  under WSL, together with the rest of the backup, lock and secrets suites.
- **Mutation testing:** 47 targeted mutations of the checks above; 46 are caught. The
  survivor is equivalent: an early "already exists" exit that the final publish step
  repeats.
- **Live run:** the Windows binary against a throwaway install, covering create, verify,
  dry run, encrypted create and restore, and a four-byte corruption caught by `verify`.

## Operator notes
- A good routine: `tenant backup --recipient age1… --out D:\backups` from cron or Task
  Scheduler, then `tenant backup verify` now and then. Keep the private key somewhere other
  than the machine you are backing up.
- A restore brings back the backed-up **identity**: `instance_id`, the peer key and paired
  peers, the dashboard token and the Discord relay setup. Do not run the restored copy
  alongside the machine the backup came from.
- `runs.db` retention still applies after a restore: an old backup's run records past
  `runs.max_age_days` are pruned the next time the store opens.
- A backup is a point-in-time copy of Tenant's two directories. Your wiki vault and any SQL
  database the SQL plugin points at are your own folders; back them up with them.
