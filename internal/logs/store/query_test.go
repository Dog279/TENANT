package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
)

const (
	bootA = "b0000000000000000000000000000000" // the store's own process
	bootB = "b1000000000000000000000000000000"
	trace = "7000000000000000000000000000000a"
	actA  = "a1000000000000000000000000000000" // a job started in the turn
	actB  = "a2000000000000000000000000000000" // a sandbox run started by the job
)

// fixture commits one event of each shape, named by index in Msg's first
// word (e0 …), and returns the store, the events and the time they were
// written against.
func fixture(t *testing.T) (*Store, []event.Event, time.Time) {
	t.Helper()
	now := time.Now()
	at := func(ago time.Duration) time.Time { return now.Add(-ago).UTC() }
	f := func(k string, v any) event.Field { return event.Field{Key: k, Value: v} }
	pa, pb := event.Process{PID: 4242, BootID: bootA}, event.Process{PID: 99, BootID: bootB}
	evs := []event.Event{
		{Log: "discord", Provider: "discord", ID: 2001, Level: event.Warning, Task: "Connection", Opcode: "fail", Msg: "e0 relay disconnected: gateway reset", Time: at(50 * time.Minute), Proc: pa},
		{Log: "discord", Provider: "discord", ID: 2005, Level: event.Warning, Msg: "e1 rate limited, retrying", Time: at(40 * time.Minute), Proc: pa},
		{Log: "discord", Provider: "discord", ID: 3004, Level: event.Error, Msg: "e2 message to #ops failed", Time: at(30 * time.Minute), Proc: pa},
		{Log: "mcp.github", Provider: "mcp.github", ID: 3001, Level: event.Error, Msg: "e3 permission denied for repo", Time: at(20 * time.Minute), Proc: pa},
		{Log: "mcp.jira", Provider: "mcp.jira", ID: 1001, Level: event.Info, Msg: "e4 tool call done", Time: at(19 * time.Minute), Proc: pa},
		{Log: "turns", Provider: "tenant.agent", ID: 1001, Level: event.Info, Msg: "e5 tool call os_exec returned: permission denied", Time: at(18 * time.Minute), Proc: pa,
			Data: []event.Field{f("tool", "os_exec"), f("iter", int64(3)), f("ok", true), f("req.method", "GET"), f("args", json.RawMessage(`{"q":"hn","n":10}`)), f("html", `<a&b> "q"`),
				{Key: "secret", Value: "hunter2", Sensitive: true}, {Key: "pin", Value: int64(1234), Sensitive: true},
				{Key: "blob", Value: json.RawMessage(`{"k":"v"}`), Sensitive: true}, f("after", "ok")},
			Corr: event.Correlation{TraceID: trace, SessionID: "discord:general", ToolCallID: "tc1"}, Actor: "dylan", Surface: "discord"},
		{Log: "turns", Provider: "tenant.agent", ID: 9001, Level: event.Verbose, Channel: event.Diagnostic, Msg: "e6 payload", Time: at(17 * time.Minute), Proc: pa,
			Data: []event.Field{f("tool", "web_search"), f("iter", 7.5)}, Corr: event.Correlation{TraceID: trace}},
		{Log: "security", Provider: "security", ID: 4103, Level: event.Warning, Keywords: []string{"audit.failure"}, Msg: "e7 approval a1 denied: exec", Time: at(16 * time.Minute), Proc: pa,
			Corr: event.Correlation{TraceID: trace, ApprovalID: "a1"}, Actor: "dylan", Surface: "dashboard"},
		{Log: "system", Provider: "system", ID: 3, Level: event.Critical, Msg: "e8 previous run ended unexpectedly", Time: at(25 * time.Hour), Proc: pb},
		{Log: "cron", Provider: "cron", ID: 1000, Level: event.Info, Opcode: "start", Msg: "e9 job nightly started", Time: at(10 * time.Minute), Proc: pa,
			Corr: event.Correlation{ActivityID: actA, RelatedID: trace}},
		{Log: "sandbox", Provider: "sandbox", ID: 1000, Level: event.Info, Opcode: "start", Msg: "e10 sandbox run", Time: at(9 * time.Minute), Proc: pa,
			Corr: event.Correlation{ActivityID: actB, RelatedID: actA}},
		{Log: "sandbox", Provider: "sandbox", ID: 1001, Level: event.Info, Msg: "e11 child output line", Time: at(8 * time.Minute), Proc: pa,
			Corr: event.Correlation{ActivityID: actB}},
		{Log: "eval", Provider: "eval", ID: 1, Level: event.Info, Msg: `e12 eval "quoted" run`, Time: at(48 * time.Hour), Proc: pa},
	}
	s := open(t, tempDB(t), bootA)
	if err := s.Commit(ctx, evs); err != nil {
		t.Fatal(err)
	}
	return s, evs, now
}

// indexes names the fixture events in evs by their Msg's first word.
func indexes(evs []event.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		w, _, _ := strings.Cut(e.Msg, " ")
		out = append(out, w)
	}
	slices.Sort(out)
	return out
}

func names(s string) []string {
	out := strings.Fields(s)
	slices.Sort(out)
	return out
}

// Every field, operator and value form finds exactly its events, through
// Query and Count.
func TestQueryFilters(t *testing.T) {
	s, _, now := fixture(t)
	all := "e0 e1 e2 e3 e4 e5 e6 e7 e8 e9 e10 e11 e12"
	for _, c := range []struct{ filter, want string }{
		{"", all},
		{"log:discord", "e0 e1 e2"},
		{"log:discord id:2001-2999 -id:2005", "e0"},
		{"log:discord,cron", "e0 e1 e2 e9"},
		{"-log:discord", "e3 e4 e5 e6 e7 e8 e9 e10 e11 e12"},
		{"provider:mcp.*", "e3 e4"},
		{"provider:mcp.* level>=error", "e3"},
		{"provider:discord id:3004", "e2"},
		{"id:1,3,5-1000", "e8 e9 e10 e12"},
		{"id>=4000", "e6 e7"},
		{"id<3", "e12"},
		{"id!=1000,1001", "e0 e1 e2 e3 e6 e7 e8 e12"},
		{"id:1,1000 level>=warning", ""}, // a list stays one term
		{"id:3,5-1000 level:critical,info log:system,cron", "e8 e9"},
		{"actor:dylan,x* log:turns", "e5"},
		{"data.tool:os_exec,web_* log:security", ""},
		{`data.html:"<a&b> \"q\""`, "e5"},
		{`data.html:"<a&b> \"q"`, ""},
		// A sensitive field is masked: no filter reads it.
		{"data.secret:hunter2", ""},
		{"data.secret:hunter*", ""},
		{"data.pin>=1000", ""},
		{"data.blob.k:v", ""},
		{"-data.secret:hunter2 log:turns", "e5 e6"},
		{"data.after:ok", "e5"},
		{"level>=warning", "e0 e1 e2 e3 e7 e8"},
		{"level>warning", "e2 e3 e8"},
		{"level:warning", "e0 e1 e7"},
		{"level:error,critical", "e2 e3 e8"},
		{"level<=info", "e4 e5 e6 e9 e10 e11 e12"},
		{"level<info", "e6"},
		{"level!=info", "e0 e1 e2 e3 e6 e7 e8"},
		{"-level:info,verbose", "e0 e1 e2 e3 e7 e8"},
		{"channel:diagnostic", "e6"},
		{"channel!=diagnostic log:turns", "e5"},
		{"task:Connection", "e0"},
		{"opcode:start", "e9 e10"},
		{"-opcode:start log:cron,sandbox", "e11"},
		{"tag:audit.failure", "e7"},
		{"tag:audit.*", "e7"},
		{"-tag:audit.failure log:security", ""},
		{"trace:" + trace, "e5 e6 e7"},
		{"activity:" + actA, "e9"},
		{"related:" + actA, "e10"},
		{"session:discord:general", "e5"},
		{"approval:a1", "e7"},
		{"tool_call:tc1", "e5"},
		{"actor:dylan", "e5 e7"},
		{"-actor:dylan", "e0 e1 e2 e3 e4 e6 e8 e9 e10 e11 e12"},
		{"actor!=dylan", "e0 e1 e2 e3 e4 e6 e8 e9 e10 e11 e12"},
		{"surface:dash*", "e7"},
		{"pid:99", "e8"},
		{"pid>=100", "e0 e1 e2 e3 e4 e5 e6 e7 e9 e10 e11 e12"},
		{"boot:" + bootB, "e8"},
		{"boot:current level>=warning", "e0 e1 e2 e3 e7"},
		{"since:15m", "e9 e10 e11"},
		{"since:1h until:17m30s", "e0 e1 e2 e3 e4 e5"},
		{"-since:24h", "e8 e12"},
		{"data.tool:os_exec", "e5"},
		{"data.tool:os_*,web_search", "e5 e6"},
		{"-data.tool:os_exec log:turns", "e6"},
		{"data.iter>=3", "e5 e6"},
		{"data.iter>7", "e6"},
		{"data.iter:3", "e5"},
		{`data.iter:"3"`, ""},
		{"data.ok:true", "e5"},
		{"data.req.method:GET", "e5"},
		{"data.args.q:hn", "e5"},
		{"data.args.n>5", "e5"},
		{`data.tool:os_exec "permission denied"`, "e5"},
		{`"permission denied"`, "e3 e5"},
		{"permission denied", "e3 e5"},
		{`permission -"permission denied"`, ""},
		{`-permission log:mcp.github,turns`, "e6"},
		{"DENIED", "e3 e5 e7"},
		{`"eval \"quoted\" run"`, "e12"},
		// FTS5's syntax in user text is only text.
		{`"denied OR payload"`, ""},
		{`"denied" "NEAR(denied payload)"`, ""},
		{`"perm*"`, ""},
		{`"^tool"`, "e4 e5"},
		{`"..."`, ""},
		// SQL in user text is only text.
		{`log:"x') OR 1=1 --"`, ""},
		{`data.tool:"x' OR '1'='1"`, ""},
	} {
		f, err := query.ParseAt(c.filter, now)
		if err != nil {
			t.Errorf("%q: %v", c.filter, err)
			continue
		}
		evs, _, _, err := s.Query(ctx, f, Cursor{}, 0, false)
		if err != nil {
			t.Errorf("%q: %v", c.filter, err)
			continue
		}
		if got, want := indexes(evs), names(c.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %v, want %v", c.filter, got, want)
		}
		if n, err := s.Count(ctx, f); err != nil || n != int64(len(evs)) {
			t.Errorf("Count(%q) = %d, %v; Query found %d", c.filter, n, err, len(evs))
		}
	}
	// boot:current needs the store's boot ID.
	other := open(t, tempDB(t), "")
	if _, err := other.Count(ctx, query.Filter{Terms: []query.Term{{Field: "boot", Op: query.Match, Values: []query.Value{{Text: "current"}}}}}); err == nil {
		t.Error("boot:current on a store without a boot ID")
	}
}

// The planner writes level <= 3 literally for warning-and-above, and SQLite
// then reads the administrative partial index. User text is never in the SQL.
func TestPlanAdminIndex(t *testing.T) {
	s, _, now := fixture(t)
	for filter, want := range map[string]string{
		"level>=warning":      "e.level <= 3",
		"level:error":         "e.level <= 3 AND e.level IN (2)",
		"-level:info,verbose": "e.level <= 3",
		"level<=info":         "e.level IN (4, 5)",
		"level>critical":      "0",
		"level<=critical":     "1",
	} {
		f, _ := query.ParseAt(filter, now)
		where, args, err := s.where(f, false)
		if err != nil || where != want || len(args) != 0 {
			t.Errorf("%q planned as %q %v, %v; want %q", filter, where, args, err, want)
		}
	}
	f, _ := query.ParseAt(`level>=warning log:"x') OR 1=1 --" "a"" OR b"`, now)
	where, _, _ := s.where(f, false)
	if strings.Contains(where, "OR 1=1") || strings.Contains(where, "OR b") {
		t.Errorf("user text in the SQL: %s", where)
	}
	f, _ = query.ParseAt("level>=error since:30d", now)
	where, args, _ := s.where(f, false)
	var plan []string
	rows, err := s.r.Query(`EXPLAIN QUERY PLAN SELECT e.seq FROM events e WHERE `+where+` ORDER BY e.time_ns DESC`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "; "), "events_admin") {
		t.Errorf("level>=error since:30d does not read events_admin: %v", plan)
	}
}

// Paging through every page gives the same events, in order, whether a page
// is found by scanning windows, through the indexes, or both.
func TestPageWindows(t *testing.T) {
	s, _, now := fixture(t)
	defer func(w int64) { pageWindow = w }(pageWindow)
	for _, filter := range []string{"", "log:discord", "trace:" + trace, "level>=warning", "-level:info permission", "log:sandbox,eval", "data.tool:nothing"} {
		f, _ := query.ParseAt(filter, now)
		all, _, _, err := s.Query(ctx, f, Cursor{}, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		want := indexes(all)
		for _, w := range []int64{1, 2, 3, 20_000} {
			pageWindow = w
			for _, limit := range []int{1, 2, 3, 100} {
				for _, reverse := range []bool{false, true} {
					var got []event.Event
					var cur Cursor
					for range 20 {
						page, next, _, err := s.Query(ctx, f, cur, limit, reverse)
						if err != nil {
							t.Fatal(err)
						}
						if len(page) == 0 {
							break
						}
						got, cur = append(got, page...), next
					}
					if reverse {
						slices.Reverse(got)
					}
					if !slices.IsSortedFunc(got, func(a, b event.Event) int { return int(a.Seq) - int(b.Seq) }) || !reflect.DeepEqual(indexes(got), want) {
						t.Errorf("%q, window %d, limit %d, reverse %v: %v, want %v", filter, w, limit, reverse, indexes(got), want)
					}
				}
			}
		}
	}
}

// Cursor paging over seq, both ways, and a foreign cursor restarting.
func TestQueryPaging(t *testing.T) {
	s, evs, now := fixture(t)
	f, _ := query.ParseAt("-log:eval", now)
	var got []string
	var cur Cursor
	for range 10 {
		page, next, restarted, err := s.Query(ctx, f, cur, 5, false)
		if err != nil || restarted {
			t.Fatal(err, restarted)
		}
		if len(page) == 0 {
			if next != cur {
				t.Errorf("an empty page moved the cursor: %v to %v", cur, next)
			}
			break
		}
		got = append(got, indexes(page[:1])[0])
		if next.Seq != page[len(page)-1].Seq {
			t.Errorf("next = %v after seq %d", next, page[len(page)-1].Seq)
		}
		cur = next
	}
	if want := []string{"e0", "e5", "e10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("forward pages start %v, want %v", got, want)
	}
	page, next, _, err := s.Query(ctx, f, Cursor{}, 3, true)
	if err != nil || len(page) != 3 || page[0].Seq != evs[11].Seq || page[2].Seq != evs[9].Seq || next.Seq != evs[9].Seq {
		t.Fatalf("newest page = %v, next %v, %v", indexes(page), next, err)
	}
	page, _, _, _ = s.Query(ctx, f, next, 2, true)
	if len(page) != 2 || page[0].Seq != evs[8].Seq || page[1].Seq != evs[7].Seq {
		t.Errorf("second newest page = %v", indexes(page))
	}
	page, _, restarted, _ := s.Query(ctx, f, Cursor{StoreID: [16]byte{9}, Seq: evs[3].Seq}, 0, false)
	if !restarted || len(page) != 12 {
		t.Errorf("foreign cursor: %d events, restarted %v", len(page), restarted)
	}
}

// Stats come from the rollups for log, level and time filters, and agree
// with counting the events; any other field counts the events.
func TestStats(t *testing.T) {
	s, evs, now := fixture(t)
	for _, filter := range []string{"", "log:discord level>=warning", "-log:discord since:1h", "since:30h until:15m level:info,critical"} {
		f, _ := query.ParseAt(filter, now)
		if !rollupable(f) {
			t.Fatalf("%q is not rollupable", filter)
		}
		for _, bucket := range []time.Duration{0, time.Minute, time.Hour, 24 * time.Hour} {
			fromRollup, err := s.stats(ctx, f, bucket, true)
			if err != nil {
				t.Fatal(err)
			}
			fromEvents, err := s.stats(ctx, f, bucket, false)
			if err != nil {
				t.Fatal(err)
			}
			// Times inside a minute round out in the rollups; the fixture's
			// bounds don't split a minute that holds an event.
			if !reflect.DeepEqual(fromRollup, fromEvents) {
				t.Errorf("%q by %v: rollups %+v, events %+v", filter, bucket, fromRollup, fromEvents)
			}
		}
	}
	f, _ := query.ParseAt("log:discord", now)
	got, err := s.Stats(ctx, f, 0)
	want := []Bucket{{Log: "discord", Level: event.Error, N: 1}, {Log: "discord", Level: event.Warning, N: 2}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Stats(log:discord, 0) = %+v, %v; want %+v", got, err, want)
	}
	f, _ = query.ParseAt("provider:discord id:2001", now)
	got, err = s.Stats(ctx, f, time.Hour)
	start := evs[0].Time.Truncate(time.Hour)
	if err != nil || len(got) != 1 || !got[0].Start.Equal(start) || got[0].N != 1 {
		t.Errorf("Stats(provider:discord id:2001, 1h) = %+v, %v; want one at %v", got, err, start)
	}
	// Rolled up rows are really read: a count the events don't have shows.
	if _, err := s.w.Exec(`UPDATE rollup SET n = n + 100`); err != nil {
		t.Fatal(err)
	}
	f, _ = query.ParseAt("log:discord", now)
	if got, _ := s.Stats(ctx, f, 0); len(got) != 2 || got[0].N != 101 {
		t.Errorf("Stats(log:discord) after bumping the rollups = %+v", got)
	}
}

func TestTopIDs(t *testing.T) {
	s, _, now := fixture(t)
	more := []event.Event{
		{Log: "discord", Provider: "discord", ID: 2005, Level: event.Warning, Msg: "again"},
		{Log: "discord", Provider: "discord", ID: 2005, Level: event.Warning, Msg: "again"},
		{Log: "discord", Provider: "discord", ID: 3004, Level: event.Error, Msg: "again"},
	}
	if err := s.Commit(ctx, more); err != nil {
		t.Fatal(err)
	}
	f, _ := query.ParseAt("log:discord", now)
	got, err := s.TopIDs(ctx, f, 2)
	want := []IDCount{{"discord", 2005, 3}, {"discord", 3004, 2}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("TopIDs = %+v, %v; want %+v", got, err, want)
	}
}

// The pivot follows related_id down as far as it is asked to.
func TestTimeline(t *testing.T) {
	s, _, _ := fixture(t)
	for depth, want := range map[int]string{
		-1: "e5 e6 e7 e9",
		0:  "e5 e6 e7 e9",         // the turn, and what it started
		1:  "e5 e6 e7 e9 e10",     // the job's own events, and what it started
		2:  "e5 e6 e7 e9 e10 e11", // the sandbox run's output
		99: "e5 e6 e7 e9 e10 e11", // capped, and cycles end
	} {
		got, err := s.Timeline(ctx, trace, depth)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.IsSortedFunc(got, func(a, b event.Event) int { return int(a.Seq) - int(b.Seq) }) {
			t.Errorf("depth %d: not in seq order", depth)
		}
		if g := indexes(got); !reflect.DeepEqual(g, names(want)) {
			t.Errorf("Timeline(trace, %d) = %v, want %v", depth, g, want)
		}
	}
	if got, _ := s.Timeline(ctx, actB, 3); !reflect.DeepEqual(indexes(got), names("e10 e11")) {
		t.Errorf("Timeline(sandbox run) = %v", indexes(got))
	}
	if got, err := s.Timeline(ctx, "nope", 3); err != nil || len(got) != 0 {
		t.Errorf("Timeline(unknown) = %v, %v", got, err)
	}
}

// "Has this happened before?": count, first and last seen, and the trend.
func TestEventStats(t *testing.T) {
	s := open(t, tempDB(t), "")
	now := time.Now()
	var batch []event.Event
	for _, ago := range []time.Duration{time.Hour, 2 * time.Hour, 30 * time.Hour, 80 * time.Hour, 10 * 24 * time.Hour} {
		batch = append(batch, event.Event{Log: "system", Provider: "system", ID: 3101, Level: event.Error, Msg: "failed", Time: now.Add(-ago).UTC()})
	}
	batch = append(batch, event.Event{Log: "system", Provider: "system", ID: 3102, Level: event.Error, Msg: "other", Time: now.UTC()},
		event.Event{Log: "other", Provider: "other", ID: 3101, Level: event.Error, Msg: "other", Time: now.UTC()})
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	got, err := s.EventStats(ctx, "system", 3101)
	if err != nil {
		t.Fatal(err)
	}
	want := IDStats{Count: 5, First: batch[4].Time, Last: batch[0].Time, Trend: [7]int64{3: 1, 5: 1, 6: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EventStats = %+v, want %+v", got, want)
	}
	if got, err := s.EventStats(ctx, "nobody", 1); err != nil || got != (IDStats{}) {
		t.Errorf("EventStats(unknown) = %+v, %v", got, err)
	}
}
