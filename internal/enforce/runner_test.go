package enforce

import (
	"context"
	"strings"
	"testing"

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
			out := ExecRunner{}.Run(context.Background(), gate(tc.command...), ".")
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
	out := ExecRunner{}.Run(context.Background(), gate("echo", "a && b > c | d"), ".")
	if out.ExitCode == nil || *out.ExitCode != 0 {
		t.Fatalf("echo should succeed, got %+v", out)
	}
	got := strings.TrimSpace(out.Stdout)
	if got != "a && b > c | d" {
		t.Errorf("shell metacharacters were interpreted: %q", got)
	}
}

func TestExecRunnerStartFailure(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(), gate("definitely-not-a-real-binary-xyz"), ".")
	if !out.StartFailed {
		t.Error("want StartFailed for a missing executable")
	}
	if out.ExitCode != nil {
		t.Errorf("a process that never started has no exit code, got %v", *out.ExitCode)
	}
}

func TestExecRunnerEmptyCommand(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(), workflow.Gate{ID: "g"}, ".")
	if !out.StartFailed {
		t.Error("an empty command must be a start failure")
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	g := gate("/bin/sh", "-c", "sleep 5")
	g.TimeoutSeconds = 1

	out := ExecRunner{}.Run(context.Background(), g, ".")
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

	out := ExecRunner{}.Run(context.Background(), g, ".")
	if !out.OutputTruncated {
		t.Error("want OutputTruncated")
	}
	if total := len(out.Stdout) + len(out.Stderr); total != 12 {
		t.Errorf("captured %d bytes across both streams, want exactly 12", total)
	}
}

func TestExecRunnerCapturesBothStreams(t *testing.T) {
	out := ExecRunner{}.Run(context.Background(),
		gate("/bin/sh", "-c", "printf out; printf err >&2"), ".")
	if out.Stdout != "out" {
		t.Errorf("stdout = %q, want %q", out.Stdout, "out")
	}
	if out.Stderr != "err" {
		t.Errorf("stderr = %q, want %q", out.Stderr, "err")
	}
}

func TestExecRunnerRunsInGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	out := ExecRunner{}.Run(context.Background(), gate("/bin/sh", "-c", "pwd"), dir)
	if !strings.Contains(out.Stdout, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want it under %q", strings.TrimSpace(out.Stdout), dir)
	}
}
