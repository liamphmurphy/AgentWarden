// Package config loads agentwarden configuration: a JSONC-tolerant JSON file with
// ${VAR} interpolation, overlaid from global then project scope.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Well-known locations, relative to the home and project directories.
const (
	GlobalDir  = ".config/agentwarden"
	ProjectDir = ".agentwarden"
	FileName   = "agentwarden.json"
	PolicyName = "workflow.yml"
	StateDir   = "state"
)

// Model is one model offered by a provider.
type Model struct {
	// Name is the human label shown in the UI.
	Name string `json:"name,omitempty"`
	// ModelID is the identifier sent on the wire, defaulting to the map key.
	ModelID string `json:"modelID,omitempty"`
	// ContextWindow, when set, drives compaction decisions.
	ContextWindow int `json:"contextWindow,omitempty"`
	// SupportsTools is false for models that cannot do native tool calling,
	// which the enforcer needs to know because masking relies on it.
	SupportsTools *bool `json:"supportsTools,omitempty"`
}

// ToolsSupported reports whether native tool calling is available.
func (m Model) ToolsSupported() bool { return m.SupportsTools == nil || *m.SupportsTools }

// Provider is an OpenAI-compatible endpoint.
type Provider struct {
	Name    string            `json:"name,omitempty"`
	BaseURL string            `json:"baseUrl"`
	APIKey  string            `json:"apiKey,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Models  map[string]Model  `json:"models,omitempty"`
	// Extra is merged into every request body, for endpoint-specific knobs.
	Extra map[string]any `json:"extra,omitempty"`
}

// RoutingMode selects how a model is chosen per request.
type RoutingMode string

const (
	// RoutingFixed always uses the selected model.
	RoutingFixed RoutingMode = "fixed"
	// RoutingAuto escalates from a small local model to a large one when the
	// turn looks hard, keeping cheap work cheap.
	RoutingAuto RoutingMode = "auto"
)

// Escalation triggers for auto routing.
const (
	// EscalateOnViolation escalates after the enforcer blocks a call, on the
	// theory that a model failing to follow the workflow needs more capacity.
	EscalateOnViolation = "violation"
	// EscalateOnLongContext escalates once the conversation grows past the
	// small model's comfortable window.
	EscalateOnLongContext = "long_context"
	// EscalateOnToolFailure escalates after repeated malformed tool calls.
	EscalateOnToolFailure = "tool_failure"
)

// Routing configures automatic model selection.
type Routing struct {
	Mode RoutingMode `json:"mode,omitempty"`
	// Small and Large are "provider/model" references.
	Small string `json:"small,omitempty"`
	Large string `json:"large,omitempty"`
	// EscalateOn lists the triggers that switch from Small to Large.
	EscalateOn []string `json:"escalateOn,omitempty"`
	// LongContextTokens is the threshold for EscalateOnLongContext.
	LongContextTokens int `json:"longContextTokens,omitempty"`
}

// Escalates reports whether a trigger is enabled.
func (r Routing) Escalates(trigger string) bool {
	for _, t := range r.EscalateOn {
		if t == trigger {
			return true
		}
	}
	return false
}

// WorkflowConfig points at the policy and toggles governance.
type WorkflowConfig struct {
	// Enabled turns the enforcer on. When false the session is ungoverned,
	// which is also what --no-workflow and `agentwarden run` select.
	Enabled bool   `json:"enabled"`
	Policy  string `json:"policy,omitempty"`
}

// Config is the fully resolved configuration.
type Config struct {
	Providers    map[string]Provider `json:"providers"`
	DefaultAgent string              `json:"defaultAgent,omitempty"`
	DefaultModel string              `json:"defaultModel,omitempty"`
	AgentDirs    []string            `json:"agentDirs,omitempty"`
	SkillDirs    []string            `json:"skillDirs,omitempty"`
	Workflow     WorkflowConfig      `json:"workflow"`
	Routing      Routing             `json:"routing,omitempty"`
	// Auto pre-approves tool calls that would otherwise prompt. The --auto
	// flag sets it; an explicit deny rule still wins.
	Auto bool `json:"auto,omitempty"`
	// EnvFile supplies interpolation values without exporting them.
	EnvFile string `json:"envFile,omitempty"`

	// sources records which files contributed, for `agentwarden config`.
	sources []string
}

// Sources lists the config files that were loaded, in precedence order.
func (c *Config) Sources() []string { return append([]string(nil), c.sources...) }

// ErrNoConfig is returned when no config file exists in any scope.
var ErrNoConfig = errors.New("no agentwarden.json found")

// Default returns a usable configuration for a machine with no config file:
// a local Ollama endpoint and governance switched off.
func Default() *Config {
	return &Config{
		Providers: map[string]Provider{
			"ollama": {
				Name:    "Ollama",
				BaseURL: "http://127.0.0.1:11434/v1",
			},
		},
		AgentDirs: []string{
			filepath.Join(GlobalDir, "agent"),
			filepath.Join(ProjectDir, "agent"),
		},
		SkillDirs: []string{
			filepath.Join(GlobalDir, "skills"),
			filepath.Join(ProjectDir, "skills"),
		},
		Workflow: WorkflowConfig{Enabled: false, Policy: filepath.Join(ProjectDir, PolicyName)},
	}
}

// Paths returns the candidate config files in increasing precedence.
func Paths(home, project string) []string {
	return []string{
		filepath.Join(home, GlobalDir, FileName),
		filepath.Join(project, ProjectDir, FileName),
	}
}

// Load reads and merges the global then project config. A missing file is not
// an error; a malformed one is.
func Load(home, project string) (*Config, error) {
	cfg := Default()
	found := false

	for _, path := range Paths(home, project) {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		found = true
		if err := cfg.mergeFile(path, raw); err != nil {
			return nil, err
		}
		cfg.sources = append(cfg.sources, path)
	}

	if !found {
		return cfg, ErrNoConfig
	}
	if err := cfg.expand(home, project); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// mergeFile decodes one file over the accumulated config. Decoding into the
// existing struct means an absent key leaves the earlier value intact, so
// project config overrides only what it mentions.
func (c *Config) mergeFile(path string, raw []byte) error {
	stripped := StripJSONC(raw)

	// Provider maps merge per-key rather than wholesale, so a project file can
	// add an endpoint without restating the global ones.
	var probe struct {
		Providers map[string]Provider `json:"providers"`
	}
	_ = json.Unmarshal(stripped, &probe)

	if err := json.Unmarshal(stripped, c); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(probe.Providers) > 0 {
		merged := map[string]Provider{}
		for id, p := range c.Providers {
			merged[id] = p
		}
		for id, p := range probe.Providers {
			merged[id] = p
		}
		c.Providers = merged
	}
	return nil
}

// resolvePath expands ~ and makes a relative path absolute against project.
func resolvePath(path, home, project string) string {
	switch {
	case path == "":
		return ""
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	case filepath.IsAbs(path):
		return path
	default:
		return filepath.Join(project, path)
	}
}

// expand interpolates ${VAR} references across the fields that can hold them.
func (c *Config) expand(home, project string) error {
	overrides := map[string]string{}
	if c.EnvFile != "" {
		// The path is resolved here so a config can say ~/.config/agentwarden/.env
		// and remain portable between machines.
		c.EnvFile = resolvePath(c.EnvFile, home, project)
		loaded, err := LoadEnvFile(c.EnvFile)
		if err != nil {
			return fmt.Errorf("read envFile %s: %w", c.EnvFile, err)
		}
		overrides = loaded
	}
	lookup := chainLookup(overrides)

	for id, p := range c.Providers {
		expanded, err := Expand(p.BaseURL, lookup)
		if err != nil {
			return fmt.Errorf("provider %s baseUrl: %w", id, err)
		}
		p.BaseURL = expanded

		if p.APIKey != "" {
			expanded, err := Expand(p.APIKey, lookup)
			if err != nil {
				return fmt.Errorf("provider %s apiKey: %w", id, err)
			}
			p.APIKey = expanded
		}
		if len(p.Headers) > 0 {
			headers := make(map[string]string, len(p.Headers))
			for name, value := range p.Headers {
				expanded, err := Expand(value, lookup)
				if err != nil {
					return fmt.Errorf("provider %s header %s: %w", id, name, err)
				}
				headers[name] = expanded
			}
			p.Headers = headers
		}
		c.Providers[id] = p
	}
	return nil
}

// Validate checks internal consistency.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return errors.New("at least one provider must be configured")
	}
	for id, p := range c.Providers {
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s has no baseUrl", id)
		}
	}
	if c.DefaultModel != "" {
		if _, _, err := c.ResolveModel(c.DefaultModel); err != nil {
			return fmt.Errorf("defaultModel: %w", err)
		}
	}
	if c.Routing.Mode == RoutingAuto {
		if c.Routing.Small == "" || c.Routing.Large == "" {
			return errors.New("auto routing needs both a small and a large model")
		}
		for _, ref := range []string{c.Routing.Small, c.Routing.Large} {
			if _, _, err := c.ResolveModel(ref); err != nil {
				return fmt.Errorf("routing: %w", err)
			}
		}
	}
	return nil
}

// ResolveModel splits a "provider/model" reference and checks it exists. A
// provider with no declared models accepts any name, since a local endpoint
// often serves whatever has been pulled.
func (c *Config) ResolveModel(ref string) (providerID string, model Model, err error) {
	providerID, modelID, found := strings.Cut(ref, "/")
	if !found {
		return "", Model{}, fmt.Errorf("model reference %q must be provider/model", ref)
	}
	p, ok := c.Providers[providerID]
	if !ok {
		return "", Model{}, fmt.Errorf("unknown provider %q", providerID)
	}
	m, ok := p.Models[modelID]
	if !ok {
		if len(p.Models) > 0 {
			return "", Model{}, fmt.Errorf("provider %q has no model %q", providerID, modelID)
		}
		m = Model{}
	}
	if m.ModelID == "" {
		m.ModelID = modelID
	}
	if m.Name == "" {
		m.Name = modelID
	}
	return providerID, m, nil
}

// ModelRefs lists every configured provider/model pair, sorted.
//
// The order is stable because this drives the model picker: map iteration
// order would reshuffle the list between runs.
func (c *Config) ModelRefs() []string {
	var out []string
	for id, p := range c.Providers {
		for modelID := range p.Models {
			out = append(out, id+"/"+modelID)
		}
	}
	sort.Strings(out)
	return out
}

// DescribeModel renders a provider/model reference for display, preferring the
// configured labels over the raw identifiers.
func (c *Config) DescribeModel(ref string) string {
	providerID, model, err := c.ResolveModel(ref)
	if err != nil {
		return ref
	}
	providerName := c.Providers[providerID].Name
	if providerName == "" {
		providerName = providerID
	}
	if model.Name == "" || model.Name == model.ModelID {
		return providerName
	}
	return providerName + " — " + model.Name
}

// PolicyPath returns the resolved policy path.
func (c *Config) PolicyPath(project string) string {
	path := c.Workflow.Policy
	if path == "" {
		path = filepath.Join(ProjectDir, PolicyName)
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(project, path)
}

// StatePath returns the directory holding task records and the audit log.
func StatePath(project string) string {
	return filepath.Join(project, ProjectDir, StateDir)
}

// ExpandDirs resolves ~ and relative directory lists against home and project.
func ExpandDirs(dirs []string, home, project string) []string {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		switch {
		case strings.HasPrefix(dir, "~/"):
			out = append(out, filepath.Join(home, dir[2:]))
		case filepath.IsAbs(dir):
			out = append(out, dir)
		case strings.HasPrefix(dir, ProjectDir):
			out = append(out, filepath.Join(project, dir))
		default:
			// Anything else is interpreted relative to home, matching how the
			// default global agent and skill directories are written.
			out = append(out, filepath.Join(home, dir))
		}
	}
	return out
}
