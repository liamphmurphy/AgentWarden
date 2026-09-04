package enforce

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

func gate(command ...string) workflow.Gate {
	return workflow.Gate{
		ID:             "g",
		Command:        command,
		TimeoutSeconds: 10,
		MaxOutputBytes: workflow.DefaultMaxOutputBytes,
	}
}

// These exercise the real ExecRunner against actual processes, because the
// fake cannot prove that argv is passed without a shell or that a timeout
// really kills a child.
func TestExecRunnerSuccessAndFailure(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		wantCode int
	}{
		{"true exits zero", []string{"/usr/bin/true"}, 0},
		{"false exits non-zero", []string{"/usr/bin/false"}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := ExecRunner{}.Run(context.Background(), gate(tc.command...), ".", nil)
			if out.StartFailed {
				t.Fatalf("unexpected start failure: %s", out.Stderr)
			}
			if out.ExitCode == nil || *out.ExitCode != tc.wantCode {
				t.Errorf("exit code = %v, want %d", out.ExitCode, tc.wantCode)
			}
		})
	}
}

// TestExecRunnerDoesNotUseShell is the security-relevant property: a gate
// command is argv, so shell metacharacters are inert arguments.
func TestExecRunnerDoesNotUseShell(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(), gate("echo", "a && b > c | d"), ".", nil)
	if out.ExitCode == nil || *out.ExitCode != 0 {
		t.Fatalf("echo should succeed, got %+v", out)
	}
	got := strings.TrimSpace(out.Stdout)
	if got != "a && b > c | d" {
		t.Errorf("shell metacharacters were interpreted: %q", got)
	}
}

func TestExecRunnerStartFailure(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(), gate("definitely-not-a-real-binary-xyz"), ".", nil)
	if !out.StartFailed {
		t.Error("want StartFailed for a missing executable")
	}
	if out.ExitCode != nil {
		t.Errorf("a process that never started has no exit code, got %v", *out.ExitCode)
	}
}

func TestExecRunnerEmptyCommand(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(), workflow.Gate{ID: "g"}, ".", nil)
	if !out.StartFailed {
		t.Error("an empty command must be a start failure")
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	g := gate("/bin/sh", "-c", "sleep 5")
	g.TimeoutSeconds = 1

	out := ExecRunner{}.Run(context.Background(), g, ".", nil)
	if !out.TimedOut {
		t.Error("want TimedOut")
	}
	if out.Duration > 4*1e9 {
		t.Errorf("timeout should not wait for the full sleep, took %s", out.Duration)
	}
}

// TestExecRunnerOutputCap checks the budget spans both streams, so a chatty
// stderr cannot evade the cap.
func TestExecRunnerOutputCap(t *testing.T) {
	g := gate("/bin/sh", "-c", "printf 'aaaaaaaaaa'; printf 'bbbbbbbbbb' >&2")
	g.MaxOutputBytes = 12

	out := ExecRunner{}.Run(context.Background(), g, ".", nil)
	if !out.OutputTruncated {
		t.Error("want OutputTruncated")
	}
	if total := len(out.Stdout) + len(out.Stderr); total != 12 {
		t.Errorf("captured %d bytes across both streams, want exactly 12", total)
	}
}

func TestExecRunnerCapturesBothStreams(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(),
		gate("/bin/sh", "-c", "printf out; printf err >&2"), ".", nil)
	if out.Stdout != "out" {
		t.Errorf("stdout = %q, want %q", out.Stdout, "out")
	}
	if out.Stderr != "err" {
		t.Errorf("stderr = %q, want %q", out.Stderr, "err")
	}
}

func TestExecRunnerRunsInGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	out := ExecRunner{}.Run(context.Background(), gate("/bin/sh", "-c", "pwd"), dir, nil)
	if !strings.Contains(out.Stdout, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want it under %q", strings.TrimSpace(out.Stdout), dir)
	}
}

// TestExecRunnerStreamsLines is the property the UI depends on: output has to
// arrive while the gate runs, not only once it finishes, or a long suite looks
// like a hang.
func TestExecRunnerStreamsLines(t *testing.T) {
	var (
		mu    sync.Mutex
		lines []string
	)
	g := gate("/bin/sh", "-c", "printf 'one\\ntwo\\nthree\\n'")
	out := ExecRunner{}.Run(context.Background(), g, ".", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	if out.ExitCode == nil || *out.ExitCode != 0 {
		t.Fatalf("unexpected outcome: %+v", out)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("streamed %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestExecRunnerStreamsArrivesBeforeExit proves the lines are delivered during
// the run rather than in one batch at the end.
func TestExecRunnerStreamsArrivesBeforeExit(t *testing.T) {
	first := make(chan struct{})
	var once sync.Once

	g := gate("/bin/sh", "-c", "printf 'early\\n'; sleep 1; printf 'late\\n'")
	start := time.Now()
	var firstAt time.Duration

	ExecRunner{}.Run(context.Background(), g, ".", func(string) {
		once.Do(func() {
			firstAt = time.Since(start)
			close(first)
		})
	})

	select {
	case <-first:
	default:
		t.Fatal("no line was streamed at all")
	}
	// The first line must arrive well before the process exits a second later.
	if firstAt > 900*time.Millisecond {
		t.Errorf("first line arrived after %s; it should stream immediately", firstAt)
	}
}

// TestExecRunnerStreamsTrailingPartialLine: a process that exits without a
// final newline should still have its last line reported.
func TestExecRunnerStreamsTrailingPartialLine(t *testing.T) {
	var lines []string
	var mu sync.Mutex
	g := gate("/bin/sh", "-c", "printf 'no trailing newline'")
	ExecRunner{}.Run(context.Background(), g, ".", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "no trailing newline" {
		t.Errorf("streamed %v, want the trailing line", lines)
	}
}

func TestExecRunnerStreamsBothStreams(t *testing.T) {
	var lines []string
	var mu sync.Mutex
	g := gate("/bin/sh", "-c", "printf 'to stdout\\n'; printf 'to stderr\\n' >&2")
	ExecRunner{}.Run(context.Background(), g, ".", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "|")
	for _, want := range []string{"to stdout", "to stderr"} {
		if !strings.Contains(joined, want) {
			t.Errorf("streamed %v, want it to include %q", lines, want)
		}
	}
}

// TestExecRunnerStripsCarriageReturns keeps CRLF output from rendering with a
// stray glyph.
func TestExecRunnerStripsCarriageReturns(t *testing.T) {
	var lines []string
	var mu sync.Mutex
	g := gate("/bin/sh", "-c", "printf 'windows\\r\\n'")
	ExecRunner{}.Run(context.Background(), g, ".", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "windows" {
		t.Errorf("streamed %q, want the carriage return stripped", lines)
	}
}

func TestExecRunnerNilCallbackIsSafe(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(),
		gate("/bin/sh", "-c", "printf 'x\\n'"), ".", nil)
	if out.Stdout != "x\n" {
		t.Errorf("stdout = %q; a nil callback must not change capture", out.Stdout)
	}
}
