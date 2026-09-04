package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRealSkillFiles uses the actual skill files from the OpenCode config,
// so discovery is proven against real data.
func TestLoadRealSkillFiles(t *testing.T) {
	set, err := Load([]string{"testdata"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("want 2 skills, got %d (%v)", set.Len(), set.Names())
	}

	goStyle, ok := set.Get("go-style")
	if !ok {
		t.Fatal("go-style should be discovered")
	}
	if goStyle.Description == "" {
		t.Error("description should be parsed")
	}
	if !strings.Contains(goStyle.Body, "#") {
		t.Error("the markdown body should be retained")
	}

	// The one skill using a folded scalar description.
	makePlatform, ok := set.Get("make-platform")
	if !ok {
		t.Fatal("make-platform should be discovered")
	}
	if strings.Contains(makePlatform.Description, "\n") {
		t.Error("a folded description should not retain newlines")
	}
	if !strings.Contains(makePlatform.Description, "customer data platform") {
		t.Errorf("description = %q", makePlatform.Description)
	}
}

func TestLoadAcceptsBothLayouts(t *testing.T) {
	dir := t.TempDir()

	// Directory-per-skill, the current convention.
	os.MkdirAll(filepath.Join(dir, "nested"), 0o755)
	os.WriteFile(filepath.Join(dir, "nested", FileName),
		[]byte("---\nname: nested\ndescription: d\n---\nbody"), 0o644)
	// A loose markdown file.
	os.WriteFile(filepath.Join(dir, "loose.md"),
		[]byte("---\nname: loose\ndescription: d\n---\nbody"), 0o644)
	// A directory with no SKILL.md is skipped rather than failing the load.
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)

	set, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"nested", "loose"} {
		if _, ok := set.Get(name); !ok {
			t.Errorf("%s should be discovered", name)
		}
	}
	if set.Len() != 2 {
		t.Errorf("want 2 skills, got %v", set.Names())
	}
}

func TestNameFallsBackToDirectory(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "implied-name"), 0o755)
	os.WriteFile(filepath.Join(dir, "implied-name", FileName),
		[]byte("---\ndescription: no name field\n---\nbody"), 0o644)

	set, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Get("implied-name"); !ok {
		t.Errorf("name should fall back to the directory, got %v", set.Names())
	}
}

func TestProjectSkillShadowsGlobal(t *testing.T) {
	global, project := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(global, "s.md"), []byte("---\nname: s\ndescription: global\n---\nb"), 0o644)
	os.WriteFile(filepath.Join(project, "s.md"), []byte("---\nname: s\ndescription: project\n---\nb"), 0o644)

	set, err := Load([]string{global, project})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, _ := set.Get("s")
	if s.Description != "project" {
		t.Errorf("project should shadow global, got %q", s.Description)
	}
}

// TestResolveReportsMissing matters because the existing config references a
// skill (`architecture`) that has no directory; silently dropping it hides
// the typo.
func TestResolveReportsMissing(t *testing.T) {
	set := NewSet()
	set.Add(&Skill{Name: "go-style", Description: "d", Body: "b"})

	found, missing := set.Resolve([]string{"go-style", "architecture", "patterns"})
	if len(found) != 1 || found[0].Name != "go-style" {
		t.Errorf("found = %+v", found)
	}
	if len(missing) != 2 {
		t.Fatalf("missing = %v, want two entries", missing)
	}
	if missing[0] != "architecture" || missing[1] != "patterns" {
		t.Errorf("missing = %v", missing)
	}
}

func TestPromptRendersSkills(t *testing.T) {
	skills := []*Skill{
		{Name: "go-style", Description: "Go conventions", Body: "## Formatting\nUse gofmt."},
		{Name: "patterns", Body: "## Patterns\nPrefer composition."},
	}
	prompt := Prompt(skills)

	for _, want := range []string{"# Skills", "go-style", "Go conventions", "Use gofmt", "patterns", "Prefer composition"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should contain %q:\n%s", want, prompt)
		}
	}
	if Prompt(nil) != "" {
		t.Error("no skills should render nothing")
	}
}

func TestLoadMissingDirectoryIsNotAnError(t *testing.T) {
	set, err := Load([]string{filepath.Join(t.TempDir(), "does-not-exist")})
	if err != nil {
		t.Fatalf("a missing skill dir should be skipped: %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("want an empty set, got %v", set.Names())
	}
}

func TestParseWithoutFrontmatter(t *testing.T) {
	s, err := Parse(filepath.Join("dir", "some-skill", FileName), []byte("Just a body."))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name != "some-skill" {
		t.Errorf("name = %q, want the directory name", s.Name)
	}
	if s.Body != "Just a body." {
		t.Errorf("body = %q", s.Body)
	}
}
