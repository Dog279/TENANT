package main

import "testing"

func seedSetupConfig(t *testing.T) (string, setupControl) {
	t.Helper()
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "zai",
		Providers: map[string]*providerConfig{
			"zai": {Kind: "zai", Endpoint: "https://old", Model: "glm-4.6", ToolFmt: "openai"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	return dir, setupControl{cfgDir: dir} // mc nil → reload() returns "saved"
}

func TestSetupControl_Snapshot(t *testing.T) {
	_, s := seedSetupConfig(t)
	v := s.Snapshot()
	if v.Provider != "zai" || v.Model != "glm-4.6" || v.Endpoint != "https://old" || v.ToolFormat != "openai" {
		t.Fatalf("snapshot mismatch: %+v", v)
	}
	if !v.NeedsKey || !v.NeedsEndpoint || !v.IsVLLM {
		t.Errorf("zai should need key+endpoint and be vllm: %+v", v)
	}
	if v.KeySet {
		t.Error("no key stored yet → KeySet should be false")
	}
}

func TestSetupControl_SettersPersist(t *testing.T) {
	dir, s := seedSetupConfig(t)

	if _, err := s.SetEndpoint("https://new/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel("glm-5.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetToolFormat("qwen"); err != nil {
		t.Fatal(err)
	}
	lc, _ := loadLaunchConfig(dir)
	pc := lc.Providers["zai"]
	if pc.Endpoint != "https://new" { // trailing slash trimmed
		t.Errorf("endpoint=%q", pc.Endpoint)
	}
	if pc.Model != "glm-5.1" {
		t.Errorf("model=%q", pc.Model)
	}
	if pc.ToolFmt != "qwen" {
		t.Errorf("toolfmt=%q", pc.ToolFmt)
	}
}

func TestSetupControl_SetKey(t *testing.T) {
	dir, s := seedSetupConfig(t)
	if _, err := s.SetKey("sk-zai-key"); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(dir); creds.get("zai") != "sk-zai-key" {
		t.Fatalf("key not stored: %q", creds.get("zai"))
	}
	lc, _ := loadLaunchConfig(dir)
	if !lc.Providers["zai"].Auth.Stored {
		t.Error("Auth.Stored should be set so the pasted key is used")
	}
	if !s.Snapshot().KeySet {
		t.Error("Snapshot should now report KeySet")
	}
	if _, err := s.SetKey("  "); err == nil {
		t.Error("blank key should error")
	}
}

func TestSetupControl_EmbeddingsAndGateway(t *testing.T) {
	dir, s := seedSetupConfig(t)
	if _, err := s.SetEmbeddings("http://e:11434/", "nomic"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGateway("sse", "127.0.0.1:9000"); err != nil {
		t.Fatal(err)
	}
	lc, _ := loadLaunchConfig(dir)
	if lc.Embed == nil || lc.Embed.Endpoint != "http://e:11434" || lc.Embed.Model != "nomic" {
		t.Errorf("embed not persisted: %+v", lc.Embed)
	}
	if lc.Gateway.Mode != "sse" || lc.Gateway.SSEAddr != "127.0.0.1:9000" {
		t.Errorf("gateway not persisted: %+v", lc.Gateway)
	}
}

func TestSetupControl_SetProviderNeedsModelControl(t *testing.T) {
	_, s := seedSetupConfig(t) // mc nil
	if _, err := s.SetProvider("openai"); err == nil {
		t.Error("SetProvider with no model control should error, not panic")
	}
}
