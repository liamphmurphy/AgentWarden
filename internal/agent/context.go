package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InstructionsFileName is the conventional project-level agent instructions
// file. It is loaded separately from role definitions because its guidance
// applies to every agent working in the project.
const InstructionsFileName = "AGENTS.md"

// LoadProjectInstructions loads the project-level agent instructions.
//
// A missing file is expected: projects do not have to provide extra guidance.
// Other read failures are returned so a permissions or filesystem problem
// cannot silently remove instructions the user expected the agent to follow.
func LoadProjectInstructions(project string) (string, error) {
	path := filepath.Join(project, InstructionsFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(raw)), nil
}
