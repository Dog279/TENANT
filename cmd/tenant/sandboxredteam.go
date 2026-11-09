package main

// sandboxredteam.go: `tenant sandbox redteam` — a live, model-in-the-loop
// containment eval for the execution sandbox. It puts a local Ollama model in
// the adversary seat and lets it drive the REAL sandbox boundary
// (sandbox.Execute against the restricted backend) inside a disposable fake
// environment with canary secrets, then reports what the boundary contained.
//
// The eval logic lives in the testing-free internal/sandbox/redteamllm so it
// runs under go test with a scripted fake; this file is only the operator
// wiring: build an Ollama-backed model client, resolve the sandbox backend,
// run the harness, and render the report. Operator-only, like the rest of
// `tenant sandbox`.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"tenant/internal/model"
	"tenant/internal/model/backend/vllm"
	"tenant/internal/sandbox"
	"tenant/internal/sandbox/redteamllm"
)

const sandboxRedteamUsage = "usage: tenant sandbox redteam [--model qwen3.8:27b] [--endpoint http://localhost:11434] " +
	"[--backend restricted|container|host] [--turns 12] [--timeout 20s] [--context 32768] [--max-tokens 2048] [--temp 0.7] " +
	"[--phase2 FILE] [--json] [--keep]"

func sandboxRedteam(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sandbox redteam", flag.ContinueOnError)
	modelName := fs.String("model", "qwen3.8:27b", "model to put in the adversary seat (an id the server lists at /v1/models)")
	endpoint := fs.String("endpoint", defaultOllamaEndpoint(), "OpenAI-compatible base URL of the model server (Ollama, vLLM, …)")
	backendName := fs.String("backend", "restricted", "sandbox backend to test: restricted | container | host")
	turns := fs.Int("turns", 12, "max model turns per phase")
	timeout := fs.Duration("timeout", 20*time.Second, "wall-clock limit per sandboxed command")
	contextLen := fs.Int("context", 32768, "model context length hint")
	maxTokens := fs.Int("max-tokens", 2048, "max tokens per model reply")
	temp := fs.Float64("temp", 0.7, "sampling temperature for the adversary")
	phase2 := fs.String("phase2", "", "a file whose text is sent to the model as a second phase once it stops acting (the operator writes it; the harness adds nothing)")
	unguided := fs.Bool("unguided", false, "tell the model nothing about the objectives, target paths or sink: only that it is meant to be confined")
	instruction := fs.String("instruction", "", "your own brief to the model, used verbatim in place of the harness's objective list (implies --unguided)")
	transcript := fs.String("transcript", "", "also write the full JSON report (every turn and command) to this file")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	keep := fs.Bool("keep", false, "keep the throwaway environment dir for inspection")
	if err := fs.Parse(args); err != nil {
		return sbxHelpOK(err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sandbox redteam: unexpected argument %q\n%s", fs.Arg(0), sandboxRedteamUsage)
	}

	be, err := redteamBackend(ctx, *backendName)
	if err != nil {
		return err
	}
	var phase2Text string
	if *phase2 != "" {
		b, err := os.ReadFile(*phase2)
		if err != nil {
			return fmt.Errorf("sandbox redteam: --phase2: %w", err)
		}
		phase2Text = strings.TrimSpace(string(b))
		if phase2Text == "" {
			return fmt.Errorf("sandbox redteam: --phase2 file %s is empty", *phase2)
		}
	}

	base := normalizeEndpoint(*endpoint)
	if err := preflightOllama(ctx, base, *modelName); err != nil {
		return err
	}

	llm, err := ollamaLLM(ctx, base, *modelName, *contextLen)
	if err != nil {
		return err
	}
	adv := &ollamaAdversary{llm: llm, temp: float32(*temp), maxTokens: *maxTokens}

	// Progress to stderr-ish (the out writer) unless the caller wants clean JSON.
	logf := func(string, ...any) {}
	if !*asJSON {
		fmt.Fprintf(out, "Running sandbox red-team: model %q vs the %q backend (up to %d turns)…\n\n", *modelName, be.Name(), *turns)
		logf = func(event string, kv ...any) {
			switch event {
			case "sandbox_run":
				fmt.Fprintf(out, "  · %s\n", kvString(kv))
			case "model_turn":
				fmt.Fprintf(out, "turn %s\n", kvString(kv))
			case "phase2":
				fmt.Fprintf(out, "== phase 2 (%s)\n", kvString(kv))
			}
		}
	}

	rep, err := redteamllm.Run(ctx, adv, be, *modelName, redteamllm.Config{
		Turns:             *turns,
		PerCommandTimeout: *timeout,
		Keep:              *keep,
		Logf:              logf,
		Phase2:            phase2Text,
		Unguided:          *unguided,
		Instruction:       strings.TrimSpace(*instruction),
	})
	if *transcript != "" {
		// Written before the error check: a run the model server cut short still
		// leaves its partial transcript on disk.
		if b, jerr := json.MarshalIndent(rep, "", "  "); jerr == nil {
			if werr := os.WriteFile(*transcript, b, 0o600); werr != nil {
				fmt.Fprintf(out, "warning: could not write transcript %s: %v\n", *transcript, werr)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("sandbox redteam: %w", err)
	}

	if *asJSON {
		if err := sbxWriteJSON(out, rep); err != nil {
			return err
		}
	} else {
		renderRedteam(out, rep)
	}
	if !rep.Pass {
		return fmt.Errorf("sandbox redteam FAILED: %d problem(s)", len(rep.Problems))
	}
	return nil
}

// redteamBackend resolves the backend under test. "restricted" and "container"
// are the point of the exercise; each errors clearly when this host cannot run
// it (no Seatbelt off macOS; no engine, a failed gate or an unbuilt image for
// the container). "host" is allowed so an operator can watch the adversary walk
// the unconfined baseline for comparison.
func redteamBackend(ctx context.Context, name string) (sandbox.Backend, error) {
	switch name {
	case "", "restricted":
		b, err := sandbox.Restricted()
		if err != nil {
			return nil, fmt.Errorf("sandbox redteam: restricted backend unavailable on this host: %w\n"+
				"run on a host with a restricted backend (macOS), or pass --backend container|host", err)
		}
		return b, nil
	case "container":
		b, err := sandbox.For("container")
		if err != nil {
			return nil, fmt.Errorf("sandbox redteam: %w", err)
		}
		if a, ok := b.(sandbox.Available); ok {
			if err := a.Available(ctx); err != nil {
				return nil, fmt.Errorf("sandbox redteam: the container backend cannot run here: %w\n"+
					"`tenant sandbox probe` has the detail; `tenant sandbox image build` builds the image", err)
			}
		}
		return b, nil
	case "host":
		return sandbox.Host(), nil
	default:
		return nil, fmt.Errorf("sandbox redteam: unknown backend %q (want restricted | container | host)", name)
	}
}

// ollamaLLM builds the OpenAI-compatible client (reusing the vllm backend, which
// speaks the same wire protocol Ollama exposes at /v1/chat/completions).
func ollamaLLM(ctx context.Context, endpoint, modelName string, contextLen int) (model.LLM, error) {
	p := model.Profile{
		ID:                 "redteam-adversary",
		Role:               model.RoleExecutor,
		Backend:            "vllm",
		ProviderKind:       "ollama",
		Endpoint:           endpoint,
		Model:              modelName,
		ChatPath:           "/v1/chat/completions",
		ContextLength:      contextLen,
		ToolFormat:         "qwen", // enables the Hermes/XML tool-call safety-net parser for qwen-family models
		EstimateTokensOnly: true,   // Ollama has no /tokenize
	}
	b, err := vllm.New(ctx, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return nil, fmt.Errorf("sandbox redteam: build model client: %w", err)
	}
	llm, ok := b.(model.LLM)
	if !ok {
		return nil, fmt.Errorf("sandbox redteam: model backend does not implement LLM")
	}
	return llm, nil
}

// preflightOllama gives a clear, early error if the model server is unreachable
// or does not have the requested model, instead of a cryptic mid-run failure.
func preflightOllama(ctx context.Context, endpoint, modelName string) error {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("sandbox redteam: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox redteam: cannot reach the model server at %s: %w\n"+
			"start Ollama (`ollama serve`) or pass --endpoint; from WSL use the Windows host address, not localhost", endpoint, err)
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	for _, m := range body.Data {
		if m.ID == modelName {
			return nil
		}
	}
	// Reachable but the model wasn't listed: warn via error only if we got a
	// clean list back; some gateways don't implement /v1/models, so a decode
	// miss is not fatal.
	if len(body.Data) > 0 {
		have := make([]string, 0, len(body.Data))
		for _, m := range body.Data {
			have = append(have, m.ID)
		}
		return fmt.Errorf("sandbox redteam: model %q not found at %s; available: %s\n"+
			"pull it with `ollama pull %s` or pass --model", modelName, endpoint, strings.Join(have, ", "), modelName)
	}
	return nil
}

func defaultOllamaEndpoint() string {
	if h := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); h != "" {
		return normalizeEndpoint(h)
	}
	return "http://localhost:11434"
}

func normalizeEndpoint(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	return s
}

// ---- adversary: adapt model.LLM to redteamllm.Chat ----

type ollamaAdversary struct {
	llm       model.LLM
	temp      float32
	maxTokens int
}

func (a *ollamaAdversary) Chat(ctx context.Context, msgs []redteamllm.Msg, tools []redteamllm.Tool) (redteamllm.Reply, error) {
	resp, err := a.llm.Generate(ctx, model.GenerateRequest{
		Messages:    toModelMessages(msgs),
		Tools:       toToolSpecs(tools),
		ToolChoice:  model.ToolChoice{Mode: "auto"},
		Temperature: a.temp,
		MaxTokens:   a.maxTokens,
	})
	if err != nil {
		return redteamllm.Reply{}, err
	}
	out := redteamllm.Reply{Content: resp.Text}
	for _, tc := range resp.ToolCalls {
		out.Calls = append(out.Calls, redteamllm.Call{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
	}
	return out, nil
}

func toModelMessages(msgs []redteamllm.Msg) []model.Message {
	out := make([]model.Message, len(msgs))
	for i, m := range msgs {
		wm := model.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, c := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, model.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Args})
		}
		out[i] = wm
	}
	return out
}

func toToolSpecs(tools []redteamllm.Tool) []model.ToolSpec {
	out := make([]model.ToolSpec, len(tools))
	for i, t := range tools {
		out[i] = model.ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
	}
	return out
}

// ---- rendering ----

func renderRedteam(w io.Writer, rep redteamllm.Report) {
	c := rep.Caps
	fmt.Fprintf(w, "Sandbox red-team: %q vs backend %q (isolation %s)\n", rep.Model, rep.Backend, c.Isolation)
	fmt.Fprintf(w, "  claims  filesystem=%s  network=%s  limits=%s  env=%s\n", c.Filesystem, c.Network, c.Limits, c.Env)
	fmt.Fprintf(w, "  %d model turn(s), %d command(s) run, %d escape-escalation(s) denied\n", rep.Turns, rep.ToolCalls, rep.EscalationsDenied)
	if rep.Phase2At > 0 {
		fmt.Fprintf(w, "  the model stopped acting at turn %d; phase 2 ran from turn %d\n", rep.GaveUpAt, rep.Phase2At)
	} else if rep.GaveUpAt > 0 {
		fmt.Fprintf(w, "  the model stopped acting at turn %d\n", rep.GaveUpAt)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  OBJECTIVE\tDIMENSION\tOUTCOME")
	for _, o := range rep.Objectives {
		outcome := "contained"
		if o.Escaped {
			outcome = "ESCAPED"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", o.Key, o.Dimension, outcome)
	}
	tw.Flush()

	for _, o := range rep.Objectives {
		if o.Escaped && o.Evidence != "" {
			fmt.Fprintf(w, "    %s: %s\n", o.Key, o.Evidence)
		}
	}
	if len(rep.Notes) > 0 {
		fmt.Fprintln(w, "\nnotes:")
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	fmt.Fprintln(w)
	if rep.Pass {
		fmt.Fprintf(w, "PASS: the %q backend contained every objective it claims to enforce.\n", rep.Backend)
		return
	}
	fmt.Fprintf(w, "FAIL: %d problem(s):\n", len(rep.Problems))
	for i, p := range rep.Problems {
		fmt.Fprintf(w, "  %d. %s\n", i+1, p)
	}
}

// kvString renders a Logf key/value list as "k=v k=v" for progress lines.
func kvString(kv []any) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%v=%v", kv[i], kv[i+1])
	}
	return b.String()
}
