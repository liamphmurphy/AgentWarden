// Package agent loads agent definitions and runs the model loop.
package agent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lmurphy/agentwarden/internal/enforce"
	"gopkg.in/yaml.v3"
)

// Mode determines whether an agent can be selected directly.
type Mode string

const (
	// ModePrimary agents can be the session's top-level agent.
	ModePrimary Mode = "primary"
	// ModeSubagent agents can only be delegated to.
	ModeSubagent Mode = "subagent"
	// ModeAll agents can be either.
	ModeAll Mode = "all"
)

// Definition is one agent loaded from markdown. The frontmatter mirrors the
// existing OpenCode agent format so current agent files port over unchanged,
// while also accepting the older tools/skills shape.
type Definition struct {
	// Name defaults to the filename without its extension.
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Mode        Mode   `yaml:"mode"`
	// Model optionally overrides the session model, as "provider/model".
	Model string `yaml:"model"`
	// Skills names the skills injected into this agent's prompt.
	Skills []string `yaml:"skills"`
	// Permissions is the ordered rule list; later rules win.
	Permissions []enforce.Rule `yaml:"permissions"`
	// Tools is the older boolean map form, kept so existing files still load.
	Tools map[string]bool `yaml:"tools"`
	// Prompt is the markdown body: the agent's role instructions.
	Prompt string `yaml:"-"`
	// Path records where the definition came from, for error messages.
	Path string `yaml:"-"`
}

// IsSubagent reports whether the agent may be delegated to.
func (d *Definition) IsSubagent() bool {
	return d.Mode == ModeSubagent || d.Mode == ModeAll || d.Mode == ""
}

// IsPrimary reports whether the agent may lead a session.
func (d *Definition) IsPrimary() bool {
	return d.Mode == ModePrimary || d.Mode == ModeAll
}

// AllowsTool reports whether the older tools map permits a tool. A definition
// without a tools map allows everything, leaving the decision to permissions
// and the enforcer.
func (d *Definition) AllowsTool(name string) bool {
	if len(d.Tools) == 0 {
		return true
	}
	allowed, mentioned := d.Tools[name]
	if !mentioned {
		return true
	}
	return allowed
}

// frontmatterDelimiter separates YAML frontmatter from the markdown body.
const frontmatterDelimiter = "---"

// ParseDefinition reads one agent markdown file.
func ParseDefinition(path string, raw []byte) (*Definition, error) {
	frontmatter, body := splitFrontmatter(raw)

	def := &Definition{Path: path}
	if len(frontmatter) > 0 {
		if err := yaml.Unmarshal(frontmatter, def); err != nil {
			return nil, fmt.Errorf("%s: parse frontmatter: %w", path, err)
		}
	}
	def.Prompt = strings.TrimSpace(string(body))
	if def.Name == "" {
		def.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if def.Mode == "" {
		def.Mode = ModeSubagent
	}
	switch def.Mode {
	case ModePrimary, ModeSubagent, ModeAll:
	default:
		return nil, fmt.Errorf("%s: unknown mode %q", path, def.Mode)
	}
	for i, rule := range def.Permissions {
		switch rule.Effect {
		case enforce.EffectAllow, enforce.EffectAsk, enforce.EffectDeny:
		default:
			return nil, fmt.Errorf("%s: permission %d has unknown effect %q", path, i, rule.Effect)
		}
	}
	return def, nil
}

// splitFrontmatter separates a leading YAML block from the body. A file with
// no frontmatter is treated as all body, so a plain prompt file still loads.
func splitFrontmatter(raw []byte) (frontmatter, body []byte) {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(frontmatterDelimiter)) {
		return nil, raw
	}
	// Skip the opening delimiter line.
	rest := trimmed[len(frontmatterDelimiter):]
	if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
		rest = rest[idx+1:]
	} else {
		return nil, raw
	}

	// Find the closing delimiter at the start of a line.
	lines := bytes.Split(rest, []byte("\n"))
	for i, line := range lines {
		if strings.TrimRight(string(line), " \t\r") == frontmatterDelimiter {
			frontmatter = bytes.Join(lines[:i], []byte("\n"))
			body = bytes.Join(lines[i+1:], []byte("\n"))
			return frontmatter, body
		}
	}
	// Unterminated frontmatter: treat the whole file as body rather than
	// silently dropping the content.
	return nil, raw
}

// Registry holds the loaded agent definitions.
type Registry struct {
	byName map[string]*Definition
	order  []string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]*Definition{}}
}

// Add registers a definition, replacing an earlier one of the same name. Later
// directories therefore override earlier ones, letting a project agent shadow
// a global one.
func (r *Registry) Add(def *Definition) {
	if _, exists := r.byName[def.Name]; !exists {
		r.order = append(r.order, def.Name)
	}
	r.byName[def.Name] = def
}

// Get returns a definition by name.
func (r *Registry) Get(name string) (*Definition, bool) {
	def, ok := r.byName[name]
	return def, ok
}

// Names lists every agent name, sorted.
func (r *Registry) Names() []string {
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// Subagents lists the agents that may be delegated to, sorted. This feeds the
// task tool's enum so the model chooses from a closed set.
func (r *Registry) Subagents() []string {
	var out []string
	for _, name := range r.order {
		if r.byName[name].IsSubagent() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Len reports how many agents are loaded.
func (r *Registry) Len() int { return len(r.order) }

// LoadRegistry reads every *.md file from each directory in order. A missing
// directory is skipped, since global and project scopes are both optional.
func LoadRegistry(dirs []string) (*Registry, error) {
	registry := NewRegistry()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read agent dir %s: %w", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)

		for _, name := range names {
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			def, err := ParseDefinition(path, raw)
			if err != nil {
				return nil, err
			}
			registry.Add(def)
		}
	}
	return registry, nil
}
