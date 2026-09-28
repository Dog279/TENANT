package backup

import (
	"path"
	"path/filepath"
	"regexp"
)

// The log book (docs/design/logging-v2.md §6.5, TEN-374) lives in the logs
// root, <data>/logs unless logs.dir moves it:
//   - logbook.db is captured with VACUUM INTO, known by its name. The process
//     taking the backup may hold it open through its own log hub, and on
//     POSIX opening and closing the raw file, even just to read its header,
//     drops every lock the process holds on it.
//   - the text mirror, <log>/YYYY-MM-DD[.N].log, holds the same events as
//     logbook.db and is left out unless asked for;
//   - the archives (archive/) and anything else are captured as usual.
//
// A log book moved outside the data dir is not in the backup at all; the
// caller lists it in NotIncluded.

// mirrorDay matches a day file of the text mirror.
var mirrorDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(\.\d+)?\.log$`)

// logbookFile classifies a file under the logs root: the database (sqlite
// true) or a mirror day file to leave out (mirror true). Anything else is
// neither.
func logbookFile(abs, logsDir string) (sqlite, mirror bool) {
	rel, err := filepath.Rel(canonical(logsDir), canonical(abs))
	if err != nil || !filepath.IsLocal(rel) {
		return false, false
	}
	rel = filepath.ToSlash(rel)
	dir := path.Dir(rel)
	return rel == "logbook.db", dir != "." && dir != "archive" && path.Dir(dir) == "." && mirrorDay.MatchString(path.Base(rel))
}
