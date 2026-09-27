package skillpkg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const example = `---
name: pdf-forms
description: Fill in PDF forms from structured data. Use when the user hands over a PDF form to complete.
license: Apache-2.0
compatibility: Needs pdftk on PATH.
metadata:
  author: example-org
  version: 1.0
allowed-tools: Bash(pdftk:*) Bash(git log:*) Read
---

# Filling a form

1. List the form's fields.
2. Fill them.
`

func TestParseReadsEveryField(t *testing.T) {
	p, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "pdf-forms" || p.License != "Apache-2.0" || p.Compatibility != "Needs pdftk on PATH." {
		t.Errorf("fields = %+v", p)
	}
	if !strings.HasPrefix(p.Description, "Fill in PDF forms") {
		t.Errorf("description = %q", p.Description)
	}
	if p.Metadata["author"] != "example-org" || p.Metadata["version"] != "1.0" {
		t.Errorf("metadata = %v", p.Metadata)
	}
	if want := []string{"Bash(pdftk:*)", "Bash(git log:*)", "Read"}; !reflect.DeepEqual(p.AllowedTools, want) {
		t.Errorf("allowed-tools = %q, want %q", p.AllowedTools, want)
	}
	if !strings.HasPrefix(p.Body, "# Filling a form") || !strings.Contains(p.Body, "2. Fill them.") {
		t.Errorf("body = %q", p.Body)
	}
	if len(p.Digest) != 64 {
		t.Errorf("digest = %q", p.Digest)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestParseAcceptsCRLFAndBOMAndAToolList(t *testing.T) {
	src := "\xef\xbb\xbf---\r\nname: x\r\ndescription: d\r\nallowed-tools:\r\n  - Read\r\n  - Bash(ls:*) Grep\r\nsomething-else: 1\r\n---\r\nbody\r\n"
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Read", "Bash(ls:*)", "Grep"}; !reflect.DeepEqual(p.AllowedTools, want) {
		t.Errorf("allowed-tools = %q", p.AllowedTools)
	}
	if p.Body != "body\n" {
		t.Errorf("body = %q", p.Body)
	}
	if !reflect.DeepEqual(p.UnknownKeys, []string{"something-else"}) {
		t.Errorf("unknown keys = %v", p.UnknownKeys)
	}
}

func TestParseRefusesMalformedFrontmatter(t *testing.T) {
	for name, src := range map[string]string{
		"no frontmatter":     "# just markdown\n",
		"unclosed":           "---\nname: x\ndescription: d\n",
		"bad yaml":           "---\nname: [x\n---\n",
		"metadata not a map": "---\nname: x\ndescription: d\nmetadata: nope\n---\n",
		"nested metadata":    "---\nname: x\ndescription: d\nmetadata:\n  a:\n    b: c\n---\n",
		"name not a string":  "---\nname: [a, b]\ndescription: d\n---\n",
		"duplicate key":      "---\nname: x\nname: y\ndescription: d\n---\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestValidateAppliesTheSpecification(t *testing.T) {
	ok := func() *Package { return &Package{Name: "a-b2", Description: "d"} }
	if err := ok().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Package){
		"no name":            func(p *Package) { p.Name = "" },
		"upper case":         func(p *Package) { p.Name = "Pdf" },
		"underscore":         func(p *Package) { p.Name = "pdf_forms" },
		"leading hyphen":     func(p *Package) { p.Name = "-pdf" },
		"trailing hyphen":    func(p *Package) { p.Name = "pdf-" },
		"double hyphen":      func(p *Package) { p.Name = "pdf--forms" },
		"long name":          func(p *Package) { p.Name = strings.Repeat("a", 65) },
		"no description":     func(p *Package) { p.Description = "  " },
		"long description":   func(p *Package) { p.Description = strings.Repeat("d", 1025) },
		"long compatibility": func(p *Package) { p.Compatibility = strings.Repeat("c", 501) },
		"directory mismatch": func(p *Package) { p.Dir = filepath.Join("x", "other") },
	} {
		p := ok()
		mutate(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	p := ok()
	p.Name, p.Description = strings.Repeat("a", 64), strings.Repeat("d", 1024)
	if err := p.Validate(); err != nil {
		t.Errorf("at the limits: %v", err)
	}
}

func TestSlugMakesAValidName(t *testing.T) {
	for in, want := range map[string]string{
		"Code Review":      "code-review",
		"gstack_ship!!now": "gstack-ship-now",
		"--x--":            "x",
		"___":              "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
		if got := Slug(in); got != "" {
			if err := ValidName(got); err != nil {
				t.Errorf("Slug(%q) = %q is invalid: %v", in, got, err)
			}
		}
	}
	if got := Slug(strings.Repeat("ab-", 40)); len(got) > MaxNameLen || ValidName(got) != nil {
		t.Errorf("long slug = %q", got)
	}
}

// Render writes what Parse reads back.
func TestRenderRoundTrips(t *testing.T) {
	p, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}
	q, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	q.Digest, p.Digest = "", ""
	if !reflect.DeepEqual(p, q) {
		t.Errorf("round trip:\n%+v\n%+v\n%s", p, q, out)
	}
}

func TestReadAndWrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pdf-forms")
	for path, content := range map[string]string{
		"SKILL.md":               example,
		"scripts/fill.py":        "print('x')\n",
		"references/FIELDS.md":   "fields\n",
		".git/config":            "ignored\n",
		"assets/.DS_Store":       "ignored\n",
		"assets/template.pdf.gz": "x",
	} {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{dir, filepath.Join(dir, "SKILL.md")} {
		p, err := Read(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
		if want := []string{"assets/template.pdf.gz", "references/FIELDS.md", "scripts/fill.py"}; !reflect.DeepEqual(p.Files, want) {
			t.Errorf("files = %v", p.Files)
		}
	}

	p, _ := Read(dir)
	out := t.TempDir()
	written, err := p.Write(out)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(written) != "pdf-forms" {
		t.Errorf("written to %s", written)
	}
	back, err := Read(written)
	if err != nil {
		t.Fatal(err)
	}
	if back.Body != p.Body || back.Name != p.Name || !reflect.DeepEqual(back.AllowedTools, p.AllowedTools) {
		t.Errorf("written package differs: %+v", back)
	}
	if _, err := p.Write(out); err == nil {
		t.Error("Write overwrote an existing SKILL.md")
	}
	bad := *p
	bad.Name = "Bad Name"
	if _, err := bad.Write(out); err == nil {
		t.Error("Write accepted an invalid name")
	}

	if _, err := Read(filepath.Join(root, "missing")); err == nil {
		t.Error("Read of a missing directory succeeded")
	}
	wrong := filepath.Join(root, "other")
	_ = os.MkdirAll(wrong, 0o755)
	_ = os.WriteFile(filepath.Join(wrong, "SKILL.md"), []byte(example), 0o644)
	if p, err := Read(wrong); err != nil || p.Validate() == nil {
		t.Error("a package whose name differs from its directory must fail validation")
	}
}
