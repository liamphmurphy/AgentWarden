// Package tool defines the tool contract and the built-in tool set.
package tool

import (
	"context"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// Call is a request to run a tool, as emitted by the model.
type Call struct {
	ID   string
	Name string
	// Args is the raw JSON argument object.
	Args string
}

// Result is the outcome of a tool execution, fed back to the model.
type Result struct {
	Content string
	// IsError marks a failure. Blocked calls are returned as errors so the
	// model observes the refusal in-band rather than silently losing a turn.
	IsError bool
}

// Tool is one executable capability.
type Tool interface {
	Def() provider.ToolDef
	Run(ctx context.Context, call Call) (Result, error)
}

// Registry holds the tools available to a session, in a stable order so that
// masked tool lists are deterministic and diffable in request logs.
type Registry struct {
	order []string
	tools map[string]Tool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Add registers a tool, replacing any tool of the same name while keeping the
// original position.
func (r *Registry) Add(t Tool) {
	name := t.Def().Name
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Defs returns every tool definition in registration order.
func (r *Registry) Defs() []provider.ToolDef {
	out := make([]provider.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].Def())
	}
	return out
}

// Names returns every tool name in registration order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}
