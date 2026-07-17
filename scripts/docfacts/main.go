// Command docfacts measures repository facts used by the public documentation.
//
// Run it from the repository root:
//
//	go run ./scripts/docfacts
//	go run ./scripts/docfacts -write docs/ARCHITECTURE.md
//	go run ./scripts/docfacts -check docs/ARCHITECTURE.md
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	generatedStart = "<!-- docfacts:generated:start"
	generatedEnd   = "<!-- docfacts:generated:end -->"
)

type facts struct {
	GoPackages         int    `json:"go_packages"`
	GoFiles            int    `json:"go_files"`
	TestFiles          int    `json:"test_files"`
	ProductionLOC      int    `json:"production_loc"`
	TestLOC            int    `json:"test_loc"`
	ModuleCount        int    `json:"module_count"`
	CapabilityPackages int    `json:"capability_packages"`
	Version            string `json:"version"`
}

func main() {
	var writePath string
	var checkPath string
	flag.StringVar(&writePath, "write", "", "replace the generated facts block in a Markdown file")
	flag.StringVar(&checkPath, "check", "", "fail if a Markdown file's generated facts block is stale")
	flag.Parse()

	if writePath != "" && checkPath != "" {
		fatal(errors.New("-write and -check are mutually exclusive"))
	}

	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	f, err := collect(root)
	if err != nil {
		fatal(err)
	}

	switch {
	case writePath != "":
		if err := writeGeneratedBlock(resolvePath(root, writePath), renderMarkdown(f)); err != nil {
			fatal(err)
		}
	case checkPath != "":
		if err := checkGeneratedBlock(resolvePath(root, checkPath), renderMarkdown(f)); err != nil {
			fatal(err)
		}
	default:
		fmt.Print(renderMarkdown(f))
		fmt.Println()
		fmt.Println("```json")
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(f); err != nil {
			fatal(err)
		}
		fmt.Println("```")
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "docfacts:", err)
	os.Exit(1)
}

func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find repository go.mod")
		}
		dir = parent
	}
}

func resolvePath(root, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(root, name)
}

func collect(root string) (facts, error) {
	var f facts
	moduleFiles := make([]string, 0, 1)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "go.mod" {
			moduleFiles = append(moduleFiles, path)
		}
		if filepath.Ext(entry.Name()) != ".go" {
			return nil
		}

		lines, err := fileLineCount(path)
		if err != nil {
			return err
		}
		f.GoFiles++
		if strings.HasSuffix(entry.Name(), "_test.go") {
			f.TestFiles++
			f.TestLOC += lines
		} else {
			f.ProductionLOC += lines
		}
		return nil
	})
	if err != nil {
		return facts{}, fmt.Errorf("walk repository: %w", err)
	}
	f.ModuleCount = len(moduleFiles)

	packages, err := packageCount(root)
	if err != nil {
		return facts{}, err
	}
	f.GoPackages = packages

	capabilities, err := capabilityPackageCount(filepath.Join(root, "internal", "plugins"))
	if err != nil {
		return facts{}, err
	}
	f.CapabilityPackages = capabilities

	version, err := libraryVersion(filepath.Join(root, "internal", "mcp", "version.go"))
	if err != nil {
		return facts{}, err
	}
	f.Version = version
	return f, nil
}

func fileLineCount(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lines := 0
	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			lines++
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) > 0 {
				lines++
			}
			return lines, nil
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
}

func packageCount(root string) (int, error) {
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return 0, fmt.Errorf("go list ./...: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return 0, fmt.Errorf("go list ./...: %w", err)
	}
	return len(bytes.Fields(out)), nil
}

func capabilityPackageCount(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read plugin packages: %w", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		goFiles, err := filepath.Glob(filepath.Join(dir, entry.Name(), "*.go"))
		if err != nil {
			return 0, fmt.Errorf("scan plugin package %s: %w", entry.Name(), err)
		}
		for _, goFile := range goFiles {
			if !strings.HasSuffix(goFile, "_test.go") {
				count++
				break
			}
		}
	}
	return count, nil
}

func libraryVersion(path string) (string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return "", fmt.Errorf("parse version file: %w", err)
	}
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if name.Name != "LibraryVersion" || i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return "", errors.New("LibraryVersion is not a string literal")
				}
				version, err := strconv.Unquote(literal.Value)
				if err != nil {
					return "", fmt.Errorf("unquote LibraryVersion: %w", err)
				}
				return version, nil
			}
		}
	}
	return "", errors.New("LibraryVersion constant not found")
}

func renderMarkdown(f facts) string {
	rows := [][2]string{
		{"Go packages", formatInt(f.GoPackages)},
		{"Go files", formatInt(f.GoFiles)},
		{"Test files", formatInt(f.TestFiles)},
		{"Production Go LOC", formatInt(f.ProductionLOC)},
		{"Test Go LOC", formatInt(f.TestLOC)},
		{"Go modules", formatInt(f.ModuleCount)},
		{"Plugin capability packages", formatInt(f.CapabilityPackages)},
		{"Version", "`" + f.Version + "`"},
	}
	var out strings.Builder
	out.WriteString("| Measure | Result |\n")
	out.WriteString("|---|---:|\n")
	for _, row := range rows {
		fmt.Fprintf(&out, "| %s | %s |\n", row[0], row[1])
	}
	return out.String()
}

func formatInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func generatedBounds(data []byte) (int, int, error) {
	start := bytes.Index(data, []byte(generatedStart))
	if start < 0 {
		return 0, 0, errors.New("generated start marker not found")
	}
	headerEndRelative := bytes.IndexByte(data[start:], '\n')
	if headerEndRelative < 0 {
		return 0, 0, errors.New("generated start marker has no newline")
	}
	contentStart := start + headerEndRelative + 1
	endRelative := bytes.Index(data[contentStart:], []byte(generatedEnd))
	if endRelative < 0 {
		return 0, 0, errors.New("generated end marker not found")
	}
	return contentStart, contentStart + endRelative, nil
}

func writeGeneratedBlock(path, markdown string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	start, end, err := generatedBounds(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	updated := append([]byte(nil), data[:start]...)
	updated = append(updated, markdown...)
	updated = append(updated, data[end:]...)

	markerStart := bytes.LastIndex(updated[:start], []byte(generatedStart))
	markerEnd := bytes.IndexByte(updated[markerStart:], '\n')
	markerEnd += markerStart
	marker := fmt.Sprintf("%s; date=%s; command=go run ./scripts/docfacts -write docs/ARCHITECTURE.md -->", generatedStart, time.Now().UTC().Format(time.DateOnly))
	updated = append(append(append([]byte(nil), updated[:markerStart]...), marker...), updated[markerEnd:]...)
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func checkGeneratedBlock(path, markdown string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	start, end, err := generatedBounds(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	got := strings.ReplaceAll(string(data[start:end]), "\r\n", "\n")
	want := strings.ReplaceAll(markdown, "\r\n", "\n")
	if got != want {
		return fmt.Errorf("%s has stale generated facts; run go run ./scripts/docfacts -write docs/ARCHITECTURE.md", path)
	}
	return nil
}
