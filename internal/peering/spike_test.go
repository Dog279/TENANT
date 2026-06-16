package peering

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pingArgs is the (empty) typed input for the spike tool — the generic
// AddTool[In,Out] helper auto-derives the JSON object schema from it, which is
// the pattern TEN-184's real peer tools use (e.g. {Query string} for search).
type pingArgs struct{}

// TestSpike_GoSDKServerLoop de-risks TEN-184: it stands up a go-sdk
// streamable-HTTP server with ONE stub tool, fronts it with
// auth.RequireBearerToken over a constant-time token verifier (the peers.json
// pattern), and dials it with the same go-sdk client + DisableStandaloneSSE
// posture the mcpremote spine uses (TEN-180 lesson). It asserts the full loop
// works AND that a missing/wrong bearer is rejected.
//
// This is the literal pre-code spike the federation epic calls for. If it
// passes, TEN-184's server path + TEN-186's StaticTokenHandler client path are
// both proven against go-sdk v1.6.1.
func TestSpike_GoSDKServerLoop(t *testing.T) {
	const goodToken = "peer-secret-abc123"

	// --- Server side (TEN-184 shape) ---------------------------------
	// Per-request server factory: this is exactly NewStreamableHTTPHandler's
	// getServer hook — in TEN-184 it yields a per-peer SCOPED server.
	newServer := func(*http.Request) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "tenant-peer", Version: "spike"}, nil)
		// Generic AddTool auto-derives the input schema from pingArgs — the
		// plain (*Server).AddTool requires a hand-written InputSchema or it
		// panics (a real TEN-184 finding).
		mcp.AddTool(s,
			&mcp.Tool{Name: "peer_ping", Description: "spike stub"},
			func(_ context.Context, _ *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
			},
		)
		return s
	}
	handler := mcp.NewStreamableHTTPHandler(newServer, nil)

	// Bearer verifier: constant-time compare (crypto/subtle), non-zero
	// Expiration is MANDATORY (go-sdk verify() 401s on a zero expiration —
	// a real TEN-184 finding: peer tokens must carry a far-future expiry).
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if subtle.ConstantTimeCompare([]byte(token), []byte(goodToken)) != 1 {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(24 * time.Hour)}, nil
	}
	authed := auth.RequireBearerToken(verifier, nil)(handler)

	srv := httptest.NewServer(authed)
	defer srv.Close()

	// --- Client side (TEN-186 StaticTokenHandler shape) --------------
	// A plain HTTP client that injects the static bearer on every request —
	// the StaticTokenHandler TEN-186 adds beside mcpremote's persistentHandler.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connect := func(token string) (*mcp.ClientSession, error) {
		httpClient := &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}}
		transport := &mcp.StreamableClientTransport{
			Endpoint:             srv.URL,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true, // TEN-180: request/response only
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "spike"}, nil)
		return client.Connect(ctx, transport, nil)
	}

	// Happy path: connect, list, call.
	session, err := connect(goodToken)
	if err != nil {
		t.Fatalf("connect with good token failed: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tl := range tools.Tools {
		if tl.Name == "peer_ping" {
			found = true
		}
	}
	if !found {
		t.Fatalf("peer_ping not in tools list: %+v", tools.Tools)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "peer_ping"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("CallTool returned no content")
	}
	if txt, ok := res.Content[0].(*mcp.TextContent); !ok || txt.Text != "pong" {
		t.Fatalf("CallTool content = %+v, want text 'pong'", res.Content[0])
	}

	// Auth gate: a wrong token must be refused at connect (no usable session).
	if bad, err := connect("wrong-token"); err == nil {
		bad.Close()
		t.Fatal("connect with WRONG token should have failed, but succeeded")
	}
}

// bearerRoundTripper injects a static Authorization header — the minimal
// StaticTokenHandler shape for TEN-186.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
