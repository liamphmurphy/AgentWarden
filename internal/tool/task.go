package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// Delegation is a validated request to hand work to a subagent.
type Delegation struct {
	Subagent           string   `json:"subagent"`
	Objective          string   `json:"objective"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	FilesInScope       []string `json:"files_in_scope"`
}

// DelegateFunc runs a subagent and returns its report.
type DelegateFunc func(ctx context.Context, d Delegation) (string, error)

// Task delegates work to a subagent.
//
// The schema deliberately demands more than a prompt string: requiring an
// objective, acceptance criteria and a file scope forces even a small model to
// decompose the work rather than forwarding "fix it".
type Task struct {
	// Agents lists the subagents that may be targeted, surfaced in the schema
	// so the model picks from a closed set.
	Agents   []string
	Delegate DelegateFunc
}

func (t Task) Def() provider.ToolDef {
	subagent := map[string]any{
		"type":        "string",
		"description": "The subagent to delegate to.",
	}
	if len(t.Agents) > 0 {
		subagent["enum"] = t.Agents
	}
	return provider.ToolDef{
		Name:        "task",
		Description: "Delegate a self-contained piece of work to a subagent.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subagent":  subagent,
				"objective": map[string]any{"type": "string", "description": "Concretely, what the subagent must accomplish."},
				"acceptance_criteria": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Observable criteria that decide whether the work is done.",
				},
				"files_in_scope": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
			},
			"required": []string{"subagent", "objective", "acceptance_criteria"},
		},
	}
}

func (t Task) Run(ctx context.Context, call Call) (Result, error) {
	var d Delegation
	if err := json.Unmarshal([]byte(call.Args), &d); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if t.Delegate == nil {
		return errResult("delegation is not available in this session")
	}
	if len(t.Agents) > 0 {
		known := false
		for _, agent := range t.Agents {
			if agent == d.Subagent {
				known = true
				break
			}
		}
		if !known {
			return errResult("unknown subagent %q; available: %v", d.Subagent, t.Agents)
		}
	}

	report, err := t.Delegate(ctx, d)
	if err != nil {
		return errResult("subagent %s failed: %v", d.Subagent, err)
	}
	return Result{Content: fmt.Sprintf("%s reported:\n\n%s", d.Subagent, report)}, nil
}
