package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplianceTerminology keeps the product description in line with ADR
// 0001 (TEN-324): Tenant is an appliance extended through MCP servers and
// Agent Skills, not an importable framework. The historical design doc is
// exempt; it carries a pointer to the ADR instead.
func TestApplianceTerminology(t *testing.T) {
	root := filepath.Join("..", "..")
	adr := filepath.Join(root, "docs", "adr", "0001-appliance-not-framework.md")
	if _, err := os.Stat(adr); err != nil {
		t.Fatalf("the appliance ADR is missing: %v", err)
	}
	for _, rel := range []string{"README.md", filepath.Join("docs", "ARCHITECTURE.md"), filepath.Join("docs", "PLUGINS.md"), filepath.Join("docs", "QUICKSTART.md"), filepath.Join("cmd", "tenant", "main.go")} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), "agent framework") {
			t.Errorf("%s calls Tenant an agent framework; ADR 0001 says it is an appliance", rel)
		}
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "docs/adr/0001-appliance-not-framework.md") {
		t.Error("README.md does not point at the appliance ADR")
	}
	design, err := os.ReadFile(filepath.Join(root, "docs", "DESIGN.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(design), "adr/0001-appliance-not-framework.md") {
		t.Error("docs/DESIGN.md still reads as current; it needs its pointer to ADR 0001")
	}
}
