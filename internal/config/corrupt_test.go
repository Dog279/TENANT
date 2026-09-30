package config

import (
	"os"
	"strings"
	"testing"
)

// A config.json that doesn't parse says where it broke, for a syntax error
// and for a value of the wrong type.
func TestLoadNamesWhereTheFileBroke(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"{\n  \"providers\": {,\n}\n", "at line 2, column 17"},
		{"{\n  \"plan_loop_ceiling\": \"many\"\n}\n", "at line 2, column 29"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(Path(dir), []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load(%q) = %v, want %q", tc.body, err, tc.want)
		}
	}
}

// A saved gateway mode "both" loads as sse: one mcp-memory process serves one
// transport, and "both" never served stdio as well.
func TestGatewayBothLoadsAsSSE(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{"gateway":{"mode":"both","sse_addr":"127.0.0.1:8765"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lc, err := Load(dir)
	if err != nil || lc.Gateway.Mode != "sse" || lc.Gateway.SSEAddr != "127.0.0.1:8765" {
		t.Fatalf("gateway = %+v, %v", lc.Gateway, err)
	}
}
