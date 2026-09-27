package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/memory/archive"
)

// archiveChange is what a plan does to one archive month or session file.
type archiveChange struct {
	path       string
	removeDir  bool // a whole month directory
	removeFile bool
	drop       int // events removed
	total      int // events in the file (0 for a removed directory)
	samples    []string
}

// archiveEvent is the part of an archived event the planner reads.
type archiveEvent struct {
	TS        time.Time       `json:"ts"`
	AgentID   string          `json:"agent_id"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

// turnWindow is how long after the user's message an episode may be recorded
// and still be matched to it.
const turnWindow = 60

// archiveChanges works out, file by file, what the selection removes from the
// archive: whole months, whole sessions, and events by time, agent, text or
// forgotten turn.
func archiveChanges(dataDir string, s *selection) ([]archiveChange, error) {
	root := archive.ArchiveDir(dataDir)
	months, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("privacy: archive: %w", err)
	}
	sessionFiles := map[string]bool{}
	for id := range s.sessions {
		sessionFiles[archive.SessionFileName(id)] = true
	}
	turns := turnsBySession(s)
	var out []archiveChange
	for _, m := range months {
		if !m.IsDir() {
			continue
		}
		start, err := time.Parse("2006-01", m.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, m.Name())
		wholeMonth := monthSelected(s, m.Name())
		if wholeMonth && s.agent == "" {
			out = append(out, archiveChange{path: dir, removeDir: true})
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("privacy: archive: %w", err)
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			path := filepath.Join(dir, name)
			if sessionFiles[name] && s.agent == "" {
				out = append(out, archiveChange{path: path, removeFile: true})
				continue
			}
			touched := wholeMonth || sessionFiles[name] || s.match != "" || len(turns[name]) > 0 ||
				(!s.archiveBefore.IsZero() && start.Before(s.archiveBefore))
			if !touched {
				continue
			}
			c, err := scanArchiveFile(path, s, wholeMonth || sessionFiles[name], turns[name])
			if err != nil {
				return nil, err
			}
			if c.drop > 0 {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// scanArchiveFile decides which events of one file go. all removes every
// event of the selected agent.
func scanArchiveFile(path string, s *selection, all bool, turns []episodeRef) (archiveChange, error) {
	lines, _, err := readCompleteLines(path)
	if err != nil {
		return archiveChange{}, err
	}
	drop := dropMask(lines, s, all, turns)
	c := archiveChange{path: path, total: len(lines)}
	for i, d := range drop {
		if !d {
			continue
		}
		c.drop++
		if len(c.samples) < 3 {
			var ev archiveEvent
			if json.Unmarshal(lines[i], &ev) == nil && ev.Content != "" {
				c.samples = append(c.samples, ev.Role+": "+clip(ev.Content, 60))
			}
		}
	}
	c.removeFile = c.drop == c.total
	return c, nil
}

// dropMask marks the events that go.
func dropMask(lines [][]byte, s *selection, all bool, turns []episodeRef) []bool {
	events := make([]archiveEvent, len(lines))
	valid := make([]bool, len(lines))
	for i, l := range lines {
		valid[i] = json.Unmarshal(l, &events[i]) == nil
	}
	mine := func(i int) bool { return valid[i] && (s.agent == "" || events[i].AgentID == s.agent) }
	drop := make([]bool, len(lines))
	for i := range lines {
		if !mine(i) {
			continue
		}
		switch {
		case all:
			drop[i] = true
		case !s.archiveBefore.IsZero() && events[i].TS.Before(s.archiveBefore):
			drop[i] = true
		case s.match != "" && mentions(lines[i], s.match):
			drop[i] = true
		}
	}
	// A forgotten turn runs from the user's message to the next one.
	for _, t := range turns {
		start := -1
		for i := range lines {
			if mine(i) && events[i].Role == "user" && events[i].Content == t.prompt &&
				events[i].TS.Unix() <= t.ts+turnWindow {
				start = i
			}
		}
		if start < 0 {
			continue
		}
		for i := start; i < len(lines); i++ {
			if i > start && valid[i] && events[i].Role == "user" {
				break
			}
			if mine(i) {
				drop[i] = true
			}
		}
	}
	return drop
}

// mentions reports whether any string inside an event contains match.
// Decoding first means escaped text (quotes, non-ASCII) is compared as the
// words it stands for.
func mentions(line []byte, match string) bool {
	var v any
	if json.Unmarshal(line, &v) != nil {
		return containsFold(string(line), match)
	}
	return walkStrings(v, func(s string) bool { return containsFold(s, match) })
}

func walkStrings(v any, fn func(string) bool) bool {
	switch x := v.(type) {
	case string:
		return fn(x)
	case []any:
		for _, e := range x {
			if walkStrings(e, fn) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if fn(k) || walkStrings(e, fn) {
				return true
			}
		}
	}
	return false
}

// readCompleteLines returns the newline-terminated lines of path, without
// their newlines, and the file's size. An unterminated tail is a write in
// progress or a torn one, and archive readers skip it too.
func readCompleteLines(path string) ([][]byte, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	size := int64(len(data))
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[:i+1]
	} else {
		data = nil
	}
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		lines = append(lines, data[:i])
		data = data[i+1:]
	}
	return lines, size, nil
}

// applyArchiveChange carries out one change. A rewrite recomputes the events
// to drop from the file as it is now, writes the rest to a temporary file and
// renames it over the original, and starts over if the file grew meanwhile,
// so an event the live archive appends during the rewrite is not lost.
func applyArchiveChange(c archiveChange, s *selection, turns map[string][]episodeRef) (int, error) {
	if c.removeDir {
		if err := os.RemoveAll(c.path); err != nil {
			return 0, fmt.Errorf("privacy: remove %s: %w", c.path, err)
		}
		return 0, nil
	}
	base := filepath.Base(c.path)
	all := sessionSelected(s, base) || monthSelected(s, filepath.Base(filepath.Dir(c.path)))
	for attempt := 0; attempt < 5; attempt++ {
		lines, size, err := readCompleteLines(c.path)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		drop := dropMask(lines, s, all, turns[base])
		var keep bytes.Buffer
		dropped := 0
		for i, l := range lines {
			if drop[i] {
				dropped++
				continue
			}
			keep.Write(l)
			keep.WriteByte('\n')
		}
		if dropped == 0 {
			return 0, nil
		}
		if unchanged, err := sameSize(c.path, size); err != nil || !unchanged {
			continue
		}
		if keep.Len() == 0 {
			if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return 0, fmt.Errorf("privacy: remove %s: %w", c.path, err)
			}
			return dropped, nil
		}
		tmp := c.path + ".privacy.tmp"
		f, err := fsperm.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return 0, err
		}
		if _, err := f.Write(keep.Bytes()); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(tmp)
			return 0, err
		}
		if unchanged, err := sameSize(c.path, size); err != nil || !unchanged {
			_ = os.Remove(tmp)
			continue
		}
		if err := os.Rename(tmp, c.path); err != nil {
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("privacy: rewrite %s: %w", c.path, err)
		}
		return dropped, nil
	}
	return 0, fmt.Errorf("privacy: %s kept changing while it was being rewritten; run the forget again", c.path)
}

// monthSelected reports whether a whole archive month goes: named outright,
// or ending before the cutoff.
func monthSelected(s *selection, month string) bool {
	if s.archiveMonths[month] {
		return true
	}
	start, err := time.Parse("2006-01", month)
	return err == nil && !s.archiveBefore.IsZero() && !start.AddDate(0, 1, 0).After(s.archiveBefore)
}

// removeEmptyMonths drops archive month directories a forget or prune left
// empty.
func removeEmptyMonths(dataDir string) {
	root := archive.ArchiveDir(dataDir)
	months, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, m := range months {
		if m.IsDir() {
			_ = os.Remove(filepath.Join(root, m.Name())) // only succeeds when empty
		}
	}
}

func sessionSelected(s *selection, fileName string) bool {
	for id := range s.sessions {
		if archive.SessionFileName(id) == fileName {
			return true
		}
	}
	return false
}

func sameSize(path string, size int64) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Size() == size, nil
}

// turnsBySession groups forgotten turns by archive file name.
func turnsBySession(s *selection) map[string][]episodeRef {
	out := map[string][]episodeRef{}
	if !s.derive {
		return out
	}
	for _, ep := range s.episodes {
		if ep.session != "" && !s.sessions[ep.session] {
			f := archive.SessionFileName(ep.session)
			out[f] = append(out[f], ep)
		}
	}
	return out
}
