package sandbox

import (
	"strings"
	"testing"
)

// `wsl --version` as this box printed it before and after `wsl --update` on
// 2026-11-09: UTF-16LE with CRLF line ends.
const (
	wslBefore = "WSL version: 2.5.10.0\r\nKernel version: 6.6.87.2-1\r\nWSLg version: 1.0.66\r\nMSRDC version: 1.2.6074\r\nDirect3D version: 1.611.1-81528511\r\nDXCore version: 10.0.26100.1-240331-1435.ge-release\r\nWindows version: 10.0.26200.9711\r\n"
	wslAfter  = "WSL version: 3.0.3.0\r\nKernel version: 6.18.54.1-1\r\nWSLg version: 1.0.79\r\nMSRDC version: 1.2.7391\r\nDirect3D version: 1.611.1-81528511\r\nDXCore version: 10.0.26100.1-240331-1435.ge-release\r\nWindows version: 10.0.26200.9711\r\n"
)

// W1-1: `wsl --version` is UTF-16; the parser reads it with or without a BOM,
// and plain UTF-8 too.
func TestDecodeConsoleOutput(t *testing.T) {
	withBOM := utf16Bytes(wslAfter)
	if got := decodeConsoleOutput(withBOM); got != wslAfter {
		t.Errorf("BOM: got %q", got)
	}
	if got := decodeConsoleOutput(withBOM[2:]); got != wslAfter {
		t.Errorf("no BOM: got %q", got)
	}
	if got := decodeConsoleOutput([]byte(wslAfter)); got != wslAfter {
		t.Errorf("UTF-8 passthrough: got %q", got)
	}
	if got := decodeConsoleOutput([]byte("ok")); got != "ok" {
		t.Errorf("short UTF-8: got %q", got)
	}
}

func TestParseWSLVersion(t *testing.T) {
	wsl, kernel := parseWSLVersion(decodeConsoleOutput(utf16Bytes(wslBefore)))
	if wsl != "2.5.10.0" || kernel != "6.6.87.2-1" {
		t.Errorf("before: got (%q, %q)", wsl, kernel)
	}
	wsl, kernel = parseWSLVersion(wslAfter)
	if wsl != "3.0.3.0" || kernel != "6.18.54.1-1" {
		t.Errorf("after: got (%q, %q)", wsl, kernel)
	}
	if wsl, kernel := parseWSLVersion("nonsense"); wsl != "" || kernel != "" {
		t.Errorf("nonsense: got (%q, %q)", wsl, kernel)
	}
}

// W1-1: the WSL release and kernel floors refuse below the fixed releases.
// This box before `wsl --update` (2.5.10 / 6.6.87) is refused on both; after
// (3.0.3 / 6.18.54) it passes.
func TestCheckWSLFloors(t *testing.T) {
	g := checkWSLFloors("2.5.10.0", "6.6.87.2-1")
	if len(g.refuse) != 2 || !strings.Contains(g.refuse[0], "WSL 2.5.10.0") || !strings.Contains(g.refuse[1], "CVE-2026-53362") {
		t.Errorf("old box: expected two refusals, got %+v", g)
	}
	g = checkWSLFloors("3.0.3.0", "6.18.54.1-1")
	if len(g.refuse) != 0 {
		t.Errorf("updated box: expected no refusal, got %v", g.refuse)
	}
	// The probe box in the plan: WSL current, kernel 6.18.33.2 predating the fix.
	g = checkWSLFloors("2.7.14.0", "6.18.33.2-microsoft-standard-WSL2")
	if len(g.refuse) != 1 || !strings.Contains(g.refuse[0], "6.18.33.2") {
		t.Errorf("probe box: expected the kernel refusal only, got %+v", g)
	}
	g = checkWSLFloors("2.7.12.0", "")
	if len(g.refuse) != 0 || len(g.warn) != 2 {
		t.Errorf("no kernel line: expected two warnings (behind latest, no kernel), got %+v", g)
	}
}
