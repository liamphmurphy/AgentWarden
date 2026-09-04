package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// scopes builds a home and project pair with optional config files.
func scopes(t *testing.T, global, project string) (homeDir, projectDir string) {
	t.Helper()
	root := t.TempDir()
	homeDir = filepath.Join(root, "home")
	projectDir = filepath.Join(root, "project")
	if global != "" {
		writeFile(t, filepath.Join(homeDir, GlobalDir, FileName), global)
	}
	if project != "" {
		writeFile(t, filepath.Join(projectDir, ProjectDir, FileName), project)
	}
	return homeDir, projectDir
}

func TestLoadMissingConfigReturnsDefault(t *testing.T) {
	home, project := scopes(t, "", "")
	cfg, err := Load(home, project)
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
	if cfg == nil || len(cfg.Providers) == 0 {
		t.Fatal("a usable default should still be returned")
	}
	if cfg.Workflow.Enabled {
		t.Error("governance should be off by default")
	}
}

// TestJSONCTolerance matters because the config being migrated from is not
// strict JSON: it has trailing commas after each model block.
func TestJSONCTolerance(t *testing.T) {
	home, project := scopes(t, `{
  // a line comment
  "providers": {
    "ollama": {
      "baseUrl": "http://127.0.0.1:11434/v1",
      "models": {
        "qwen3.5": { "name": "qwen3.5" },   // trailing comma next
      },
    },
  },
  /* a block comment
     spanning lines */
  "defaultAgent": "orchestrator",
}`, "")

	cfg, err := Load(home, project)
	if err != nil {
		t.Fatalf("JSONC should parse: %v", err)
	}
	if cfg.DefaultAgent != "orchestrator" {
		t.Errorf("defaultAgent = %q", cfg.DefaultAgent)
	}
	if _, ok := cfg.Providers["ollama"].Models["qwen3.5"]; !ok {
		t.Error("model should survive the trailing comma")
	}
}

func TestStripJSONCPreservesStringContents(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"url with slashes", `{"a":"http://x/y"}`, `{"a":"http://x/y"}`},
		{"comment marker in string", `{"a":"not // a comment"}`, `{"a":"not // a comment"}`},
		{"block marker in string", `{"a":"not /* a comment */"}`, `{"a":"not /* a comment */"}`},
		{"escaped quote", `{"a":"say \"hi\" // x"}`, `{"a":"say \"hi\" // x"}`},
		{"line comment removed", "{\"a\":1 // note\n}", "{\"a\":1 \n}"},
		{"trailing comma in array", `{"a":[1,2,]}`, `{"a":[1,2]}`},
		{"comma before comment then brace", "{\"a\":1, // note\n}", "{\"a\":1 \n}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(StripJSONC([]byte(tc.input))); got != tc.want {
				t.Errorf("StripJSONC(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestProjectOverridesGlobal(t *testing.T) {
	home, project := scopes(t, `{
  "providers": { "ollama": { "baseUrl": "http://global:11434/v1" } },
  "defaultAgent": "global-agent",
  "workflow": { "enabled": false }
}`, `{
  "defaultAgent": "project-agent",
  "workflow": { "enabled": true, "policy": ".agentwarden/workflow.yml" }
}`)

	cfg, err := Load(home, project)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultAgent != "project-agent" {
		t.Errorf("project should win: %q", cfg.DefaultAgent)
	}
	// A key the project file never mentions must survive from global scope.
	if got := cfg.Providers["ollama"].BaseURL; got != "http://global:11434/v1" {
		t.Errorf("global provider should survive: %q", got)
	}
	if !cfg.Workflow.Enabled {
		t.Error("project should be able to switch governance on")
	}
	if len(cfg.Sources()) != 2 {
		t.Errorf("want 2 sources, got %v", cfg.Sources())
	}
}

func TestProjectAddsProviderWithoutRestatingGlobals(t *testing.T) {
	home, project := scopes(t, `{
  "providers": { "ollama": { "baseUrl": "http://127.0.0.1:11434/v1" } }
}`, `{
  "providers": { "vllm": { "baseUrl": "http://127.0.0.1:8000/v1" } }
}`)

	cfg, err := Load(home, project)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, id := range []string{"ollama", "vllm"} {
		if _, ok := cfg.Providers[id]; !ok {
			t.Errorf("provider %s should be present", id)
		}
	}
}

// TestEnvInterpolation is how a gateway key stays out of the config file.
func TestEnvInterpolation(t *testing.T) {
	t.Setenv("AGENTWARDEN_TEST_KEY", "secret-value")
	home, project := scopes(t, `{
  "providers": {
    "gateway": {
      "baseUrl": "https://gw.example.com/v1",
      "headers": { "x-api-key": "${AGENTWARDEN_TEST_KEY}" }
    }
  }
}`, "")

	cfg, err := Load(home, project)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Providers["gateway"].Headers["x-api-key"]; got != "secret-value" {
		t.Errorf("header = %q, want the interpolated value", got)
	}
}

// TestMissingEnvVarIsAnError: a missing credential should fail at startup, not
// surface later as a puzzling 401.
func TestMissingEnvVarIsAnError(t *testing.T) {
	home, project := scopes(t, `{
  "providers": {
    "gateway": {
      "baseUrl": "https://gw.example.com/v1",
      "headers": { "x-api-key": "${AGENTWARDEN_DEFINITELY_UNSET_VAR}" }
    }
  }
}`, "")

	if _, err := Load(home, project); err == nil {
		t.Fatal("an unset variable should be an error")
	} else if !strings.Contains(err.Error(), "AGENTWARDEN_DEFINITELY_UNSET_VAR") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestExpand(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "SET":
			return "value", true
		case "EMPTY":
			return "", true
		default:
			return "", false
		}
	}

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"no reference", "plain text", "plain text", false},
		{"simple", "${SET}", "value", false},
		{"embedded", "a-${SET}-b", "a-value-b", false},
		{"two references", "${SET}/${SET}", "value/value", false},
		{"default used when unset", "${UNSET:-fallback}", "fallback", false},
		{"default ignored when set", "${SET:-fallback}", "value", false},
		{"empty value takes default", "${EMPTY:-fallback}", "fallback", false},
		{"empty default", "${UNSET:-}", "", false},
		{"bare dollar left alone", "$SET and $$", "$SET and $$", false},
		{"unterminated is literal", "${SET", "${SET", false},
		{"unset without default", "${UNSET}", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.input, lookup)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if got != tc.want {
				t.Errorf("Expand(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestEnvFileSuppliesValues(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, ".env")
	writeFile(t, envPath, `
# a comment
export QUOTED="quoted-value"
PLAIN=plain-value
SINGLE='single-value'
`)

	home, project := scopes(t, `{
  "envFile": "`+envPath+`",
  "providers": {
    "gateway": {
      "baseUrl": "https://gw.example.com/v1",
      "headers": {
        "a": "${QUOTED}",
        "b": "${PLAIN}",
        "c": "${SINGLE}"
      }
    }
  }
}`, "")

	cfg, err := Load(home, project)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	headers := cfg.Providers["gateway"].Headers
	for key, want := range map[string]string{"a": "quoted-value", "b": "plain-value", "c": "single-value"} {
		if headers[key] != want {
			t.Errorf("header %s = %q, want %q", key, headers[key], want)
		}
	}
}

func TestResolveModel(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"ollama": {BaseURL: "x", Models: map[string]Model{"qwen3.5": {Name: "Qwen"}}},
		"bare":   {BaseURL: "y"},
	}}

	t.Run("declared model", func(t *testing.T) {
		id, model, err := cfg.ResolveModel("ollama/qwen3.5")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if id != "ollama" || model.ModelID != "qwen3.5" || model.Name != "Qwen" {
			t.Errorf("got %s / %+v", id, model)
		}
	})

	// A local endpoint often serves whatever has been pulled, so a provider
	// with no declared models accepts any name.
	t.Run("undeclared model on bare provider", func(t *testing.T) {
		_, model, err := cfg.ResolveModel("bare/anything")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if model.ModelID != "anything" {
			t.Errorf("ModelID = %q", model.ModelID)
		}
	})

	for _, ref := range []string{"noslash", "unknown/model", "ollama/not-declared"} {
		t.Run("rejects "+ref, func(t *testing.T) {
			if _, _, err := cfg.ResolveModel(ref); err == nil {
				t.Errorf("%q should be rejected", ref)
			}
		})
	}
}

func TestValidateRejectsIncompleteAutoRouting(t *testing.T) {
	base := map[string]Provider{"ollama": {BaseURL: "x", Models: map[string]Model{"small": {}}}}

	tests := []struct {
		name    string
		routing Routing
		wantErr bool
	}{
		{"fixed needs nothing", Routing{Mode: RoutingFixed}, false},
		{"auto without models", Routing{Mode: RoutingAuto}, true},
		{"auto without large", Routing{Mode: RoutingAuto, Small: "ollama/small"}, true},
		{"auto with unknown model", Routing{Mode: RoutingAuto, Small: "ollama/small", Large: "ollama/nope"}, true},
		{"auto complete", Routing{Mode: RoutingAuto, Small: "ollama/small", Large: "ollama/small"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Providers: base, Routing: tc.routing}
			err := cfg.Validate()
			if tc.wantErr != (err != nil) {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRoutingEscalates(t *testing.T) {
	r := Routing{EscalateOn: []string{EscalateOnViolation, EscalateOnLongContext}}
	if !r.Escalates(EscalateOnViolation) || !r.Escalates(EscalateOnLongContext) {
		t.Error("declared triggers should be active")
	}
	if r.Escalates(EscalateOnToolFailure) {
		t.Error("undeclared triggers should be inactive")
	}
}

func TestValidateRequiresProviderBaseURL(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"broken": {}}}
	if err := cfg.Validate(); err == nil {
		t.Error("a provider without a baseUrl should be rejected")
	}
	if err := (&Config{}).Validate(); err == nil {
		t.Error("no providers at all should be rejected")
	}
}

func TestPolicyPathResolution(t *testing.T) {
	project := "/tmp/proj"
	tests := []struct {
		name   string
		policy string
		want   string
	}{
		{"default", "", "/tmp/proj/.agentwarden/workflow.yml"},
		{"relative", ".agentwarden/custom.yml", "/tmp/proj/.agentwarden/custom.yml"},
		{"absolute", "/etc/agentwarden/workflow.yml", "/etc/agentwarden/workflow.yml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Workflow: WorkflowConfig{Policy: tc.policy}}
			if got := cfg.PolicyPath(project); got != tc.want {
				t.Errorf("PolicyPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpandDirs(t *testing.T) {
	got := ExpandDirs([]string{
		"~/custom/agents",
		"/abs/agents",
		".agentwarden/agent",
		".config/agentwarden/agent",
	}, "/home/u", "/proj")

	want := []string{
		"/home/u/custom/agents",
		"/abs/agents",
		"/proj/.agentwarden/agent",
		"/home/u/.config/agentwarden/agent",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestModelToolsSupportedDefaultsTrue(t *testing.T) {
	if !(Model{}).ToolsSupported() {
		t.Error("tool calling should be assumed available")
	}
	no := false
	if (Model{SupportsTools: &no}).ToolsSupported() {
		t.Error("an explicit false should be honored")
	}
}

func TestMalformedConfigIsAnError(t *testing.T) {
	home, project := scopes(t, `{"providers": {`, "")
	if _, err := Load(home, project); err == nil {
		t.Error("malformed JSON should be an error, unlike a missing file")
	}
}

// TestEnvFilePathForms covers the path shapes a real config uses: a portable
// ~ reference, an absolute path, and one relative to the project.
func TestEnvFilePathForms(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "project")

	writeFile(t, filepath.Join(home, "secrets.env"), "TOKEN=from-home\n")
	writeFile(t, filepath.Join(project, "local.env"), "TOKEN=from-project\n")
	absolute := filepath.Join(root, "abs.env")
	writeFile(t, absolute, "TOKEN=from-absolute\n")

	tests := []struct {
		name    string
		envFile string
		want    string
	}{
		{"tilde", "~/secrets.env", "from-home"},
		{"absolute", absolute, "from-absolute"},
		{"project relative", "local.env", "from-project"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, filepath.Join(home, GlobalDir, FileName), `{
  "envFile": "`+tc.envFile+`",
  "providers": {
    "p": { "baseUrl": "https://x/v1", "headers": { "auth": "${TOKEN}" } }
  }
}`)
			cfg, err := Load(home, project)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Providers["p"].Headers["auth"]; got != tc.want {
				t.Errorf("auth = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMissingEnvFileIsAnError(t *testing.T) {
	home, project := scopes(t, `{
  "envFile": "~/definitely-absent.env",
  "providers": { "p": { "baseUrl": "https://x/v1" } }
}`, "")
	if _, err := Load(home, project); err == nil {
		t.Error("a configured but absent envFile should be an error, not silence")
	}
}
