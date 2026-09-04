package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// maxReadBytes bounds a single file read so one large file cannot blow the
// context window.
const maxReadBytes = 256 * 1024

// resolve turns a tool-supplied path into an absolute path inside root.
//
// Confining paths to the project root is what stops a model from reading
// ~/.ssh or writing outside the workspace, so every filesystem tool goes
// through here.
//
// Symlinks are resolved on both sides before comparing, using the deepest
// existing ancestor for the target: a file being created does not exist yet,
// and on macOS the root itself is often a symlink (/tmp -> /private/tmp), so
// resolving only one side reports every new file as an escape.
func resolve(root, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}

	rootClean := resolveExisting(filepath.Clean(root))

	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(rootClean, path)
	}
	abs = filepath.Clean(abs)

	rel, err := filepath.Rel(rootClean, resolveExisting(abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the project directory", path)
	}
	return abs, nil
}

// resolveExisting resolves symlinks for the deepest existing prefix of path,
// re-appending the components that do not exist yet.
func resolveExisting(path string) string {
	remainder := ""
	current := path
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root without finding anything that exists.
			return path
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// errResult renders a tool failure the model can act on.
func errResult(format string, args ...any) (Result, error) {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}, nil
}

// Read returns file contents.
type Read struct{ Root string }

func (Read) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        "read",
		Description: "Read the contents of a file.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the file, relative to the project root."},
			},
			"required": []string{"path"},
		},
	}
}

func (t Read) Run(_ context.Context, call Call) (Result, error) {
	var args struct{ Path string }
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	path, err := resolve(t.Root, args.Path)
	if err != nil {
		return errResult("%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return errResult("cannot read %s: %v", args.Path, err)
	}
	if info.IsDir() {
		return errResult("%s is a directory; use ls", args.Path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return errResult("cannot read %s: %v", args.Path, err)
	}
	truncated := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		truncated = true
	}
	content := string(data)
	if truncated {
		content += fmt.Sprintf("\n... [truncated at %d bytes]", maxReadBytes)
	}
	return Result{Content: content}, nil
}

// Write creates or replaces a file.
type Write struct{ Root string }

func (Write) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        "write",
		Description: "Write content to a file, creating or replacing it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (t Write) Run(_ context.Context, call Call) (Result, error) {
	var args struct {
		Path    string
		Content string
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	path, err := resolve(t.Root, args.Path)
	if err != nil {
		return errResult("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errResult("cannot create directory for %s: %v", args.Path, err)
	}
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return errResult("cannot write %s: %v", args.Path, err)
	}
	return Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path)}, nil
}

// Edit replaces an exact string in a file.
type Edit struct{ Root string }

func (Edit) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "edit",
		Description: "Replace an exact string in a file. " +
			"old_string must appear exactly once unless replace_all is true.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string"},
				"old_string":  map[string]any{"type": "string"},
				"new_string":  map[string]any{"type": "string"},
				"replace_all": map[string]any{"type": "boolean"},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

func (t Edit) Run(_ context.Context, call Call) (Result, error) {
	var args struct {
		Path       string
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if args.OldString == args.NewString {
		return errResult("old_string and new_string are identical")
	}
	path, err := resolve(t.Root, args.Path)
	if err != nil {
		return errResult("%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errResult("cannot read %s: %v", args.Path, err)
	}

	content := string(data)
	count := strings.Count(content, args.OldString)
	switch {
	case count == 0:
		return errResult("old_string not found in %s", args.Path)
	case count > 1 && !args.ReplaceAll:
		// Refusing an ambiguous edit prevents silently changing the wrong
		// occurrence.
		return errResult("old_string appears %d times in %s; pass replace_all or use a longer, unique string", count, args.Path)
	}

	updated := content
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(updated), mode); err != nil {
		return errResult("cannot write %s: %v", args.Path, err)
	}
	replaced := 1
	if args.ReplaceAll {
		replaced = count
	}
	return Result{Content: fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, args.Path)}, nil
}

// LS lists a directory.
type LS struct{ Root string }

func (LS) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        "ls",
		Description: "List the entries of a directory.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Directory path; defaults to the project root."},
			},
		},
	}
}

func (t LS) Run(_ context.Context, call Call) (Result, error) {
	var args struct{ Path string }
	if call.Args != "" {
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
			return errResult("invalid arguments: %v", err)
		}
	}
	if args.Path == "" {
		args.Path = "."
	}
	path, err := resolve(t.Root, args.Path)
	if err != nil {
		return errResult("%v", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errResult("cannot list %s: %v", args.Path, err)
	}

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return Result{Content: fmt.Sprintf("%s is empty", args.Path)}, nil
	}
	return Result{Content: strings.Join(lines, "\n")}, nil
}
