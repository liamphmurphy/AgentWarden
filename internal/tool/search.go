package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// Search limits keep a broad query from flooding the context window.
const (
	maxMatches = 200
	maxFiles   = 500
)

// skipDirs are never walked. Searching them wastes the budget and returns
// results the model cannot act on.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".gocache":     true,
	"dist":         true,
	"build":        true,
	"__pycache__":  true,
}

// Glob finds files by shell pattern.
type Glob struct{ Root string }

func (Glob) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        "glob",
		Description: "Find files matching a glob pattern, for example **/*.go.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t Glob) Run(_ context.Context, call Call) (Result, error) {
	var args struct{ Pattern string }
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if args.Pattern == "" {
		return errResult("pattern is required")
	}

	var matches []string
	err := filepath.WalkDir(t.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory should not abort the whole walk.
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(t.Root, path)
		if relErr != nil {
			return nil
		}
		if matchGlob(args.Pattern, rel) {
			matches = append(matches, rel)
		}
		if len(matches) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return errResult("search failed: %v", err)
	}

	sort.Strings(matches)
	if len(matches) == 0 {
		return Result{Content: fmt.Sprintf("no files match %q", args.Pattern)}, nil
	}
	return Result{Content: strings.Join(matches, "\n")}, nil
}

// matchGlob supports ** as "any number of path segments", which filepath.Match
// does not handle.
func matchGlob(pattern, path string) bool {
	if !strings.Contains(pattern, "**") {
		if ok, err := filepath.Match(pattern, path); err == nil && ok {
			return true
		}
		// A bare pattern such as *.go should also match nested files, which is
		// what a caller almost always means.
		if !strings.Contains(pattern, string(filepath.Separator)) {
			if ok, err := filepath.Match(pattern, filepath.Base(path)); err == nil && ok {
				return true
			}
		}
		return false
	}

	// Translate the glob into a regexp so ** can span separators.
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			// Match zero or more leading segments.
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case pattern[i] == '*':
			b.WriteString("[^/]*")
		case pattern[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// Grep searches file contents by regular expression.
type Grep struct{ Root string }

func (Grep) Def() provider.ToolDef {
	return provider.ToolDef{
		Name:        "grep",
		Description: "Search file contents with a regular expression.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
				"include": map[string]any{"type": "string", "description": "Optional glob limiting which files are searched."},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t Grep) Run(_ context.Context, call Call) (Result, error) {
	var args struct {
		Pattern string
		Include string
	}
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return errResult("invalid regular expression: %v", err)
	}

	var matches []string
	truncated := false
	walkErr := filepath.WalkDir(t.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(t.Root, path)
		if relErr != nil {
			return nil
		}
		if args.Include != "" && !matchGlob(args.Include, rel) {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil || isBinary(data) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			matches = append(matches, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			if len(matches) >= maxMatches {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return errResult("search failed: %v", walkErr)
	}

	if len(matches) == 0 {
		return Result{Content: fmt.Sprintf("no matches for %q", args.Pattern)}, nil
	}
	content := strings.Join(matches, "\n")
	if truncated {
		content += fmt.Sprintf("\n... [stopped at %d matches]", maxMatches)
	}
	return Result{Content: content}, nil
}

// isBinary reports whether data looks non-textual. A NUL byte in the first
// chunk is the usual heuristic and avoids dumping binary into the context.
func isBinary(data []byte) bool {
	limit := min(len(data), 512)
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
