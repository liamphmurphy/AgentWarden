// Package skill discovers SKILL.md files and renders them into a prompt.
package skill

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the conventional skill filename inside a skill directory.
const FileName = "SKILL.md"

// Skill is one loaded skill. The frontmatter is intentionally minimal —
// name and description — matching the existing skill files so they load as-is.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Body is the markdown content injected when the skill is active.
	Body string `yaml:"-"`
	Path string `yaml:"-"`
}

// Parse reads one skill document.
func Parse(path string, raw []byte) (*Skill, error) {
	frontmatter, body := splitFrontmatter(raw)

	s := &Skill{Path: path}
	if len(frontmatter) > 0 {
		if err := yaml.Unmarshal(frontmatter, s); err != nil {
			return nil, fmt.Errorf("%s: parse frontmatter: %w", path, err)
		}
	}
	s.Body = strings.TrimSpace(string(body))
	if s.Name == "" {
		// Fall back to the containing directory, which is how skills are
		// conventionally named.
		s.Name = filepath.Base(filepath.Dir(path))
	}
	if s.Name == "" || s.Name == "." {
		return nil, fmt.Errorf("%s: skill has no name", path)
	}
	return s, nil
}

func splitFrontmatter(raw []byte) (frontmatter, body []byte) {
	const delim = "---"
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(delim)) {
		return nil, raw
	}
	rest := trimmed[len(delim):]
	if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
		rest = rest[idx+1:]
	} else {
		return nil, raw
	}
	lines := bytes.Split(rest, []byte("\n"))
	for i, line := range lines {
		if strings.TrimRight(string(line), " \t\r") == delim {
			return bytes.Join(lines[:i], []byte("\n")), bytes.Join(lines[i+1:], []byte("\n"))
		}
	}
	return nil, raw
}

// Set holds the discovered skills.
type Set struct {
	byName map[string]*Skill
}

// NewSet returns an empty Set.
func NewSet() *Set { return &Set{byName: map[string]*Skill{}} }

// Add registers a skill, replacing an earlier one of the same name so a
// project skill can shadow a global one.
func (s *Set) Add(skill *Skill) { s.byName[skill.Name] = skill }

// Get returns a skill by name.
func (s *Set) Get(name string) (*Skill, bool) {
	skill, ok := s.byName[name]
	return skill, ok
}

// Names lists every skill name, sorted.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.byName))
	for name := range s.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Len reports how many skills are loaded.
func (s *Set) Len() int { return len(s.byName) }

// Resolve returns the named skills, reporting any that are missing.
//
// A missing skill is surfaced rather than ignored: the existing OpenCode
// config references a skill (`architecture`) that has no directory, and
// silently dropping it hides the typo.
func (s *Set) Resolve(names []string) (found []*Skill, missing []string) {
	for _, name := range names {
		if skill, ok := s.byName[name]; ok {
			found = append(found, skill)
			continue
		}
		missing = append(missing, name)
	}
	return found, missing
}

// Prompt renders skills into a single prompt section.
func Prompt(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skills\n\nThe following reference material applies to this task.\n")
	for _, skill := range skills {
		fmt.Fprintf(&b, "\n## %s\n\n", skill.Name)
		if skill.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", skill.Description)
		}
		b.WriteString(skill.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// Load discovers skills from each directory in order.
//
// Both layouts are accepted: a directory per skill containing SKILL.md (the
// current convention) and loose *.md files.
func Load(dirs []string) (*Set, error) {
	set := NewSet()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read skill dir %s: %w", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		sort.Strings(names)

		for _, name := range names {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}

			skillPath := path
			if info.IsDir() {
				skillPath = filepath.Join(path, FileName)
				if _, err := os.Stat(skillPath); err != nil {
					continue
				}
			} else if !strings.EqualFold(filepath.Ext(name), ".md") {
				continue
			}

			raw, err := os.ReadFile(skillPath)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", skillPath, err)
			}
			skill, err := Parse(skillPath, raw)
			if err != nil {
				return nil, err
			}
			set.Add(skill)
		}
	}
	return set, nil
}
