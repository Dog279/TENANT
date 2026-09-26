package backup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"tenant/internal/fsperm"
)

// copyLog copies an append-only JSONL log up to and including its last
// newline, as the file stands when the copy starts. A record being appended
// while the backup runs is left for the next backup instead of arriving torn,
// which matches how the archive itself treats an unterminated tail.
func copyLog(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	end, err := lastNewline(in, info.Size())
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	out, err := fsperm.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(in, 0, end)); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return out.Close()
}

// lastNewline returns the offset just past the last '\n' in the first size
// bytes of f, or 0 when there is none.
func lastNewline(f *os.File, size int64) (int64, error) {
	const block = 64 << 10
	buf := make([]byte, block)
	for end := size; end > 0; {
		start := end - block
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// maxLogLine matches the archive reader's largest accepted record.
const maxLogLine = 4 << 20

// checkLog parses every line of a JSONL log. A backup's copy is cut at a
// newline, so every line must be a complete JSON value; the first one that is
// not is reported with its line number.
func checkLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLine)
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		if !json.Valid(b) {
			return fmt.Errorf("line %d is not valid JSON", line)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("after line %d: %w", line, err)
	}
	return nil
}
