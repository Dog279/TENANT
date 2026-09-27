// Package pluginreg is the registry of first-party plugins (TEN-321): every
// plugin is declared once, by a descriptor, and everything that needs to know
// about plugins — the tool mux's stubs and activators, the approval
// categories, the configure surfaces, doctor, the CLI — reads the registry
// instead of keeping its own switchboard.
//
// The descriptors themselves belong to the composition root, which knows how
// to build each plugin; the registry holds any descriptor type that declares
// its identity, tools and permission classes, checks the invariants across
// all of them, and indexes them.
package pluginreg

import (
	"fmt"
	"sort"
	"strings"
)

// Declared is what the registry reads from a descriptor.
type Declared interface {
	// PluginID is the plugin's one name: its tool-mux label, its key under
	// skills in config.json, and the <id> in credentials' skill:<id>:<field>.
	PluginID() string
	// ToolNames are the tools the plugin can register.
	ToolNames() []string
	// ActionCategories maps each approval action id the plugin raises to its
	// category: the plugin's permission classes.
	ActionCategories() map[string]string
}

// Registry holds the declared plugins in registration order.
type Registry[D Declared] struct {
	list     []D
	byID     map[string]D
	byTool   map[string]string
	byAction map[string]string
}

// Rules are the constraints New checks the descriptors against.
type Rules struct {
	// Categories are the approval categories an action may belong to.
	Categories []string
	// Reserved are labels the host uses itself (host tools, connector
	// families); a plugin may not take one. A trailing '*' reserves a prefix.
	Reserved []string
}

// New checks the descriptors and indexes them. It refuses an empty or
// duplicate id, a reserved id, a tool two plugins declare, an action two
// plugins declare, and an action in an unknown category.
func New[D Declared](rules Rules, ds ...D) (*Registry[D], error) {
	r := &Registry[D]{byID: map[string]D{}, byTool: map[string]string{}, byAction: map[string]string{}}
	cats := map[string]bool{}
	for _, c := range rules.Categories {
		cats[c] = true
	}
	for _, d := range ds {
		id := d.PluginID()
		switch {
		case strings.TrimSpace(id) == "" || id != strings.TrimSpace(id):
			return nil, fmt.Errorf("pluginreg: a plugin has an empty or padded id %q", id)
		case reserved(rules.Reserved, id):
			return nil, fmt.Errorf("pluginreg: plugin id %q is reserved by the host", id)
		}
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("pluginreg: plugin %q is declared twice", id)
		}
		for _, tool := range d.ToolNames() {
			if owner, dup := r.byTool[tool]; dup {
				return nil, fmt.Errorf("pluginreg: tool %q is declared by both %q and %q", tool, owner, id)
			}
			r.byTool[tool] = id
		}
		actions := d.ActionCategories()
		names := make([]string, 0, len(actions))
		for a := range actions {
			names = append(names, a)
		}
		sort.Strings(names)
		for _, a := range names {
			if !cats[actions[a]] {
				return nil, fmt.Errorf("pluginreg: plugin %q files action %q under unknown category %q", id, a, actions[a])
			}
			if owner, dup := r.byAction[a]; dup {
				return nil, fmt.Errorf("pluginreg: action %q is declared by both %q and %q", a, owner, id)
			}
			r.byAction[a] = id
		}
		r.byID[id] = d
		r.list = append(r.list, d)
	}
	return r, nil
}

// MustNew is New for a registry built from code, where an error is a bug.
func MustNew[D Declared](rules Rules, ds ...D) *Registry[D] {
	r, err := New(rules, ds...)
	if err != nil {
		panic(err)
	}
	return r
}

func reserved(list []string, id string) bool {
	for _, r := range list {
		if p, ok := strings.CutSuffix(r, "*"); ok {
			if strings.HasPrefix(id, p) {
				return true
			}
		} else if r == id {
			return true
		}
	}
	return false
}

// All returns the plugins in registration order.
func (r *Registry[D]) All() []D { return append([]D(nil), r.list...) }

// Get returns the plugin declared under id.
func (r *Registry[D]) Get(id string) (D, bool) {
	d, ok := r.byID[id]
	return d, ok
}

// OwnerOfTool returns the id of the plugin that declares tool.
func (r *Registry[D]) OwnerOfTool(tool string) (string, bool) {
	id, ok := r.byTool[tool]
	return id, ok
}

// ActionCategory returns the category a plugin files action under.
func (r *Registry[D]) ActionCategory(action string) (string, bool) {
	id, ok := r.byAction[action]
	if !ok {
		return "", false
	}
	return r.byID[id].ActionCategories()[action], true
}

// Actions returns every declared action id with its category.
func (r *Registry[D]) Actions() map[string]string {
	out := make(map[string]string, len(r.byAction))
	for a := range r.byAction {
		cat, _ := r.ActionCategory(a)
		out[a] = cat
	}
	return out
}
