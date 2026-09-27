// Package skillpkg reads and writes Agent Skills packages (TEN-298): the
// open SKILL.md format. A package is a directory named after its skill,
// holding a SKILL.md whose YAML frontmatter describes the skill and whose
// Markdown body is its instructions:
//
//	---
//	name: pdf-forms                # required: 1-64 of a-z, 0-9 and single hyphens; the directory's name
//	description: Fill PDF forms…   # required: 1-1024 characters; what it does and when to use it
//	license: Apache-2.0            # optional
//	compatibility: needs poppler   # optional: at most 500 characters
//	metadata:                      # optional: string keys to string values
//	  author: example-org
//	  version: "1.0"
//	allowed-tools: Bash(pdftk:*) Read  # optional, experimental: tools the skill expects to use
//	---
//	# Filling a form
//	1. …
//
// A package may also bundle scripts/, references/ and assets/. This package
// only reads them as a list: the caller decides what, if anything, to install.
package skillpkg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// FileName is the one file a package must hold.
const FileName = "SKILL.md"

// Limits from the specification.
const (
	MaxNameLen          = 64
	MaxDescriptionLen   = 1024
	MaxCompatibilityLen = 500
	// MaxFileSize bounds what Read accepts for SKILL.md: far above the
	// specification's advice (under 500 lines), far below anything that is
	// not instructions.
	MaxFileSize = 1 << 20
)

// Package is one Agent Skills package.
type Package struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	Metadata      map[string]string
	// AllowedTools is the experimental allowed-tools field: the tools the skill
	// expects to use, one entry per space-separated item (Bash(git:*), Read, …).
	AllowedTools []string
	// Body is the Markdown after the frontmatter: the skill's instructions.
	Body string

	// Read fills these; Render ignores them.
	Dir         string   // the package directory, when read from disk
	Files       []string // the package's other files, slash-separated, relative to Dir, sorted
	Digest      string   // sha256 of SKILL.md as read, hex
	UnknownKeys []string // frontmatter keys the specification does not define, sorted
}

// frontmatter is SKILL.md's YAML header as the specification defines it.
type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  string            `yaml:"allowed-tools,omitempty"`
}

var knownKeys = map[string]bool{"name": true, "description": true, "license": true, "compatibility": true, "metadata": true, "allowed-tools": true}

// Parse reads SKILL.md's content. It does not validate; see Validate.
func Parse(data []byte) (*Package, error) {
	sum := sha256.Sum256(data)
	text := string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, errors.New("SKILL.md must start with a --- line opening its YAML frontmatter")
	}
	rest := text[len("---\n"):]
	var head, body string
	switch {
	case strings.HasPrefix(rest, "---\n"):
		head, body = "", rest[len("---\n"):]
	case rest == "---":
		head, body = "", ""
	default:
		end := strings.Index(rest, "\n---\n")
		if end < 0 {
			if !strings.HasSuffix(rest, "\n---") {
				return nil, errors.New("SKILL.md's frontmatter has no closing --- line")
			}
			end = len(rest) - len("\n---")
			head, body = rest[:end], ""
		} else {
			head, body = rest[:end], rest[end+len("\n---\n"):]
		}
	}

	var raw map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(head), &raw); err != nil {
		return nil, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	p := &Package{Body: strings.TrimLeft(body, "\n"), Digest: hex.EncodeToString(sum[:])}
	for key, node := range raw {
		if !knownKeys[key] {
			p.UnknownKeys = append(p.UnknownKeys, key)
			continue
		}
		node := node
		switch key {
		case "name":
			if err := scalar(&node, key, &p.Name); err != nil {
				return nil, err
			}
		case "description":
			if err := scalar(&node, key, &p.Description); err != nil {
				return nil, err
			}
		case "license":
			if err := scalar(&node, key, &p.License); err != nil {
				return nil, err
			}
		case "compatibility":
			if err := scalar(&node, key, &p.Compatibility); err != nil {
				return nil, err
			}
		case "metadata":
			if node.Kind != yaml.MappingNode {
				return nil, errors.New("SKILL.md frontmatter: metadata must be a map of strings")
			}
			p.Metadata = map[string]string{}
			for i := 0; i+1 < len(node.Content); i += 2 {
				k, v := node.Content[i], node.Content[i+1]
				if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("SKILL.md frontmatter: metadata %q must map to a string", k.Value)
				}
				p.Metadata[k.Value] = v.Value
			}
		case "allowed-tools":
			tools, err := toolList(&node)
			if err != nil {
				return nil, err
			}
			p.AllowedTools = tools
		}
	}
	sort.Strings(p.UnknownKeys)
	return p, nil
}

func scalar(node *yaml.Node, key string, out *string) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("SKILL.md frontmatter: %s must be a string", key)
	}
	*out = strings.TrimSpace(node.Value)
	return nil
}

// toolList reads allowed-tools: the specification's space-separated string, or
// the YAML list some tools write.
func toolList(node *yaml.Node) ([]string, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		return splitTools(node.Value), nil
	case yaml.SequenceNode:
		var out []string
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, errors.New("SKILL.md frontmatter: allowed-tools must list strings")
			}
			out = append(out, splitTools(item.Value)...)
		}
		return out, nil
	}
	return nil, errors.New("SKILL.md frontmatter: allowed-tools must be a string")
}

// splitTools splits on spaces outside parentheses, so Bash(git log:*) is one tool.
func splitTools(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case unicode.IsSpace(r) && depth == 0:
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// Validate checks the package against the specification's constraints.
func (p *Package) Validate() error {
	var problems []string
	if err := ValidName(p.Name); err != nil {
		problems = append(problems, err.Error())
	}
	switch n := utf8.RuneCountInString(p.Description); {
	case strings.TrimSpace(p.Description) == "":
		problems = append(problems, "description is required")
	case n > MaxDescriptionLen:
		problems = append(problems, fmt.Sprintf("description is %d characters; the limit is %d", n, MaxDescriptionLen))
	}
	if n := utf8.RuneCountInString(p.Compatibility); n > MaxCompatibilityLen {
		problems = append(problems, fmt.Sprintf("compatibility is %d characters; the limit is %d", n, MaxCompatibilityLen))
	}
	if p.Dir != "" && p.Name != "" && filepath.Base(p.Dir) != p.Name {
		problems = append(problems, fmt.Sprintf("name %q must match the package directory %q", p.Name, filepath.Base(p.Dir)))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ValidName checks a skill name: 1-64 lowercase letters, digits and hyphens,
// not starting or ending with a hyphen, and no two hyphens in a row.
func ValidName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required")
	case utf8.RuneCountInString(name) > MaxNameLen:
		return fmt.Errorf("name %q is longer than %d characters", name, MaxNameLen)
	case strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-"):
		return fmt.Errorf("name %q must not start or end with a hyphen", name)
	case strings.Contains(name, "--"):
		return fmt.Errorf("name %q must not contain two hyphens in a row", name)
	}
	for _, r := range name {
		if r != '-' && !unicode.IsDigit(r) && !unicode.IsLower(r) {
			return fmt.Errorf("name %q may hold only lowercase letters, digits and hyphens", name)
		}
	}
	return nil
}

// Slug turns any name into a valid one, or "" when nothing is left: lowercase,
// every run of other characters becomes one hyphen, trimmed to the limit.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if unicode.IsLower(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteRune('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if utf8.RuneCountInString(s) > MaxNameLen {
		s = strings.TrimRight(string([]rune(s)[:MaxNameLen]), "-")
	}
	return s
}

// Render writes the package as SKILL.md.
func (p *Package) Render() ([]byte, error) {
	fm := frontmatter{
		Name: p.Name, Description: p.Description, License: p.License,
		Compatibility: p.Compatibility, Metadata: p.Metadata,
		AllowedTools: strings.Join(p.AllowedTools, " "),
	}
	var head bytes.Buffer
	enc := yaml.NewEncoder(&head)
	enc.SetIndent(2)
	if err := enc.Encode(&fm); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(head.Bytes())
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimLeft(p.Body, "\n"))
	if !strings.HasSuffix(p.Body, "\n") {
		b.WriteString("\n")
	}
	return b.Bytes(), nil
}

// Read reads a package from its directory, or from its SKILL.md.
func Read(path string) (*Package, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	dir, file := path, filepath.Join(path, FileName)
	if !info.IsDir() {
		if filepath.Base(path) != FileName {
			return nil, fmt.Errorf("%s is not a %s", path, FileName)
		}
		dir, file = filepath.Dir(path), path
	}
	fi, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("no %s in %s", FileName, dir)
	}
	if fi.Size() > MaxFileSize {
		return nil, fmt.Errorf("%s is %d bytes; the limit is %d", file, fi.Size(), MaxFileSize)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	p.Dir = abs
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == FileName || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		p.Files = append(p.Files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(p.Files)
	return p, nil
}

// Write writes the package as parent/<name>/SKILL.md and returns the
// directory. It refuses an invalid package and an existing SKILL.md.
func (p *Package) Write(parent string) (string, error) {
	q := *p
	q.Dir = ""
	if err := q.Validate(); err != nil {
		return "", err
	}
	data, err := p.Render()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, p.Name)
	file := filepath.Join(dir, FileName)
	if _, err := os.Stat(file); err == nil {
		return "", fmt.Errorf("%s already exists", file)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return "", err
	}
	return dir, nil
}
