package enforce

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// killGrace is how long a timed-out process gets to exit after SIGTERM before
// it is killed outright.
const killGrace = 2 * time.Second

// RunOutcome is the raw result of executing one command.
type RunOutcome struct {
	// ExitCode is nil when the process never started or was killed before
	// reporting a status.
	ExitCode        *int
	Stdout          string
	Stderr          string
	TimedOut        bool
	OutputTruncated bool
	StartFailed     bool
	Duration        time.Duration
}

// Runner executes a gate command. It is an interface so gate logic can be
// tested without spawning processes.
type Runner interface {
	Run(ctx context.Context, gate workflow.Gate, dir string) RunOutcome
}

// ExecRunner runs commands as real processes: argv only, never through a
// shell, so a gate cannot smuggle in a pipe or a redirect.
type ExecRunner struct{}

// cappedBuffer accumulates output up to a shared byte budget. The budget spans
// both streams together so a chatty stderr cannot evade the cap.
type cappedBuffer struct {
	mu        *sync.Mutex
	budget    *int
	truncated *bool
	buf       []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if *c.budget <= 0 {
		*c.truncated = true
		return len(p), nil
	}
	take := len(p)
	if take > *c.budget {
		take = *c.budget
		*c.truncated = true
	}
	c.buf = append(c.buf, p[:take]...)
	*c.budget -= take
	return len(p), nil
}

// Run executes the gate, enforcing its timeout and output cap.
func (ExecRunner) Run(ctx context.Context, gate workflow.Gate, dir string) RunOutcome {
	started := time.Now()
	if len(gate.Command) == 0 {
		return RunOutcome{StartFailed: true, Stderr: "gate has no command"}
	}

	// A dedicated timeout context keeps a gate timeout distinguishable from
	// the caller cancelling the whole run.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(gate.TimeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, gate.Command[0], gate.Command[1:]...)
	cmd.Dir = dir
	if gate.WorkDir != "" {
		cmd.Dir = gate.WorkDir
	}
	cmd.Stdin = nil

	var mu sync.Mutex
	budget := gate.MaxOutputBytes
	truncated := false
	stdout := &cappedBuffer{mu: &mu, budget: &budget, truncated: &truncated}
	stderr := &cappedBuffer{mu: &mu, budget: &budget, truncated: &truncated}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Escalate to SIGKILL if the process ignores the termination signal.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace

	if err := cmd.Start(); err != nil {
		return RunOutcome{
			StartFailed: true,
			Stderr:      err.Error(),
			Duration:    time.Since(started),
		}
	}

	waitErr := cmd.Wait()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	outcome := RunOutcome{
		TimedOut: timedOut,
		Duration: time.Since(started),
	}
	mu.Lock()
	outcome.Stdout = string(stdout.buf)
	outcome.Stderr = string(stderr.buf)
	outcome.OutputTruncated = truncated
	mu.Unlock()

	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		outcome.ExitCode = &code
	} else if waitErr != nil && !timedOut {
		// Killed by a signal with no exit status to report.
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code := exitErr.ExitCode()
			if code >= 0 {
				outcome.ExitCode = &code
			}
		}
	}
	return outcome
}
