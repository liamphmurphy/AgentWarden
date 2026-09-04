package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lmurphy/agentwarden/internal/provider"
)

func args(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func run(t *testing.T, tool Tool, argv any) Result {
	t.Helper()
	res, err := tool.Run(context.Background(), Call{ID: "c1", Name: tool.Def().Name, Args: args(t, argv)})
	if err != nil {
		t.Fatalf("%s returned a runtime error: %v", tool.Def().Name, err)
	}
	return res
}

func projectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("package sub\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadFile(t *testing.T) {
	dir := projectDir(t)
	res := run(t, Read{Root: dir}, map[string]any{"path": "a.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "hello world" {
		t.Errorf("content = %q", res.Content)
	}
}

func TestReadErrors(t *testing.T) {
	dir := projectDir(t)
	tests := []struct {
		name     string
		argv     map[string]any
		wantWord string
	}{
		{"missing file", map[string]any{"path": "nope.txt"}, "cannot read"},
		{"directory", map[string]any{"path": "sub"}, "is a directory"},
		{"no path", map[string]any{}, "required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, Read{Root: dir}, tc.argv)
			if !res.IsError {
				t.Fatal("expected an error result")
			}
			if !strings.Contains(res.Content, tc.wantWord) {
				t.Errorf("content = %q, want it to mention %q", res.Content, tc.wantWord)
			}
		})
	}
}

// TestPathConfinement is the security-relevant property: a model must not be
// able to reach outside the project directory.
func TestPathConfinement(t *testing.T) {
	dir := projectDir(t)
	outside := filepath.Join(t.TempDir(), "vault.txt")
	if err := os.WriteFile(outside, []byte("CLASSIFIED-CONTENTS"), 0o600); err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"../vault.txt",
		"../../etc/passwd",
		"sub/../../outside.txt",
		outside,
		"/etc/passwd",
	}
	for _, path := range escapes {
		t.Run(path, func(t *testing.T) {
			res := run(t, Read{Root: dir}, map[string]any{"path": path})
			if !res.IsError {
				t.Fatalf("reading %q should be refused, got: %s", path, res.Content)
			}
			if strings.Contains(res.Content, "CLASSIFIED-CONTENTS") {
				t.Error("the refusal must not leak file contents")
			}
		})
	}

	// Writes are confined too.
	res := run(t, Write{Root: dir}, map[string]any{"path": "../escaped.txt", "content": "x"})
	if !res.IsError {
		t.Error("writing outside the project should be refused")
	}
}

func TestWriteCreatesNestedFile(t *testing.T) {
	dir := projectDir(t)
	res := run(t, Write{Root: dir}, map[string]any{"path": "deep/nested/new.txt", "content": "data"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(dir, "deep/nested/new.txt"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("content = %q", got)
	}
}

func TestEditReplacesExactString(t *testing.T) {
	dir := projectDir(t)
	res := run(t, Edit{Root: dir}, map[string]any{
		"path":       "a.txt",
		"old_string": "world",
		"new_string": "there",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "hello there" {
		t.Errorf("content = %q", got)
	}
}

// TestEditRefusesAmbiguousMatch prevents silently changing the wrong
// occurrence.
func TestEditRefusesAmbiguousMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dup.txt"), []byte("x\nx\nx\n"), 0o644)

	res := run(t, Edit{Root: dir}, map[string]any{
		"path": "dup.txt", "old_string": "x", "new_string": "y",
	})
	if !res.IsError {
		t.Fatal("an ambiguous edit should be refused")
	}
	if !strings.Contains(res.Content, "3 times") {
		t.Errorf("the refusal should say how many matches: %q", res.Content)
	}

	// replace_all makes the intent explicit.
	res = run(t, Edit{Root: dir}, map[string]any{
		"path": "dup.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	})
	if res.IsError {
		t.Fatalf("replace_all should be accepted: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "dup.txt"))
	if string(got) != "y\ny\ny\n" {
		t.Errorf("content = %q", got)
	}
}

func TestEditErrors(t *testing.T) {
	dir := projectDir(t)
	tests := []struct {
		name     string
		argv     map[string]any
		wantWord string
	}{
		{"not found", map[string]any{"path": "a.txt", "old_string": "zzz", "new_string": "y"}, "not found"},
		{"identical strings", map[string]any{"path": "a.txt", "old_string": "a", "new_string": "a"}, "identical"},
		{"missing file", map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"}, "cannot read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, Edit{Root: dir}, tc.argv)
			if !res.IsError || !strings.Contains(res.Content, tc.wantWord) {
				t.Errorf("got %q, want an error mentioning %q", res.Content, tc.wantWord)
			}
		})
	}
}

func TestEditPreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")
	os.WriteFile(path, []byte("echo old\n"), 0o755)

	run(t, Edit{Root: dir}, map[string]any{"path": "script.sh", "old_string": "old", "new_string": "new"})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755 preserved", info.Mode().Perm())
	}
}

func TestLS(t *testing.T) {
	dir := projectDir(t)
	res := run(t, LS{Root: dir}, map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, want := range []string{"a.txt", "sub/"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("listing should contain %q:\n%s", want, res.Content)
		}
	}
}

func TestGlob(t *testing.T) {
	dir := projectDir(t)
	os.WriteFile(filepath.Join(dir, "top.go"), []byte("package main"), 0o644)
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.go"), []byte("x"), 0o644)

	tests := []struct {
		pattern string
		want    []string
		notWant []string
	}{
		{"**/*.go", []string{"top.go", "sub/b.go"}, []string{"a.txt"}},
		{"*.go", []string{"top.go", "sub/b.go"}, []string{"a.txt"}},
		{"sub/*.go", []string{"sub/b.go"}, []string{"top.go"}},
		{"*.txt", []string{"a.txt"}, []string{"top.go"}},
	}
	for _, tc := range tests {
		t.Run(tc.pattern, func(t *testing.T) {
			res := run(t, Glob{Root: dir}, map[string]any{"pattern": tc.pattern})
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.Content, want) {
					t.Errorf("want %q in results:\n%s", want, res.Content)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(res.Content, notWant) {
					t.Errorf("did not want %q in results:\n%s", notWant, res.Content)
				}
			}
			// Vendored trees waste the context budget and are skipped.
			if strings.Contains(res.Content, "node_modules") {
				t.Errorf("node_modules should be skipped:\n%s", res.Content)
			}
		})
	}
}

func TestGlobNoMatches(t *testing.T) {
	res := run(t, Glob{Root: projectDir(t)}, map[string]any{"pattern": "*.rs"})
	if res.IsError {
		t.Fatalf("no matches is not an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no files match") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestGrep(t *testing.T) {
	dir := projectDir(t)
	res := run(t, Grep{Root: dir}, map[string]any{"pattern": "func B"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "sub/b.go:3") {
		t.Errorf("want a file:line match:\n%s", res.Content)
	}
}

func TestGrepWithInclude(t *testing.T) {
	dir := projectDir(t)
	os.WriteFile(filepath.Join(dir, "note.txt"), []byte("package sub\n"), 0o644)

	res := run(t, Grep{Root: dir}, map[string]any{"pattern": "package sub", "include": "**/*.go"})
	if strings.Contains(res.Content, "note.txt") {
		t.Errorf("include should exclude non-Go files:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "b.go") {
		t.Errorf("want the Go match:\n%s", res.Content)
	}
}

func TestGrepInvalidRegexp(t *testing.T) {
	res := run(t, Grep{Root: projectDir(t)}, map[string]any{"pattern": "([unclosed"})
	if !res.IsError || !strings.Contains(res.Content, "invalid regular expression") {
		t.Errorf("want a regexp error, got %q", res.Content)
	}
}

// TestGrepSkipsBinaryFiles keeps binary content out of the context window.
func TestGrepSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("match\x00\x01binary"), 0o644)
	os.WriteFile(filepath.Join(dir, "text.txt"), []byte("match here"), 0o644)

	res := run(t, Grep{Root: dir}, map[string]any{"pattern": "match"})
	if strings.Contains(res.Content, "blob.bin") {
		t.Errorf("binary files should be skipped:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "text.txt") {
		t.Errorf("text files should still match:\n%s", res.Content)
	}
}

func TestBashRunsCommand(t *testing.T) {
	dir := projectDir(t)
	res := run(t, Bash{Root: dir}, map[string]any{"command": "cat a.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello world") {
		t.Errorf("content = %q", res.Content)
	}
}

// TestBashNonZeroExitIsInformation: a failing command is a result the model
// should see, not a runtime failure.
func TestBashNonZeroExitIsInformation(t *testing.T) {
	res := run(t, Bash{Root: t.TempDir()}, map[string]any{"command": "echo oops >&2; exit 3"})
	if !res.IsError {
		t.Error("a non-zero exit should be marked as an error result")
	}
	if !strings.Contains(res.Content, "exit status 3") {
		t.Errorf("content should report the exit status: %q", res.Content)
	}
	if !strings.Contains(res.Content, "oops") {
		t.Errorf("stderr should be included: %q", res.Content)
	}
}

func TestBashTimeout(t *testing.T) {
	res := run(t, Bash{Root: t.TempDir()}, map[string]any{"command": "sleep 5", "timeout_seconds": 1})
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Errorf("want a timeout, got %q", res.Content)
	}
}

func TestBashRequiresCommand(t *testing.T) {
	res := run(t, Bash{Root: t.TempDir()}, map[string]any{"command": "   "})
	if !res.IsError || !strings.Contains(res.Content, "required") {
		t.Errorf("want a required-argument error, got %q", res.Content)
	}
}

func TestArgv(t *testing.T) {
	got, err := Argv(`{"command":"git -c a=b push origin main"}`)
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	want := []string{"git", "-c", "a=b", "push", "origin", "main"}
	if len(got) != len(want) {
		t.Fatalf("Argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := Argv(`{}`); err == nil {
		t.Error("a missing command should be an error")
	}
}

// TestTaskSchemaForcesDecomposition: requiring criteria in the schema is what
// makes a weak model decompose instead of forwarding "fix it".
func TestTaskSchemaForcesDecomposition(t *testing.T) {
	task := Task{Agents: []string{"engineer", "qa-engineer"}}
	def := task.Def()

	required, ok := def.Parameters["required"].([]string)
	if !ok {
		t.Fatalf("required = %#v", def.Parameters["required"])
	}
	for _, field := range []string{"subagent", "objective", "acceptance_criteria"} {
		found := false
		for _, r := range required {
			if r == field {
				found = true
			}
		}
		if !found {
			t.Errorf("%q should be a required argument", field)
		}
	}

	// The subagent list is a closed set so the model picks from it.
	props := def.Parameters["properties"].(map[string]any)
	subagent := props["subagent"].(map[string]any)
	if _, hasEnum := subagent["enum"]; !hasEnum {
		t.Error("subagent should be constrained to the known agents")
	}
}

func TestTaskDelegates(t *testing.T) {
	var got Delegation
	task := Task{
		Agents: []string{"engineer"},
		Delegate: func(_ context.Context, d Delegation) (string, error) {
			got = d
			return "done, tests pass", nil
		},
	}

	res := run(t, task, map[string]any{
		"subagent":            "engineer",
		"objective":           "add request timeouts to the HTTP client",
		"acceptance_criteria": []string{"integration suite passes"},
		"files_in_scope":      []string{"client.go"},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if got.Subagent != "engineer" || len(got.AcceptanceCriteria) != 1 {
		t.Errorf("delegation not passed through: %+v", got)
	}
	if !strings.Contains(res.Content, "done, tests pass") {
		t.Errorf("the subagent report should be returned: %q", res.Content)
	}
}

func TestTaskRejectsUnknownSubagent(t *testing.T) {
	task := Task{
		Agents:   []string{"engineer"},
		Delegate: func(context.Context, Delegation) (string, error) { return "", nil },
	}
	res := run(t, task, map[string]any{
		"subagent":            "nobody",
		"objective":           "do something at length",
		"acceptance_criteria": []string{"x"},
	})
	if !res.IsError || !strings.Contains(res.Content, "unknown subagent") {
		t.Errorf("want an unknown-subagent error, got %q", res.Content)
	}
}

func TestTaskWithoutDelegateIsUnavailable(t *testing.T) {
	res := run(t, Task{}, map[string]any{
		"subagent": "engineer", "objective": "x", "acceptance_criteria": []string{"y"},
	})
	if !res.IsError || !strings.Contains(res.Content, "not available") {
		t.Errorf("want an unavailable error, got %q", res.Content)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Add(Read{Root: "."})
	r.Add(Write{Root: "."})
	r.Add(Bash{Root: "."})

	if names := r.Names(); len(names) != 3 || names[0] != "read" || names[2] != "bash" {
		t.Errorf("registration order should be preserved: %v", names)
	}
	if _, ok := r.Get("read"); !ok {
		t.Error("read should be retrievable")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("unknown tools should not resolve")
	}
	if defs := r.Defs(); len(defs) != 3 {
		t.Errorf("want 3 defs, got %d", len(defs))
	}
}

// TestRegistryReplaceKeepsPosition matters because masked tool lists must stay
// deterministic for diffable request logs.
func TestRegistryReplaceKeepsPosition(t *testing.T) {
	r := NewRegistry()
	r.Add(Read{Root: "a"})
	r.Add(Write{Root: "a"})
	r.Add(Read{Root: "b"})

	names := r.Names()
	if len(names) != 2 || names[0] != "read" {
		t.Errorf("replacing a tool should keep its position: %v", names)
	}
	tool, _ := r.Get("read")
	if tool.(Read).Root != "b" {
		t.Error("the replacement should win")
	}
}

// Every built-in tool must satisfy the interface and expose a valid schema.
func TestAllToolsHaveValidSchemas(t *testing.T) {
	tools := []Tool{
		Read{}, Write{}, Edit{}, LS{}, Glob{}, Grep{}, Bash{}, Task{},
	}
	for _, tool := range tools {
		def := tool.Def()
		t.Run(def.Name, func(t *testing.T) {
			if def.Name == "" {
				t.Error("a tool needs a name")
			}
			if def.Description == "" {
				t.Error("a tool needs a description")
			}
			if def.Parameters["type"] != "object" {
				t.Errorf("parameters should be an object schema: %#v", def.Parameters)
			}
			// The schema must survive a round trip to the wire.
			if _, err := json.Marshal(def); err != nil {
				t.Errorf("schema is not serializable: %v", err)
			}
		})
	}
}

var (
	_ Tool = Read{}
	_ Tool = Write{}
	_ Tool = Edit{}
	_ Tool = LS{}
	_ Tool = Glob{}
	_ Tool = Grep{}
	_ Tool = Bash{}
	_ Tool = Task{}
	_ provider.ToolDef
)
