package dashboard

// rest_logs.go is the LIVE LOGGING API over the unified log book: the JSON
// surface external add-ons, sidecar scripts and the `tenant log` CLI use to
// read the folders and to write lines into them. A source is created on first
// write — folder on disk, tab on the Activity page — so a new subsystem or a
// custom automation joins the audited record with a single POST and no rebuild.
// Auth is the same Bearer token as every other /api route.
//
//	GET  /api/logs                      → {"root": "...", "sources": [{name, native, count, last_at, err}]}
//	GET  /api/logs/{source}             → {"source": "x", "files": [{name, size, mod_time}]}
//	GET  /api/logs/{source}/{name}      → the raw file (text/plain)
//	POST /api/logs/{source}             ← {"level": "info", "msg": "...", "fields": {"k": "v"}}  (or a text/plain body = the message)

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
)

type restLogSource struct {
	Name   string `json:"name"`
	Native bool   `json:"native"`
	Count  int    `json:"count"`
	LastAt string `json:"last_at,omitempty"`
	Err    string `json:"err,omitempty"`
}

type restLogSourcesResponse struct {
	Root    string          `json:"root"`
	Sources []restLogSource `json:"sources"`
}

type restLogFile struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

type restLogFilesResponse struct {
	Source string        `json:"source"`
	Files  []restLogFile `json:"files"`
}

// restLogWrite is the POST body. Level defaults to info; msg is required
// (or the whole body when it is text/plain); fields are free-form strings.
type restLogWrite struct {
	Level  string            `json:"level,omitempty"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

const (
	logWriteMaxBody   = 64 << 10
	logWriteMaxMsg    = 8 << 10
	logWriteMaxFields = 32
)

func (s *Server) handleLogSources(w http.ResponseWriter, _ *http.Request) {
	resp := restLogSourcesResponse{Sources: []restLogSource{}}
	if s.logs == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Root = s.logs.Root()
	for _, src := range s.logs.Sources() {
		v := restLogSource{Name: src.Name, Native: src.Native, Count: src.Count, Err: src.Err}
		if !src.LastAt.IsZero() {
			v.LastAt = src.LastAt.UTC().Format(time.RFC3339)
		}
		resp.Sources = append(resp.Sources, v)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogFiles(w http.ResponseWriter, r *http.Request) {
	src := r.PathValue("source")
	if !logs.ValidSource(src) {
		writeError(w, http.StatusBadRequest, "source: lowercase [a-z0-9][a-z0-9._-]{0,39} required")
		return
	}
	resp := restLogFilesResponse{Source: src, Files: []restLogFile{}}
	if s.logs == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	files, err := s.logs.Files(src)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, f := range files {
		resp.Files = append(resp.Files, restLogFile{Name: f.Name, Size: f.Size, ModTime: f.ModTime.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLogFile is the API twin of the Activity page's raw-file view.
func (s *Server) handleLogFile(w http.ResponseWriter, r *http.Request) {
	s.handleActivityFile(w, r)
}

// handleLogWrite files one line into the named source, creating it on first
// use. Accepts JSON (restLogWrite) or a text/plain body (the message).
func (s *Server) handleLogWrite(w http.ResponseWriter, r *http.Request) {
	src := logs.NormalizeSource(r.PathValue("source"))
	if src == "" {
		writeError(w, http.StatusBadRequest, "source: needs at least one letter or digit ([a-z0-9._-], ≤40 chars after normalization)")
		return
	}
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "log book not configured")
		return
	}
	var req restLogWrite
	body := http.MaxBytesReader(w, r.Body, logWriteMaxBody)
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	} else {
		raw, err := io.ReadAll(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "unreadable body")
			return
		}
		req.Msg = string(raw)
		req.Level = r.URL.Query().Get("level")
	}
	req.Msg = strings.TrimSpace(req.Msg)
	if req.Msg == "" {
		writeError(w, http.StatusBadRequest, "msg required")
		return
	}
	if len(req.Msg) > logWriteMaxMsg {
		req.Msg = req.Msg[:logWriteMaxMsg]
	}
	lvl, ok := logs.ParseLevel(req.Level)
	if !ok {
		writeError(w, http.StatusBadRequest, "level: debug|info|warn|error")
		return
	}
	e := logs.Entry{Source: src, Level: lvl, Msg: req.Msg}
	if len(req.Fields) > logWriteMaxFields {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("fields: at most %d", logWriteMaxFields))
		return
	}
	keys := make([]string, 0, len(req.Fields))
	for k := range req.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " \t\n=\"") || len(k) > 64 {
			writeError(w, http.StatusBadRequest, "fields: keys are 1-64 chars with no spaces, quotes or '='")
			return
		}
		e.Fields = append(e.Fields, logs.Field{Key: k, Value: req.Fields[k]})
	}
	s.logs.Log(e)
	// Mirror onto the live broker so an open TUI feed sees the line too, as
	// add-on events always have.
	if s.broker != nil {
		s.broker.Publish(agent.Event{Kind: agent.EventAddon, Agent: src, Source: src, Text: req.Msg, IsErr: lvl >= slog.LevelError})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "ok", "source": src, "seq": strconv.FormatUint(s.logs.Head(), 10)})
}
