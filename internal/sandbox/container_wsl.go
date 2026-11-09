package sandbox

// container_wsl.go is the WSL gate of the container backend (W1-1 / TEN-455,
// hardening plan §3): on Windows, Docker Desktop's engine runs on the WSL
// kernel, which is shared with every distro and patched through WSL updates,
// not Desktop updates. So the WSL release and its kernel are part of the
// boundary and gate the run. The parsing is OS-neutral and tested on every OS
// (the live `wsl --version` call is in container_wsl_windows.go).

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf16"
)

// probeWSL reads the WSL release and kernel on Windows and nothing elsewhere
// (container_wsl_windows.go / _other.go). A variable so tests pin it.
var probeWSL func(context.Context) (wsl, kernel string) = probeWSLOS

// desktopVersionProbe reads the Docker Desktop product version where this OS
// has one (container_paths_windows.go). A variable so tests pin it.
var desktopVersionProbe = desktopVersionOS

// WSL floors (plan §3). The kernel floor is Microsoft's first kernel with the
// CVE-2026-53362 fix (upstream 6.18.38; linux-msft-wsl-6.18.40.1). Latest
// versions only warn.
const (
	minWSLVersion    = "2.7.10"
	latestWSLVersion = "2.7.14" // as of 2026-09-30
	minWSLKernel     = "6.18.40"
)

// decodeConsoleOutput turns what a Windows console tool printed into a string:
// `wsl --version` writes UTF-16LE (with or without a BOM), most tools write
// UTF-8. UTF-16 is recognised by its BOM or by NUL bytes in the odd positions.
func decodeConsoleOutput(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	} else if !looksUTF16LE(b) {
		return string(b)
	}
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

// looksUTF16LE reports whether b reads like UTF-16LE ASCII text: at least half
// of the odd bytes are NUL and the even bytes are not.
func looksUTF16LE(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	nulOdd, nulEven := 0, 0
	for i := 0; i+1 < len(b) && i < 256; i += 2 {
		if b[i] == 0 {
			nulEven++
		}
		if b[i+1] == 0 {
			nulOdd++
		}
	}
	n := min(len(b), 256) / 2
	return nulOdd*2 >= n && nulEven*4 < n
}

// parseWSLVersion reads the "WSL version:" and "Kernel version:" lines of
// `wsl --version` (decoded). Either is "" when its line is missing. The kernel
// is returned as printed ("6.18.54.1-1"); compareVersions reads its numbers.
func parseWSLVersion(out string) (wsl, kernel string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "wsl version":
			wsl = v
		case "kernel version":
			kernel = v
		}
	}
	return wsl, kernel
}

// checkWSLFloors gates on the WSL release and kernel: below the fixed releases
// refuses, below the latest known warns, unparseable warns.
func checkWSLFloors(wsl, kernel string) gateReport {
	var g gateReport
	if cmp, ok := compareVersions(wsl, minWSLVersion); !ok {
		g.warn = append(g.warn, fmt.Sprintf("WSL version %q could not be parsed; the floor %s was not checked", wsl, minWSLVersion))
	} else if cmp < 0 {
		g.refuse = append(g.refuse, fmt.Sprintf("WSL %s is below the floor %s; run `wsl --update`", wsl, minWSLVersion))
	} else if c, _ := compareVersions(wsl, latestWSLVersion); c < 0 {
		g.warn = append(g.warn, fmt.Sprintf("WSL %s is below the latest known release %s", wsl, latestWSLVersion))
	}
	if kernel == "" {
		g.warn = append(g.warn, "wsl --version printed no kernel line; the kernel floor was not checked")
		return g
	}
	if cmp, ok := compareVersions(kernel, minWSLKernel); !ok {
		g.warn = append(g.warn, fmt.Sprintf("WSL kernel %q could not be parsed; the floor %s was not checked", kernel, minWSLKernel))
	} else if cmp < 0 {
		g.refuse = append(g.refuse, fmt.Sprintf("WSL kernel %s predates the CVE-2026-53362 fix (%s); run `wsl --update`", kernel, minWSLKernel))
	}
	return g
}

// utf16Bytes renders s as UTF-16LE with a BOM, the way `wsl --version` prints:
// a test helper kept here so the Windows and OS-neutral tests share it.
func utf16Bytes(s string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xFE})
	for _, u := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(u))
		b.WriteByte(byte(u >> 8))
	}
	return b.Bytes()
}
