package logapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
)

// The formats events are written in: exports, and tenant logs.
const (
	NDJSON = "ndjson" // each event's JSON on a line of its own
	JSON   = "json"   // the same: a run of events is NDJSON
	CSV    = "csv"    // a column row, then a row per event; the data as its JSON
	Text   = "text"   // the text mirror's line, after the event's seq
)

var (
	contentTypes = map[string]string{NDJSON: "application/x-ndjson", JSON: "application/x-ndjson", CSV: "text/csv; charset=utf-8", Text: "text/plain; charset=utf-8"}
	extensions   = map[string]string{NDJSON: "ndjson", JSON: "ndjson", CSV: "csv", Text: "log"}
	csvColumns   = []string{"seq", "time", "level", "log", "provider", "id", "task", "opcode", "keywords", "msg", "data",
		"trace", "activity", "related", "session", "approval", "tool_call", "actor", "surface", "pid", "boot", "host"}
)

// Writer writes events in one format.
type Writer struct {
	w      io.Writer
	format string
	loc    *time.Location // text only: the time zone its times are shown in
	csv    *csv.Writer
	cols   bool // the CSV column row is out
}

// NewWriter writes events to w in format, showing text times in loc.
func NewWriter(w io.Writer, format string, loc *time.Location) (*Writer, error) {
	if _, ok := contentTypes[format]; !ok {
		return nil, fmt.Errorf("format %q: ndjson, json, csv or text", format)
	}
	ew := &Writer{w: w, format: format, loc: loc}
	if format == CSV {
		ew.csv = csv.NewWriter(w)
	}
	return ew, nil
}

// Header writes an export's header record: in NDJSON the line
// {"export": {…}}, in CSV and text a # line.
func (ew *Writer) Header(h ExportHeader) error {
	if ew.format == NDJSON || ew.format == JSON {
		b, err := json.Marshal(map[string]ExportHeader{"export": h})
		if err == nil {
			_, err = ew.w.Write(append(b, '\n'))
		}
		return err
	}
	_, err := fmt.Fprintf(ew.w, "# tenant logbook export: schema=%d catalog=%s tenant=%s filter=%q exported=%s host=%s sensitive=%t\n",
		h.Schema, h.Catalog, h.Tenant, h.Filter, h.Exported.Format(time.RFC3339), h.Host, h.Sensitive)
	return err
}

// Write writes one event.
func (ew *Writer) Write(e event.Event) error {
	switch ew.format {
	case Text:
		_, err := io.WriteString(ew.w, TextLine(e, ew.loc)+"\n")
		return err
	case CSV:
		if !ew.cols {
			ew.cols = true
			if err := ew.csv.Write(csvColumns); err != nil {
				return err
			}
		}
		data, _ := event.EncodeData(e.Data)
		var id, pid string
		if e.ID != 0 {
			id = strconv.FormatUint(uint64(e.ID), 10)
		}
		if e.Proc.PID != 0 {
			pid = strconv.Itoa(e.Proc.PID)
		}
		c := e.Corr
		return ew.csv.Write([]string{strconv.FormatUint(e.Seq, 10), e.Time.UTC().Format(time.RFC3339Nano), e.Level.String(),
			e.Log, e.Provider, id, e.Task, e.Opcode, strings.Join(e.Keywords, " "), e.Msg, data,
			c.TraceID, c.ActivityID, c.RelatedID, c.SessionID, c.ApprovalID, c.ToolCallID, e.Actor, e.Surface, pid, e.Proc.BootID, e.Proc.Host})
	}
	b, err := json.Marshal(e)
	if err == nil {
		_, err = ew.w.Write(append(b, '\n'))
	}
	return err
}

// Flush writes out what the CSV writer holds: at least its column row.
func (ew *Writer) Flush() error {
	if ew.csv == nil {
		return nil
	}
	if !ew.cols {
		ew.cols = true
		_ = ew.csv.Write(csvColumns)
	}
	ew.csv.Flush()
	return ew.csv.Error()
}

// TextLine is e as the text mirror writes it (logs.EntryFromEvent: a
// sensitive value is «sensitive»), its time in loc, after its seq.
func TextLine(e event.Event, loc *time.Location) string {
	en := logs.EntryFromEvent(e)
	en.At = e.Time.In(loc)
	return strconv.FormatUint(e.Seq, 10) + " " + en.Line()
}
