package shadow

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// The stores the Q5 measurement was taken on: 683 facts and 286 episodes,
// 8.6 MB on disk. synthDim and the text lengths below are chosen so that
// synthetic stores of those counts serialize to about that size.
const (
	measuredFacts    = 683
	measuredEpisodes = 286
	synthDim         = 1536
)

var synthWords = strings.Fields(`the service restarted after a config change on the host and nginx
failed to bind port 443 because another process held it so the operator stopped the old unit checked
journalctl output for errors found a stale pid file removed it then reloaded systemd and verified the
listener with ss disk usage on var was at ninety percent logs rotated weekly backup job runs nightly
at two to the nas over rsync kernel updated reboot pending firewall allows ssh from the lan only
docker compose stack for grafana prometheus loki postgres replica lag certificate renewal dns record`)

func synthText(r *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(synthWords[r.IntN(len(synthWords))])
		b.WriteByte(' ')
	}
	return b.String()
}

func synthVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	return v
}

// fillSynthetic writes facts and episodes shaped like a machine's real memory.
func fillSynthetic(t testing.TB, ep *episodic.Store, sem *semantic.Store, facts, episodes int) {
	t.Helper()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(1, 2))
	base := time.Unix(1_700_000_000, 0)
	for i := range episodes {
		_, err := ep.Insert(ctx, &episodic.Episode{
			AgentID: "main", SessionID: fmt.Sprintf("s%d", i/8), SourceRunID: fmt.Sprintf("run-%d", i),
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Prompt:    synthText(r, 300), Response: synthText(r, 3200),
			ToolCalls: []episodic.ToolCallRef{{ID: "c1", Name: "fleet_exec", Arguments: `{"host":"web1","command":"` + synthText(r, 400) + `"}`}},
			Outcome:   episodic.OutcomeSuccess, Tags: []string{"host:web1"},
			EmbedderID: "embedder", Embedding: synthVec(r, synthDim),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range facts {
		id, err := sem.Insert(ctx, &semantic.Fact{
			AgentID: "main", Fact: synthText(r, 140), SourceEpisodes: []int64{int64(i%episodes + 1)},
			FirstSeen: base, LastConfirmed: base,
			EmbedderID: "embedder", Embedding: synthVec(r, synthDim), OriginJob: "distill",
		})
		if err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := sem.BumpAccess(ctx, []int64{id}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
