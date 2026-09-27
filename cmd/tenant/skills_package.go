package main

// skills_package.go imports and exports skills as Agent Skills packages
// (SKILL.md, TEN-298; the format is internal/skillpkg). An imported skill is
// untrusted until the operator trusts it, and nothing in a package grants
// anything: allowed-tools become the skill's required tools, which decide only
// where it is offered (skillToolAvailable), never what is approved.

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"tenant/internal/memory/skills"
	"tenant/internal/model"
	"tenant/internal/skillpkg"
)

// Metadata keys Tenant writes into an exported package. Other tools ignore
// metadata they do not know, so the package opens anywhere (ADR 0001).
const (
	metaVersion = "version"       // the conventional key; read back on import
	metaOrigin  = "tenant-origin" // how the skill came to be where it was exported
)

// importedSkill reports what importSkillPackage did.
type importedSkill struct {
	Name      string
	Trusted   bool
	Unchanged bool     // the same package was already imported
	Files     []string // bundled files not installed
	Unknown   []string // frontmatter keys the specification does not define
	Requires  []string
}

// Summary is the one-paragraph report a surface prints.
func (r importedSkill) Summary() string {
	var b strings.Builder
	switch {
	case r.Unchanged && r.Trusted:
		fmt.Fprintf(&b, "skill %q is already imported and unchanged (trusted)", r.Name)
	case r.Unchanged:
		fmt.Fprintf(&b, "skill %q is already imported and unchanged (untrusted)", r.Name)
	case r.Trusted:
		fmt.Fprintf(&b, "imported skill %q, trusted", r.Name)
	default:
		fmt.Fprintf(&b, "imported skill %q, untrusted: it is not offered to the agent until you review it (/skills show %s) and trust it (/skills trust %s)", r.Name, r.Name, r.Name)
	}
	if len(r.Requires) > 0 {
		fmt.Fprintf(&b, "\n  expects tools: %s (offered only where they are enabled; this approves nothing)", strings.Join(r.Requires, ", "))
	}
	if len(r.Files) > 0 {
		fmt.Fprintf(&b, "\n  not installed (Tenant keeps the instructions only): %s", strings.Join(r.Files, ", "))
	}
	if len(r.Unknown) > 0 {
		fmt.Fprintf(&b, "\n  ignored frontmatter: %s", strings.Join(r.Unknown, ", "))
	}
	return b.String()
}

// importSkillPackage stores the package at path (its directory or SKILL.md)
// as agentID's skill. It is untrusted unless trust is set. A package may
// replace a skill it imported before, never one made another way.
func importSkillPackage(ctx context.Context, st *skills.Store, emb model.Embedder, agentID, path string, trust bool) (importedSkill, error) {
	p, err := skillpkg.Read(path)
	if err != nil {
		return importedSkill{}, err
	}
	if err := p.Validate(); err != nil {
		return importedSkill{}, fmt.Errorf("%s is not a valid Agent Skills package: %w", path, err)
	}
	res := importedSkill{Name: p.Name, Files: p.Files, Unknown: p.UnknownKeys, Requires: p.AllowedTools}

	cur, err := st.GetByName(ctx, agentID, p.Name)
	if err != nil {
		return res, err
	}
	if cur != nil && cur.Status != skills.StatusTombstoned {
		if cur.Origin != skills.OriginImport {
			return res, fmt.Errorf("a skill named %q already exists and was not imported: forget or rename it first", p.Name)
		}
		if cur.Package != nil && cur.Package.Digest == p.Digest {
			res.Unchanged = true
			if trust && cur.Trust != skills.TrustTrusted {
				if _, err := st.SetTrust(ctx, agentID, p.Name, skills.TrustTrusted); err != nil {
					return res, err
				}
			}
			res.Trusted = trust || cur.Trust == skills.TrustTrusted
			return res, nil
		}
	}

	requires := append([]string{}, p.AllowedTools...) // non-nil: a re-import replaces the list
	sk := &skills.Skill{
		AgentID: agentID, Name: p.Name, Description: p.Description, Recipe: p.Body,
		Status: skills.StatusLive, Enabled: true,
		Trust: skills.TrustUntrusted, Origin: skills.OriginImport, Version: p.Metadata[metaVersion],
		RequiresTools: requires,
		Package: &skills.PackageInfo{
			Source: p.Dir, Digest: p.Digest, License: p.License, Compatibility: p.Compatibility,
			Metadata: p.Metadata, Files: p.Files, ImportedAt: time.Now().UTC(),
		},
	}
	if trust {
		sk.Trust = skills.TrustTrusted
	}
	sk.Embedding = embedSkill(ctx, emb, sk.Name, sk.Description)
	if _, err := st.UpsertWithSource(ctx, sk, "import"); err != nil {
		return res, err
	}
	res.Trusted = trust
	return res, nil
}

// exportSkillPackage writes agentID's skill name as parent/<name>/SKILL.md and
// returns the directory. A name the format does not allow is exported under
// its slug. Trust is not written: it is the importing operator's decision.
func exportSkillPackage(ctx context.Context, st *skills.Store, agentID, name, parent string) (string, error) {
	sk, err := st.GetByName(ctx, agentID, name)
	if err != nil {
		return "", err
	}
	if sk == nil || sk.Status == skills.StatusTombstoned {
		return "", fmt.Errorf("no skill named %q", name)
	}
	pkgName := sk.Name
	if skillpkg.ValidName(pkgName) != nil {
		pkgName = skillpkg.Slug(sk.Name)
		if pkgName == "" {
			return "", fmt.Errorf("skill %q has no characters an Agent Skills name allows", sk.Name)
		}
	}
	p := &skillpkg.Package{
		Name: pkgName, Description: sk.Description, Body: sk.Recipe,
		AllowedTools: sk.RequiresTools, Metadata: map[string]string{},
	}
	if sk.Package != nil {
		p.License, p.Compatibility = sk.Package.License, sk.Package.Compatibility
		maps.Copy(p.Metadata, sk.Package.Metadata)
	}
	if sk.Version != "" {
		p.Metadata[metaVersion] = sk.Version
	}
	if sk.Origin != "" {
		p.Metadata[metaOrigin] = sk.Origin
	}
	if len(p.Metadata) == 0 {
		p.Metadata = nil
	}
	return p.Write(parent)
}

// embedSkill embeds "name: description", the text retrieval compares a turn
// against; nil when there is no embedder or it fails (the skill then ranks
// last, and `tenant memory reembed` can fill it in).
func embedSkill(ctx context.Context, emb model.Embedder, name, description string) []float32 {
	if emb == nil {
		return nil
	}
	vecs, err := emb.Embed(ctx, []string{name + ": " + description})
	if err != nil || len(vecs) != 1 {
		return nil
	}
	return vecs[0]
}

// skillToolAliases maps the tool names Agent Skills packages list in
// allowed-tools (the names of the agents they were written for) to Tenant's.
// A name Tenant does not know stays as written and is never available.
var skillToolAliases = map[string][]string{
	"bash":       {"os_exec"},
	"shell":      {"os_exec"},
	"powershell": {"os_exec"},
	"read":       {"os_read_file"},
	"write":      {"os_write_file"},
	"edit":       {"os_edit_file"},
	"multiedit":  {"os_edit_file"},
	"ls":         {"os_list_dir"},
	"glob":       {"os_list_dir"},
	"webfetch":   {"web_read"},
	"websearch":  {"web_search"},
}

// skillToolAvailable reports whether a required tool (as a package lists it:
// "Bash(git:*)", "Read", or a Tenant tool name) is enabled in tools.
func skillToolAvailable(enabled map[string]bool, required string) bool {
	name := strings.TrimSpace(required)
	if i := strings.IndexByte(name, '('); i > 0 {
		name = name[:i]
	}
	if enabled[name] {
		return true
	}
	aliases, ok := skillToolAliases[strings.ToLower(name)]
	if !ok {
		return false
	}
	for _, a := range aliases {
		if enabled[a] {
			return true
		}
	}
	return false
}
