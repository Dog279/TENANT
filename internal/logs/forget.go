package logs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ForgetLines blanks every line of the text files under root (the mirror and
// the folders other processes write) that match selects: a privacy request
// removing a session (docs/design/logging-v2.md §6.5). It returns how many
// lines it blanked; with apply false it changes nothing and only counts.
//
// A line is blanked in place: its bytes become spaces, which every reader
// skips, and the file keeps its length. Serve, the TUI and scripts append to
// these files with no lock between them, and appends land past the lines
// this overwrites, so no process loses a line, on any OS. A rewrite into a
// new file would lose whatever another process appended to the old one
// meanwhile, and on Windows could not replace a file it holds open.
func ForgetLines(root string, match func(Entry) bool, apply bool) (int, error) {
	dirs, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range dirs {
		if !d.IsDir() || !ValidSource(d.Name()) || recordDirs[d.Name()] {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			return n, err
		}
		for _, f := range files {
			if f.IsDir() || !fileRe.MatchString(f.Name()) {
				continue
			}
			k, err := forgetFileLines(filepath.Join(root, d.Name(), f.Name()), d.Name(), match, apply)
			n += k
			if err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func forgetFileLines(path, source string, match func(Entry) bool, apply bool) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var spans [][2]int
	for off := 0; ; {
		nl := bytes.IndexByte(b[off:], '\n')
		if nl < 0 {
			break // a line still being written is left for the next request
		}
		if e, ok := ParseLine(string(b[off : off+nl])); ok {
			e.Source = source
			if match(e) {
				spans = append(spans, [2]int{off, off + nl})
			}
		}
		off += nl + 1
	}
	if !apply || len(spans) == 0 {
		return len(spans), nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	for i, s := range spans {
		if _, err := f.WriteAt(bytes.Repeat([]byte{' '}, s[1]-s[0]), int64(s[0])); err != nil {
			_ = f.Close()
			return i, err
		}
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return len(spans), err
}
