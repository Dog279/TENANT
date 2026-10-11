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
	var tools []agent.ToolCallResult
	for i := 0; i < judgeMaxCalls+5; i++ {
		tools = append(tools, agent.ToolCallResult{Call: model.ToolCall{Name: "os_exec", Arguments: json.RawMessage(fmt.Sprintf(`{"command":"step %d"}`, i))}, Result: "out"})
	}
	st := judgeState(agent.Exchange{Request: "do it", Tools: tools, Answer: strings.Repeat("é", judgeTextChars)})
	b, _ := json.Marshal(st)
	var got struct {
		Calls   []map[string]any `json:"tool_calls"`
		Skipped int              `json:"earlier_tool_calls_not_listed"`
		Answer  string           `json:"final_answer"`
	}
	_ = json.Unmarshal(b, &got)
	withOutput := 0
	for _, c := range got.Calls {
		if c["output"] != nil {
			withOutput++
		}
	}
	if len(got.Calls) != judgeMaxCalls || got.Skipped != 5 || withOutput != judgeKeepOutputs {
		t.Fatalf("calls %d, skipped %d, with output %d", len(got.Calls), got.Skipped, withOutput)
	}
	if !strings.HasSuffix(got.Answer, "…") || strings.ContainsRune(got.Answer, '�') {
		t.Fatalf("the answer wasn't cut on a rune boundary: …%q", got.Answer[len(got.Answer)-8:])
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
	if _, err := j.Set(true, "http://127.0.0.1:1", ""); err == nil || j.MaxRounds() != judgeDefaultRounds {
		t.Fatalf("a dead server was accepted (err %v)", err)
	}
	if on, _ := j.On(); on {
		t.Fatal("a refused switch turned it on")
	}
	msg, err := j.Set(true, srv.URL, "")
	if err != nil || !strings.Contains(msg, "judge loop: on — clef-flash at "+srv.URL) || !strings.Contains(msg, "answered in") {
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
	if _, err := j.Set(false, "", ""); err != nil || !strings.HasPrefix(j.Status(), "judge loop: off") {
		t.Fatalf("off: %q %v", j.Status(), err)
	}
	if on, ep, _ := j.Settings(); on || ep != srv.URL {
		t.Fatalf("off kept the endpoint for next time? on=%v ep=%q", on, ep)
	}
}
