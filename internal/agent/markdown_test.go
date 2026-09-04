package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lmurphy/agentwarden/internal/enforce"
)

func parseFixture(t *testing.T, name string) *Definition {
	t.Helper()
	path := filepath.Join("testdata", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	def, err := ParseDefinition(path, raw)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return def
}

// TestParseRealAgentFiles uses the actual agent files from the OpenCode config
// as fixtures, so the loader is proven against real data rather than a
// hand-written approximation.
func TestParseRealAgentFiles(t *testing.T) {
	t.Run("orchestrator is primary and fully restricted", func(t *testing.T) {
		def := parseFixture(t, "orchestrator.md")
		if def.Name != "orchestrator" {
			t.Errorf("name = %q", def.Name)
		}
		if def.Mode != ModePrimary {
			t.Errorf("mode = %q, want primary", def.Mode)
		}
		if !def.IsPrimary() {
			t.Error("orchestrator should be able to lead a session")
		}
		if def.Description == "" {
			t.Error("description should be parsed")
		}
		if def.Prompt == "" {
			t.Error("the markdown body should become the prompt")
		}
		// Every effect in the file must be recognized.
		if len(def.Permissions) == 0 {
			t.Fatal("permissions should be parsed")
		}
		for _, rule := range def.Permissions {
			if rule.Effect != enforce.EffectDeny {
				t.Errorf("orchestrator rule %+v should deny", rule)
			}
		}
	})

	t.Run("qa-engineer uses ordered narrowing rules", func(t *testing.T) {
		def := parseFixture(t, "qa-engineer.md")
		if def.Mode != ModeSubagent {
			t.Errorf("mode = %q, want subagent", def.Mode)
		}
		// The file denies shell broadly then re-allows inspection commands;
		// later rules must win, which is what the permission layer relies on.
		perms := enforce.NewPermissions(def.Permissions, false)
		if got := perms.Evaluate(enforce.ActionShell, "git diff HEAD"); got != enforce.EffectAllow {
			t.Errorf("git diff should be allowed, got %s", got)
		}
		if got := perms.Evaluate(enforce.ActionShell, "rm -rf /"); got != enforce.EffectDeny {
			t.Errorf("arbitrary shell should be denied, got %s", got)
		}
		if got := perms.Evaluate(enforce.ActionEdit, "src/main.go"); got != enforce.EffectDeny {
			t.Errorf("edits should be denied for QA, got %s", got)
		}
	})

	t.Run("engineer can write", func(t *testing.T) {
		def := parseFixture(t, "engineer.md")
		perms := enforce.NewPermissions(def.Permissions, false)
		if got := perms.Evaluate(enforce.ActionEdit, "src/main.go"); got != enforce.EffectAllow {
			t.Errorf("engineer should be allowed to edit, got %s", got)
		}
		if got := perms.Evaluate(enforce.ActionSubagent, "anything"); got != enforce.EffectDeny {
			t.Errorf("engineer should not delegate onward, got %s", got)
		}
	})
}

// TestParseLegacyAgentFiles covers the older frontmatter generation, which
// uses a tools boolean map and a skills list instead of permission rules.
func TestParseLegacyAgentFiles(t *testing.T) {
	def := parseFixture(t, "architect-legacy.md")

	if def.Name != "architect" {
		t.Errorf("name = %q", def.Name)
	}
	// A folded description should come through as one line.
	if !strings.Contains(def.Description, "Design and scoping subagent") {
		t.Errorf("folded description not parsed: %q", def.Description)
	}
	if strings.Contains(def.Description, "\n") {
		t.Error("a folded scalar should not retain newlines")
	}
	if len(def.Skills) != 4 {
		t.Errorf("want 4 skills, got %v", def.Skills)
	}

	// The tools map is honored: this agent reads but must not write.
	if !def.AllowsTool("read") || !def.AllowsTool("bash") {
		t.Error("read and bash should be allowed")
	}
	if def.AllowsTool("edit") || def.AllowsTool("write") || def.AllowsTool("task") {
		t.Error("edit, write and task should be refused by the tools map")
	}
	// An unmentioned tool is left to the enforcer rather than denied here.
	if !def.AllowsTool("some_new_tool") {
		t.Error("an unmentioned tool should not be denied by the tools map")
	}
}

func TestParseFrontmatterVariants(t *testing.T) {
	tests := []struct {
		name    string
		content string
		verify  func(t *testing.T, def *Definition)
	}{
		{
			name:    "no frontmatter at all",
			content: "Just a prompt body.",
			verify: func(t *testing.T, def *Definition) {
				if def.Prompt != "Just a prompt body." {
					t.Errorf("prompt = %q", def.Prompt)
				}
				if def.Name != "plain" {
					t.Errorf("name should fall back to the filename, got %q", def.Name)
				}
				if def.Mode != ModeSubagent {
					t.Errorf("mode should default to subagent, got %q", def.Mode)
				}
			},
		},
		{
			name:    "empty frontmatter",
			content: "---\n---\nBody here.",
			verify: func(t *testing.T, def *Definition) {
				if def.Prompt != "Body here." {
					t.Errorf("prompt = %q", def.Prompt)
				}
			},
		},
		{
			name:    "unterminated frontmatter is treated as body",
			content: "---\nname: x\nno closing delimiter",
			verify: func(t *testing.T, def *Definition) {
				// Better to keep the content than silently drop the file.
				if !strings.Contains(def.Prompt, "no closing delimiter") {
					t.Errorf("prompt = %q", def.Prompt)
				}
				if def.Name != "plain" {
					t.Errorf("name = %q, want the filename fallback", def.Name)
				}
			},
		},
		{
			name:    "mode all is both",
			content: "---\nmode: all\n---\nbody",
			verify: func(t *testing.T, def *Definition) {
				if !def.IsPrimary() || !def.IsSubagent() {
					t.Error("mode all should be usable either way")
				}
			},
		},
		{
			name:    "model override",
			content: "---\nmodel: ollama/qwen3.5\n---\nbody",
			verify: func(t *testing.T, def *Definition) {
				if def.Model != "ollama/qwen3.5" {
					t.Errorf("model = %q", def.Model)
				}
			},
		},
		{
			name:    "windows line endings",
			content: "---\r\nname: winagent\r\nmode: primary\r\n---\r\nbody\r\n",
			verify: func(t *testing.T, def *Definition) {
				if def.Name != "winagent" {
					t.Errorf("name = %q, CRLF frontmatter should parse", def.Name)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, err := ParseDefinition("plain.md", []byte(tc.content))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tc.verify(t, def)
		})
	}
}

func TestParseRejectsInvalidFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"unknown mode", "---\nmode: sideways\n---\nbody"},
		{"unknown permission effect", "---\npermissions:\n  - {action: edit, resource: \"*\", effect: maybe}\n---\nbody"},
		{"malformed yaml", "---\nname: [unclosed\n---\nbody"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDefinition("bad.md", []byte(tc.content)); err == nil {
				t.Error("expected a parse error")
			}
		})
	}
}

func TestLoadProjectInstructions(t *testing.T) {
	t.Run("missing file is optional", func(t *testing.T) {
		got, err := LoadProjectInstructions(t.TempDir())
		if err != nil {
			t.Fatalf("LoadProjectInstructions: %v", err)
		}
		if got != "" {
			t.Errorf("instructions = %q, want empty", got)
		}
	})

	t.Run("loads and trims project file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, InstructionsFileName)
		if err := os.WriteFile(path, []byte("\n# Local rules\n\nKeep the change small.\n\n"), 0o644); err != nil {
			t.Fatalf("write instructions: %v", err)
		}

		got, err := LoadProjectInstructions(dir)
		if err != nil {
			t.Fatalf("LoadProjectInstructions: %v", err)
		}
		want := "# Local rules\n\nKeep the change small."
		if got != want {
			t.Errorf("instructions = %q, want %q", got, want)
		}
	})
}

func TestLoadRegistryPrecedence(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()

	os.WriteFile(filepath.Join(global, "engineer.md"),
		[]byte("---\ndescription: global\n---\nglobal prompt"), 0o644)
	os.WriteFile(filepath.Join(global, "only-global.md"),
		[]byte("---\ndescription: g\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(project, "engineer.md"),
		[]byte("---\ndescription: project\n---\nproject prompt"), 0o644)

	registry, err := LoadRegistry([]string{global, project, filepath.Join(project, "missing")})
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	if registry.Len() != 2 {
		t.Errorf("want 2 agents, got %d (%v)", registry.Len(), registry.Names())
	}
	// A project agent shadows the global one of the same name.
	def, ok := registry.Get("engineer")
	if !ok {
		t.Fatal("engineer should be present")
	}
	if def.Description != "project" {
		t.Errorf("project should win, got %q", def.Description)
	}
	if _, ok := registry.Get("only-global"); !ok {
		t.Error("a global-only agent should survive")
	}
}

func TestRegistrySubagentsFeedTheTaskEnum(t *testing.T) {
	registry := NewRegistry()
	registry.Add(&Definition{Name: "orchestrator", Mode: ModePrimary})
	registry.Add(&Definition{Name: "engineer", Mode: ModeSubagent})
	registry.Add(&Definition{Name: "reviewer", Mode: ModeAll})

	got := registry.Subagents()
	want := []string{"engineer", "reviewer"}
	if len(got) != len(want) {
		t.Fatalf("Subagents() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadRegistryIgnoresNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "agent.md"), []byte("---\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	registry, err := LoadRegistry([]string{dir})
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if registry.Len() != 1 {
		t.Errorf("only .md files should load, got %v", registry.Names())
	}
}

// TestShippedStarterAgentsParse guards the examples directory: a starter agent
// that does not load would be a bad first experience, and these are the files
// the README tells people to copy.
func TestShippedStarterAgentsParse(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "agent")
	registry, err := LoadRegistry([]string{dir})
	if err != nil {
		t.Fatalf("the shipped starter agents must load: %v", err)
	}

	want := map[string]Mode{
		"orchestrator": ModePrimary,
		"tech-lead":    ModeSubagent,
		"engineer":     ModeSubagent,
		"qa-engineer":  ModeSubagent,
	}
	if registry.Len() != len(want) {
		t.Fatalf("want %d starter agents, got %d (%v)", len(want), registry.Len(), registry.Names())
	}
	for name, mode := range want {
		def, ok := registry.Get(name)
		if !ok {
			t.Errorf("%s should be present", name)
			continue
		}
		if def.Mode != mode {
			t.Errorf("%s mode = %q, want %q", name, def.Mode, mode)
		}
		if def.Description == "" {
			t.Errorf("%s needs a description", name)
		}
		if def.Prompt == "" {
			t.Errorf("%s needs a prompt body", name)
		}
	}

	// The permission boundaries are the point of the starter set, so check
	// the ones that matter rather than just that they parse.
	engineer, _ := registry.Get("engineer")
	if enforce.NewPermissions(engineer.Permissions, false).
		Evaluate(enforce.ActionEdit, "main.go") != enforce.EffectAllow {
		t.Error("the engineer must be able to edit")
	}
	for _, name := range []string{"orchestrator", "tech-lead", "qa-engineer"} {
		def, _ := registry.Get(name)
		if enforce.NewPermissions(def.Permissions, false).
			Evaluate(enforce.ActionEdit, "main.go") != enforce.EffectDeny {
			t.Errorf("%s must not be able to edit", name)
		}
	}

	// QA needs git inspection but nothing else.
	qa, _ := registry.Get("qa-engineer")
	perms := enforce.NewPermissions(qa.Permissions, false)
	if perms.Evaluate(enforce.ActionShell, "git diff HEAD") != enforce.EffectAllow {
		t.Error("QA should be able to run git diff")
	}
	if perms.Evaluate(enforce.ActionShell, "go test ./...") != enforce.EffectDeny {
		t.Error("QA should not be able to run arbitrary commands")
	}
}
