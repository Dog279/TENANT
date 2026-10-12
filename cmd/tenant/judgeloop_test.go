package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/config"
	"tenant/internal/model"
	"tenant/internal/model/systemone"
)

// A server that answers in the wrong shape is a judge error (fail open), not
// a "no" on every check; rounds are clamped.
func TestJudgeLoop_WrongShapeAndClamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"answers":{"done":{"type":"choice","choice":"yes"},"every_part":{"type":"choice"},"claims_without_tool":{"type":"choice"},"gave_up":{"type":"choice"},"kind":{"type":"noul","noul":0.9}}}`)
	}))
	t.Cleanup(srv.Close)
	j := newJudgeLoop(t.TempDir(), &config.Judge{Enabled: true, Endpoint: srv.URL, Rounds: 500})
	if _, err := j.Check(context.Background(), agent.Exchange{Request: "r", Answer: "a"}); err == nil || !strings.Contains(err.Error(), "not yes/no") {
		t.Errorf("check err = %v", err)
	}
	if _, _, err := j.Triage(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not a choice") {
		t.Errorf("triage err = %v", err)
	}
	if j.MaxRounds() != config.JudgeMaxRounds {
		t.Errorf("max rounds %d, want clamped to %d", j.MaxRounds(), config.JudgeMaxRounds)
	}
}

// Every check must pass; the summary names the failures, and a nudge is
// sent once even when two checks ask for it.
func TestJudgeVerdict(t *testing.T) {
	ans := func(done, every, claims, gaveUp float64) map[string]systemone.Answer {
		return map[string]systemone.Answer{"done": {Noul: done}, "every_part": {Noul: every}, "claims_without_tool": {Noul: claims}, "gave_up": {Noul: gaveUp}}
	}
	if v := judgeVerdict(ans(0.90, 0.88, 0.10, 0.02)); !v.Done || v.Summary != "done 0.90" {
		t.Errorf("did it all: %+v", v)
	}
	v := judgeVerdict(ans(0.21, 0.09, 0.05, 0.29))
	if v.Done || v.Summary != "done 0.21, every_part 0.09" || strings.Count(v.Nudge, "finish what's missing") != 1 {
		t.Errorf("half done: %+v", v)
	}
	if v := judgeVerdict(ans(0.93, 0.90, 0.82, 0.02)); v.Done || !strings.Contains(v.Nudge, "no tool call shows") {
		t.Errorf("false claim: %+v", v)
	}
}

// The state lists every call (name and input) but only the last outputs,
// and counts calls past the cap instead of listing them.
func TestJudgeState(t *testing.T) {
	// 200 big calls: the state stays within its budget, keeps the newest calls,
	// counts the rest, and puts the request and answer first (a right-truncating
	// server must never lose them).
	var tools []agent.ToolCallResult
	for i := 0; i < 200; i++ {
		tools = append(tools, agent.ToolCallResult{Call: model.ToolCall{Name: "os_exec", Arguments: json.RawMessage(fmt.Sprintf(`{"command":"step %d %s"}`, i, strings.Repeat("x", 1000)))}, Result: strings.Repeat("y", 1000)})
	}
	st := judgeState(agent.Exchange{Request: strings.Repeat("r", 2*judgeTextChars), Tools: tools, Answer: strings.Repeat("é", judgeTextChars)})
	b, _ := json.Marshal(st)
	if len(b) > judgeStateChars+judgeStateChars/10 { // JSON escaping adds a little over the text budget
		t.Fatalf("state is %d bytes, budget %d", len(b), judgeStateChars)
	}
	if !strings.HasPrefix(string(b), `{"user_request":`) || strings.Index(string(b), `"final_answer":`) > strings.Index(string(b), `"tool_calls":`) {
		t.Fatalf("key order: %.60s", b)
	}
	var got struct {
		Calls   []map[string]any `json:"tool_calls"`
		Skipped int              `json:"earlier_tool_calls_not_listed"`
		Answer  string           `json:"final_answer"`
	}
	_ = json.Unmarshal(b, &got)
	if len(got.Calls) == 0 || len(got.Calls)+got.Skipped != 200 {
		t.Fatalf("calls %d, skipped %d", len(got.Calls), got.Skipped)
	}
	if first, last := got.Calls[0]["input"].(string), got.Calls[len(got.Calls)-1]["input"].(string); !strings.Contains(last, "step 199 ") || strings.Contains(first, "step 0 ") {
		t.Fatalf("not the newest calls, in order: first %.30q last %.30q", first, last)
	}
	withOutput := 0
	for _, c := range got.Calls {
		if c["output"] != nil {
			withOutput++
		}
	}
	if withOutput != judgeKeepOutputs || got.Calls[0]["output"] != nil || got.Calls[len(got.Calls)-1]["output"] == nil {
		t.Fatalf("outputs on %d calls, want the newest %d", withOutput, judgeKeepOutputs)
	}
	if !strings.HasSuffix(got.Answer, "…") || strings.ContainsRune(got.Answer, '�') {
		t.Fatalf("the answer wasn't cut on a rune boundary: …%q", got.Answer[len(got.Answer)-8:])
	}
	// A short exchange is sent whole, with an empty list rather than null.
	small, _ := json.Marshal(judgeState(agent.Exchange{Request: "hi", Answer: "hello"}))
	if string(small) != `{"user_request":"hi","final_answer":"hello","tool_calls":[]}` {
		t.Fatalf("small state: %s", small)
	}
}

// fakeSystemOne answers triage with kind and checks with the given P(true)s.
func fakeSystemOne(t *testing.T, kind string, done float64) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]any `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if _, ok := req.Questions["kind"]; ok {
			fmt.Fprintf(w, `{"answers":{"kind":{"type":"choice","choice":%q,"confidence":0.9}},"usage":{"input_tokens":40}}`, kind)
			return
		}
		fmt.Fprintf(w, `{"answers":{"done":{"type":"noul","noul":%v},"every_part":{"type":"noul","noul":%v},"claims_without_tool":{"type":"noul","noul":0.1},"gave_up":{"type":"noul","noul":0.02}},"usage":{"input_tokens":300}}`, done, done)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Switching it on asks the server first and saves config.json; a dead
// server is refused and the loop stays off; off is off.
func TestJudgeLoop_SetLive(t *testing.T) {
	dir := t.TempDir()
	// An existing setup: switching the judge must keep it.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"provider":"dgxt","providers":{"dgxt":{"kind":"vllm","endpoint":"http://gpu:8000","model":"glm-5.3-flash"}},"repeat_guard":12}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeSystemOne(t, "task", 0.9)
	j := newJudgeLoop(dir, nil)
	if _, _, err := j.Triage(context.Background(), "x"); !errors.Is(err, agent.ErrJudgeOff) {
		t.Fatalf("off: triage err = %v", err)
	}
	if _, err := j.Use("clef", "http://127.0.0.1:1", ""); err == nil || j.MaxRounds() != judgeDefaultRounds {
		t.Fatalf("a dead server was accepted (err %v)", err)
	}
	if on, _ := j.On(); on {
		t.Fatal("a refused switch turned it on")
	}
	msg, err := j.Use("clef", srv.URL, "")
	if err != nil || !strings.Contains(msg, "judge: on — clef-flash (clef) at "+srv.URL) || !strings.Contains(msg, "answered in") {
		t.Fatalf("on: %q, %v", msg, err)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil || lc.Judge == nil || !lc.Judge.Enabled || lc.Judge.Endpoint != srv.URL {
		t.Fatalf("saved %+v (err %v)", lc, err)
	}
	if lc.Provider != "dgxt" || lc.Providers["dgxt"].Model != "glm-5.3-flash" || lc.RepeatGuard != 12 {
		t.Fatalf("switching the judge lost the rest of config.json: %+v", lc)
	}
	task, use, err := j.Triage(context.Background(), "clean up Downloads")
	if err != nil || !task || use.InputTokens != 40 || use.Model != "clef-flash" {
		t.Fatalf("triage %v %+v %v", task, use, err)
	}
	v, err := j.Check(context.Background(), agent.Exchange{Request: "r", Answer: "a"})
	if err != nil || !v.Done || v.Use.InputTokens != 300 {
		t.Fatalf("check %+v %v", v, err)
	}
	if _, err := j.Set(false); err != nil || !strings.HasPrefix(j.Status(), "judge: off") {
		t.Fatalf("off: %q %v", j.Status(), err)
	}
	if st := j.Settings(); st.on || st.endpoint != srv.URL {
		t.Fatalf("off kept the endpoint for next time? on=%v ep=%q", st.on, st.endpoint)
	}
}

// TEN-516: Setup asks the server with the typed key BEFORE anything is saved
// (a dead server or a refused key leaves credentials.json and config.json as
// they were); on success the key goes to credentials.json, never to
// config.json, and out as a bearer token; a blank key clears it; Use keeps the
// saved key; rounds are bounded; an unknown kind is refused by name.
func TestJudgeLoop_SetupProbesBeforeSaving(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"bad key"}`)
			return
		}
		fmt.Fprint(w, `{"answers":{"kind":{"type":"choice","choice":"chat"}}}`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	j := newJudgeLoop(dir, nil)
	if ep, has := j.Saved(); ep != "" || has {
		t.Fatalf("fresh Saved() = %q %v", ep, has)
	}
	if !strings.Contains(j.Status(), "not set up") {
		t.Fatalf("fresh status: %q", j.Status())
	}
	if _, err := j.Use("jev", srv.URL, ""); err == nil || !strings.Contains(err.Error(), "unknown judge kind") || !strings.Contains(err.Error(), "clef") {
		t.Fatalf("unknown kind: %v", err)
	}
	// A dead server: nothing saved, not the key, not the address.
	if _, err := j.Setup("clef", "http://127.0.0.1:1", "sek-dead", ""); err == nil {
		t.Fatal("a dead server was accepted")
	}
	if creds, _ := loadCredentials(dir); creds.Get(judgeCredID) != "" {
		t.Fatal("a failed setup saved the key")
	}
	if lc, _ := loadLaunchConfig(dir); lc.Judge != nil {
		t.Fatalf("a failed setup saved config: %+v", lc.Judge)
	}
	// A refused key: the same.
	if _, err := j.Setup("clef", srv.URL, "bad", ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("refused key: %v", err)
	}
	if creds, _ := loadCredentials(dir); creds.Get(judgeCredID) != "" {
		t.Fatal("a refused key was saved")
	}
	// Accepted: probed with the key, then saved; the key is in credentials.json only.
	msg, err := j.Setup("clef", srv.URL, "sek-9", "")
	if err != nil || !strings.Contains(msg, "judge: on — clef-flash (clef) at "+srv.URL) || !strings.Contains(msg, "a key is saved") || !strings.Contains(msg, "answered in") {
		t.Fatalf("setup: %q, %v", msg, err)
	}
	if auth[len(auth)-1] != "Bearer sek-9" {
		t.Fatalf("probe auth = %q", auth)
	}
	creds, _ := loadCredentials(dir)
	if creds.Get(judgeCredID) != "sek-9" {
		t.Fatalf("key not in credentials.json: %+v", creds.Secrets)
	}
	raw, _ := os.ReadFile(launchConfigPath(dir))
	if strings.Contains(string(raw), "sek-9") {
		t.Fatal("the key reached config.json")
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil || lc.Judge == nil || !lc.Judge.Enabled || lc.Judge.Kind != "clef" || lc.Judge.Endpoint != srv.URL || lc.Judge.Model != "clef-flash" {
		t.Fatalf("saved %+v (err %v)", lc.Judge, err)
	}
	if ep, has := j.Saved(); ep != srv.URL || !has {
		t.Fatalf("Saved() = %q %v", ep, has)
	}
	// Use keeps the saved key; Setup with a blank key clears it after the probe.
	if _, err := j.Use("", "", ""); err != nil || auth[len(auth)-1] != "Bearer sek-9" {
		t.Fatalf("use with the saved key: %v %q", err, auth[len(auth)-1])
	}
	if _, err := j.Setup("clef", "", "", "clef-mini"); err != nil {
		t.Fatal(err)
	}
	if auth[len(auth)-1] != "" {
		t.Fatalf("after clearing, probe auth = %q", auth[len(auth)-1])
	}
	if creds, _ := loadCredentials(dir); creds.Get(judgeCredID) != "" {
		t.Fatal("key still in credentials.json after clearing")
	}
	if st := j.Status(); strings.Contains(st, "a key is saved") || !strings.Contains(st, "clef-mini") {
		t.Fatalf("status after clearing: %q", st)
	}
	for _, bad := range []int{0, 6} {
		if _, err := j.Rounds(bad); err == nil || !strings.Contains(err.Error(), "between 1 and 5") {
			t.Fatalf("rounds %d: %v", bad, err)
		}
	}
	if msg, err := j.Rounds(2); err != nil || !strings.Contains(msg, "up to 2 rounds") || j.MaxRounds() != 2 {
		t.Fatalf("rounds 2: %q %v %d", msg, err, j.MaxRounds())
	}
	if lc, _ := loadLaunchConfig(dir); lc.Judge.Rounds != 2 {
		t.Fatalf("rounds not saved: %+v", lc.Judge)
	}
	if _, err := j.Use("clef", "not a url", ""); err == nil || !strings.Contains(err.Error(), "address") {
		t.Fatalf("bad endpoint: %v", err)
	}
	// A config.json that can't be saved: the key goes back and nothing changed.
	cfgPath := launchConfigPath(dir)
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfgPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Setup("clef", srv.URL, "sek-late", ""); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("unsavable config: %v", err)
	}
	if creds, _ := loadCredentials(dir); creds.Get(judgeCredID) != "" {
		t.Fatalf("key kept after a failed save: %q", creds.Get(judgeCredID))
	}
	if st := j.Status(); strings.Contains(st, "a key is saved") || !strings.Contains(st, "clef-mini") {
		t.Fatalf("live state not restored: %q", st)
	}
}

// TEN-516: the dashboard card saves only what the server accepted: a key
// typed with the loop off is checked too, rounds ride in the same save as the
// server, and a refusal leaves key, config and rounds as they were.
func TestJudgeLoop_DashboardCardSavesOnlyAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"bad key"}`)
			return
		}
		fmt.Fprint(w, `{"answers":{"kind":{"type":"choice","choice":"chat"}}}`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	j := newJudgeLoop(dir, nil)
	d := dashModel{mc: &modelControl{cfgDir: dir, judge: j}}
	if _, err := d.SetJudge(false, "clef", "http://127.0.0.1:1", "sek-x", "", 0); err == nil {
		t.Fatal("off + key against a dead server was accepted")
	}
	if creds, _ := loadCredentials(dir); creds.Get(judgeCredID) != "" {
		t.Fatal("off + key saved a key no server accepted")
	}
	if _, err := d.SetJudge(true, "clef", srv.URL, "bad", "", 2); err == nil {
		t.Fatal("a refused key was accepted")
	}
	if lc, _ := loadLaunchConfig(dir); lc.Judge != nil {
		t.Fatalf("a refused setup saved %+v", lc.Judge)
	}
	if j.MaxRounds() != judgeDefaultRounds {
		t.Fatalf("rounds changed to %d on a refused setup", j.MaxRounds())
	}
	if _, err := d.SetJudge(true, "clef", srv.URL, "", "", 9); err == nil || !strings.Contains(err.Error(), "between 1 and 5") {
		t.Fatalf("rounds 9: %v", err)
	}
	msg, err := d.SetJudge(false, "clef", srv.URL, "sek-1", "", 2)
	if err != nil || !strings.Contains(msg, "judge: off") {
		t.Fatalf("off + good key: %q %v", msg, err)
	}
	if creds, _ := loadCredentials(dir); creds.Get(judgeCredID) != "sek-1" {
		t.Fatal("the accepted key was not saved")
	}
	lc, _ := loadLaunchConfig(dir)
	if lc.Judge == nil || lc.Judge.Enabled || lc.Judge.Endpoint != srv.URL || lc.Judge.Rounds != 2 {
		t.Fatalf("saved %+v", lc.Judge)
	}
	if msg, err := d.SetJudge(true, "", "", "", "", 0); err != nil || !strings.Contains(msg, "judge: on") || !strings.Contains(msg, "a key is saved") {
		t.Fatalf("on with the saved key: %q %v", msg, err)
	}
	if msg, err := d.SetJudge(false, "", "", "", "", 0); err != nil || !strings.Contains(msg, "judge: off") {
		t.Fatalf("off: %q %v", msg, err)
	}
	if lc, _ := loadLaunchConfig(dir); lc.Judge == nil || lc.Judge.Enabled || lc.Judge.Rounds != 2 {
		t.Fatalf("after off: %+v", lc.Judge)
	}
}

// A kind this build doesn't know (a hand-edited config.json, or a newer
// file) leaves the judge off, and Status names the valid kinds instead of
// running with an empty model.
func TestJudgeLoop_UnknownKindStaysOff(t *testing.T) {
	j := newJudgeLoop(t.TempDir(), &config.Judge{Enabled: true, Kind: "jev", Endpoint: "http://h:30000"})
	if on, _ := j.On(); on {
		t.Fatal("an unknown kind must not switch the judge on")
	}
	if st := j.Status(); !strings.Contains(st, `unknown kind "jev"`) || !strings.Contains(st, "known: clef") {
		t.Fatalf("status: %q", st)
	}
	if _, _, err := j.Triage(context.Background(), "x"); err == nil {
		t.Fatal("triage should report the loop off")
	}
}
