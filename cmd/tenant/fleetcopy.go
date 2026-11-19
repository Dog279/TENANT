package main

// fleetcopy.go is fleet_copy: one text file between this computer and one
// machine, moved with the file tools of both ends and nothing else, so the
// path a model names is checked on each side by the same code that checks it
// for every other read and write there (the roots, the secret list, the write
// approval), in both directions.
//
// The file tools carry text in pages, so that is what a copy is: UTF-8 text,
// at most fleetMaxContent bytes (a cap, not paging: one write must fit one
// frame of the link), no line longer than a page. The source is read page by
// page, written with one os_write_file, read back from where it landed, and
// the SHA-256 of what came back is compared with the source's. A machine has
// no tool that hashes a file, so the hash is taken here, of the whole file,
// on both passes; a copy that does not read back as it was sent is an error.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"tenant/internal/fleet"
	"tenant/internal/model"
)

// What os_read_file ends a page with (internal/plugins/osys, readLines).
var (
	fleetPageCut  = regexp.MustCompile(`…\[read stopped before line (\d{1,9}); next_offset: (\d{1,9})\]$`)
	fleetPageLong = regexp.MustCompile(`\n…\[line \d{1,9} is longer than one page; next_offset: \d{1,9}\]$`)
	fleetPageNone = regexp.MustCompile(`^\(no lines at offset \d{1,9}; the file has \d{1,9} lines\)$`)
)

var (
	errFleetPageLong  = errors.New("a line of it is longer than one page of the file tools (about 100K characters), and a copy moves whole lines")
	errFleetPageMoved = errors.New("it changed while it was read, or its last line reads like the file tools' own paging note")
)

// fleetSplitPage takes apart one os_read_file answer for a read from line
// offset: the file's own text, and the line the next page starts at (0: that
// was the last). The paging note is told from file text that merely looks
// like one by its numbers: a real one names the line right after the lines
// this page holds. Text that passes for a note all the same leads to a next
// read that finds no line there, which is refused (errFleetPageMoved).
func fleetSplitPage(out string, offset int) (body string, next int, err error) {
	// A note is the last hundred bytes of a page at most. Only the end is
	// searched: a megabyte from a machine is not run through a pattern.
	const note = 160
	from := max(0, len(out)-note)
	end := out[from:]
	switch {
	case offset > 1 && from == 0 && fleetPageNone.MatchString(end):
		return "", 0, errFleetPageMoved
	case fleetPageLong.MatchString(end):
		return "", 0, errFleetPageLong
	}
	if m := fleetPageCut.FindStringSubmatchIndex(end); m != nil {
		body := out[:from+m[0]]
		stop, _ := strconv.Atoi(end[m[2]:m[3]])
		resume, _ := strconv.Atoi(end[m[4]:m[5]])
		if lines := strings.Count(body, "\n"); stop == resume && stop == offset+lines && lines > 0 && strings.HasSuffix(body, "\n") {
			return body, resume, nil
		}
	}
	return out, 0, nil
}

// fleetFileEnd is one end of a copy: this computer's own file tools, or a
// machine's. call runs one file tool there. failed says the tool refused, and
// text is then its reason; err says the tool could not be asked at all.
type fleetFileEnd struct {
	name   string
	remote bool
	call   func(ctx context.Context, tool string, args any, read bool) (text string, failed bool, err error)
}

// fleetCopyFault is a copy that stopped: at which end, doing what, and why.
type fleetCopyFault struct {
	end  fleetFileEnd
	what string
	text string // the file tool's refusal, or the reason in the master's words
	err  error  // the end could not be asked
}

// fleetCopyPages is the most pages a file of fleetMaxContent bytes comes in.
// A page ends where the next line would not fit in 100,000 bytes, so two
// pages in a row hold more than that between them: about sixty-three pages
// for the largest file. A machine that sends more is not paging a file.
const fleetCopyPages = 80

// readAll reads the whole of path at one end, page by page.
func (e fleetFileEnd) readAll(ctx context.Context, path string) (string, *fleetCopyFault) {
	fault := func(text string, err error) (string, *fleetCopyFault) {
		return "", &fleetCopyFault{end: e, what: "reading " + path, text: text, err: err}
	}
	var b strings.Builder
	for offset, pages := 1, 1; ; pages++ {
		if pages > fleetCopyPages {
			return fault(fmt.Sprintf("it came in more than %d pages, more than a file one copy moves has", fleetCopyPages), nil)
		}
		out, failed, err := e.call(ctx, "os_read_file", struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
		}{path, offset}, true)
		switch {
		case err != nil:
			return fault("", err)
		case failed:
			return fault(out, nil)
		}
		body, next, err := fleetSplitPage(out, offset)
		if err != nil {
			return fault(err.Error(), nil)
		}
		if b.WriteString(body); b.Len() > fleetMaxContent {
			return fault(fmt.Sprintf("it is larger than the %d bytes (3 MiB) one copy moves", fleetMaxContent), nil)
		}
		if next == 0 {
			break
		}
		offset = next // always further on: fleetSplitPage saw at least one whole line
	}
	content := b.String()
	if !utf8.ValidString(content) || strings.ContainsRune(content, utf8.RuneError) {
		return fault("it is not UTF-8 text, and the file tools carry text", nil)
	}
	return content, nil
}

// local is this computer's end: its own file tools, with the turn's context,
// so its roots, its secret list and its write approval decide.
func (p *fleetToolPlugin) local() fleetFileEnd {
	return fleetFileEnd{name: "this computer", call: func(ctx context.Context, tool string, args any, _ bool) (string, bool, error) {
		if p.host == nil {
			return "", false, errors.New("its own file tools are not available here")
		}
		return p.host.Dispatch(ctx, model.ToolCall{ID: "fleet_copy", Name: tool, Arguments: fleetJSON(args)})
	}}
}

// end is the machine's end. A page the link clipped is not a page.
func (t *fleetTarget) end() fleetFileEnd {
	return fleetFileEnd{name: t.m.Name, remote: true, call: func(ctx context.Context, tool string, args any, read bool) (string, bool, error) {
		res, err := t.run(ctx, tool, fleetJSON(args), read)
		if err == nil && res.Clipped {
			err = errors.New("the machine cut its answer to fit the link, so the file's text is not whole")
		}
		return res.Text, res.IsError, err
	}}
}

// copy is fleet_copy.
func (p *fleetToolPlugin) copy(ctx context.Context, t *fleetTarget, a fleetArgs) (string, bool, error) {
	src, dst, from, to := p.local(), t.end(), a.LocalPath, a.RemotePath
	if a.Direction == "from_host" {
		src, dst, from, to = dst, src, to, from
	}
	stopped := func(f *fleetCopyFault) (string, bool, error) {
		if f.err != nil && f.end.remote {
			text, _, _ := t.say(ctx, "", fleet.Result{}, f.err)
			return "fleet_copy stopped " + f.what + ": " + text, true, nil
		}
		why := f.text
		if f.err != nil {
			why = f.err.Error()
		}
		why = fleetShown(why, 2000)
		if f.end.remote {
			t.label(ctx, why)
		}
		return fmt.Sprintf("fleet_copy stopped %s on %s: %s", fleetShown(f.what, 600), f.end.name, why), true, nil
	}
	content, fault := src.readAll(ctx, from)
	if fault != nil {
		return stopped(fault)
	}
	if out, failed, err := dst.call(ctx, "os_write_file", struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{to, content}, false); err != nil || failed {
		return stopped(&fleetCopyFault{end: dst, what: "writing " + to, text: out, err: err})
	}
	back, fault := dst.readAll(ctx, to)
	if fault != nil {
		fault.what = "after the write, reading back " + to
		return stopped(fault)
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	if want, got := sum(content), sum(back); want != got {
		return fmt.Sprintf("fleet_copy: SHA-256 MISMATCH. %s on %s reads back as %s (%d bytes); the source %s on %s is %s (%d bytes). "+
			"The file was written and is not a faithful copy: it was left as it is. Do not rely on it.",
			fleetShown(to, 600), dst.name, got, len(back), fleetShown(from, 600), src.name, want, len(content)), true, nil
	}
	return fmt.Sprintf("copied %s on %s to %s on %s: %d bytes, sha256 %s, read back from there and equal.",
		fleetShown(from, 600), src.name, fleetShown(to, 600), dst.name, len(content), sum(content)), false, nil
}
