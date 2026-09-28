package event

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// An event survives its wire form exactly: typed data, sensitive flags,
// channel, level and every correlation and process field.
func TestEventJSONRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 3, 11, 123456789, time.UTC)
	in := Event{Seq: 42, Log: "discord", Provider: "discord", ID: 2001, Version: 1, Time: at,
		Level: Warning, Channel: Diagnostic, Task: "Connection", Opcode: OpFail, Keywords: []string{"network"},
		Msg: "Relay disconnected",
		Data: []Field{{Key: "reason", Value: "reset"}, {Key: "n", Value: int64(3)}, {Key: "ratio", Value: 0.5},
			{Key: "ok", Value: false}, {Key: "none", Value: nil}, {Key: "raw", Value: json.RawMessage(`{"a":[1]}`)},
			{Key: "at", Value: at}, {Key: "took", Value: 1500 * time.Millisecond}, {Key: "body", Value: "secret text", Sensitive: true}},
		Corr:  Correlation{TraceID: "t1", ActivityID: "a1", RelatedID: "r1", SessionID: "s1", ApprovalID: "ap-1", ToolCallID: "tc1"},
		Actor: "operator", Surface: "discord",
		Proc: Process{PID: 7, Role: "serve", BootID: "b1", Host: "h", Version: "0.0.1"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the event:\n in  %+v\n out %+v\n json %s", in, out, b)
	}
}

// A writer outside Tenant sends plain JSON: no dtypes, a level by name.
// Types come from the values, nothing is sensitive, an unknown level is info.
func TestEventJSONFromAnOutsideWriter(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(`{"log":"ext.deploy","msg":"deployed","level":"loud",
		"data":{"svc":"api","n":2,"ms":1.5,"ok":true,"tags":["a"]}}`), &e); err != nil {
		t.Fatal(err)
	}
	want := []Field{{Key: "svc", Value: "api"}, {Key: "n", Value: int64(2)}, {Key: "ms", Value: 1.5},
		{Key: "ok", Value: true}, {Key: "tags", Value: json.RawMessage(`["a"]`)}}
	if e.Level != Info || e.Log != "ext.deploy" || !reflect.DeepEqual(e.Data, want) {
		t.Fatalf("got %+v", e)
	}
}
