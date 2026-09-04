package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// Bash execution limits.
const (
	defaultBashTimeout = 2 * time.Minute
	maxBashOutput      = 64 * 1024
)

// Bash runs a shell command.
//
// Unlike a gate, an agent command is genuinely shell-shaped (pipes, globs), so
// it runs through `sh -c`. The enforcer inspects the parsed argv beforehand to
// block publication, which is why Argv is exposed.
type Bash struct {
	Root    string
	Timeout time.Duration
}

func (Bash) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        "bash",
		Description: "Run a shell command in the project directory.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Optional timeout; defaults to 120 seconds.",
				},
			},
			"required": []string{"command"},
		},
	}
}

// bashArgs is the decoded argument shape.
type bashArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (t Bash) Run(ctx context.Context, call Call) (Result, error) {
	var args bashArgs
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return errResult("command is required")
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = defaultBashTimeout
	}
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", args.Command)
	cmd.Dir = t.Root
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	output := combined.String()
	if len(output) > maxBashOutput {
		output = output[:maxBashOutput] + fmt.Sprintf("\n... [truncated at %d bytes]", maxBashOutput)
	}
	if output == "" {
		output = "(no output)"
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return Result{
			Content: fmt.Sprintf("command timed out after %s\n%s", timeout, output),
			IsError: true,
		}, nil
	}
	if err != nil {
		// A non-zero exit is information for the model, not a runtime failure,
		// so it comes back as an error result rather than a Go error.
		return Result{
			Content: fmt.Sprintf("exit status %d\n%s", cmd.ProcessState.ExitCode(), output),
			IsError: true,
		}, nil
	}
	return Result{Content: output}, nil
}

// Argv parses the command out of a bash call's arguments so a caller can
// inspect what is about to run.
func Argv(args string) ([]string, error) {
	var parsed bashArgs
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return nil, err
	}
	if parsed.Command == "" {
		return nil, fmt.Errorf("no command")
	}
	return strings.Fields(parsed.Command), nil
}
