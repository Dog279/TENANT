package route_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/route"
)

func TestParseSignalGolden(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want route.Signal
	}{
		{
			name: "small factual lower bound and yes-no flags",
			raw:  "Domain: General Knowledge | Complexity: 1 | Math: no | Code: no | Route: small model | Justification: A direct factual lookup.",
			want: route.Signal{Domain: "General Knowledge", Complexity: 1, Label: "small", Justification: "A direct factual lookup."},
		},
		{
			name: "big mathematics actual boolean casing",
			raw:  "Domain: Combinatorics/Geometry | Complexity: 5 | Math: True | Code: False | Route: big model | Justification: Multi-step proof and counting are required.",
			want: route.Signal{Domain: "Combinatorics/Geometry", Complexity: 5, Math: true, Label: "big", Justification: "Multi-step proof and counting are required."},
		},
		{
			name: "code yes",
			raw:  "Domain: Software Engineering | Complexity: 4 | Math: no | Code: yes | Route: big model | Justification: The task changes concurrent code.",
			want: route.Signal{Domain: "Software Engineering", Complexity: 4, Code: true, Label: "big", Justification: "The task changes concurrent code."},
		},
		{
			name: "both flags false with completion whitespace",
			raw:  "\nDomain: Content Creation | Complexity: 2 | Math: False | Code: False | Route: small model | Justification: Short prose is locally tractable.\n",
			want: route.Signal{Domain: "Content Creation", Complexity: 2, Label: "small", Justification: "Short prose is locally tractable."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := route.ParseSignal(tc.raw)
			if err != nil {
				t.Fatalf("ParseSignal: %v", err)
			}
			if got.Domain != tc.want.Domain || got.Complexity != tc.want.Complexity ||
				got.Math != tc.want.Math || got.Code != tc.want.Code ||
				got.Label != tc.want.Label || got.Justification != tc.want.Justification {
				t.Fatalf("signal = %+v, want %+v", got, tc.want)
			}
			if got.Raw != tc.raw {
				t.Fatalf("Raw = %q, want exact input", got.Raw)
			}
		})
	}
}

func TestParseSignalRejectsDeviations(t *testing.T) {
	t.Parallel()
	valid := "Domain: Coding | Complexity: 3 | Math: False | Code: True | Route: big model | Justification: Technical work."
	cases := map[string]string{
		"empty":               "",
		"missing field":       "Domain: Coding | Complexity: 3 | Math: False | Route: big model | Justification: Technical work.",
		"wrong order":         "Complexity: 3 | Domain: Coding | Math: False | Code: True | Route: big model | Justification: Technical work.",
		"wrong delimiter":     strings.Replace(valid, " | ", "|", 1),
		"extra field":         valid + " | Confidence: high",
		"complexity zero":     strings.Replace(valid, "Complexity: 3", "Complexity: 0", 1),
		"complexity six":      strings.Replace(valid, "Complexity: 3", "Complexity: 6", 1),
		"complexity padded":   strings.Replace(valid, "Complexity: 3", "Complexity: 03", 1),
		"math lowercase true": strings.Replace(valid, "Math: False", "Math: true", 1),
		"code uppercase yes":  strings.Replace(valid, "Code: True", "Code: YES", 1),
		"unknown flag":        strings.Replace(valid, "Math: False", "Math: maybe", 1),
		"bare route label":    strings.Replace(valid, "Route: big model", "Route: big", 1),
		"unknown route":       strings.Replace(valid, "Route: big model", "Route: medium model", 1),
		"empty domain":        strings.Replace(valid, "Domain: Coding", "Domain: ", 1),
		"padded domain":       strings.Replace(valid, "Domain: Coding", "Domain:  Coding", 1),
		"empty justification": strings.Replace(valid, "Justification: Technical work.", "Justification: ", 1),
		"internal newline":    strings.Replace(valid, " | Code", "\n | Code", 1),
		"trailing commentary": valid + "\nThis should use the large model.",
		"misspelled field":    strings.Replace(valid, "Complexity:", "Complex:", 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := route.ParseSignal(raw)
			if err == nil {
				t.Fatalf("ParseSignal(%q) unexpectedly succeeded: %+v", raw, got)
			}
			if !strings.HasPrefix(err.Error(), "route: parse:") {
				t.Fatalf("error = %q, want route: parse prefix", err)
			}
			if got.Raw != raw {
				t.Fatalf("Raw = %q, want malformed input preserved", got.Raw)
			}
		})
	}
}

func TestSupraClassifyUsesNativeOllamaRequest(t *testing.T) {
	t.Parallel()
	type request struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		Raw       bool   `json:"raw"`
		Stream    bool   `json:"stream"`
		KeepAlive string `json:"keep_alive"`
	}
	reqCh := make(chan request, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/generate" {
			t.Errorf("path = %q, want /api/generate", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		reqCh <- body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": "Domain: Programming | Complexity: 4 | Math: False | Code: True | Route: big model | Justification: Code generation requires technical logic.",
			"done":     true,
		})
	}))
	defer ts.Close()

	c := route.NewSupra(ts.URL+"/", "checkpoint", time.Second)
	sig, err := c.Classify(context.Background(), "write a mutex")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if sig.Label != "big" || !sig.Code || sig.Complexity != 4 {
		t.Fatalf("signal = %+v", sig)
	}
	body := <-reqCh
	if body.Model != "checkpoint" {
		t.Errorf("model = %q", body.Model)
	}
	if body.Prompt != "Task: write a mutex\nAnalysis: " {
		t.Errorf("prompt = %q", body.Prompt)
	}
	if !body.Raw || body.Stream {
		t.Errorf("raw/stream = %v/%v, want true/false", body.Raw, body.Stream)
	}
	if body.KeepAlive != "5m" {
		t.Errorf("keep_alive = %q, want 5m", body.KeepAlive)
	}
}

func TestSupraDefaultsModel(t *testing.T) {
	t.Parallel()
	modelCh := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		modelCh <- body.Model
		_, _ = w.Write([]byte(`{"response":"Domain: General | Complexity: 1 | Math: False | Code: False | Route: small model | Justification: Simple."}`))
	}))
	defer ts.Close()
	if _, err := route.NewSupra(ts.URL, "", 0).Classify(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := <-modelCh; got != route.DefaultModel {
		t.Fatalf("model = %q, want %q", got, route.DefaultModel)
	}
}

func TestSupraEndpointDown(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + ln.Addr().String()
	_ = ln.Close()
	sig, err := route.NewSupra(endpoint, "test", 100*time.Millisecond).Classify(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected endpoint error")
	}
	if !strings.HasPrefix(err.Error(), "route: supra:") {
		t.Fatalf("error = %q", err)
	}
	if sig.Latency <= 0 || sig.Raw != "" {
		t.Fatalf("signal on failure = %+v", sig)
	}
}

func TestSupraTimeout(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{"response": "too late"})
	}))
	defer ts.Close()
	sig, err := route.NewSupra(ts.URL, "test", 10*time.Millisecond).Classify(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
	if sig.Latency < 5*time.Millisecond {
		t.Fatalf("latency = %v, want timeout-scale duration", sig.Latency)
	}
}

func TestSupraMalformedOutputPreservesRaw(t *testing.T) {
	t.Parallel()
	const malformed = "Route: probably big"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"response": malformed})
	}))
	defer ts.Close()
	sig, err := route.NewSupra(ts.URL, "test", time.Second).Classify(context.Background(), "hi")
	if err == nil || !strings.HasPrefix(err.Error(), "route: parse:") {
		t.Fatalf("error = %v, want parse failure", err)
	}
	if sig.Raw != malformed || sig.Latency <= 0 {
		t.Fatalf("signal = %+v", sig)
	}
}
