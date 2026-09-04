package enforce

import (
	"bytes"
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

// LineFunc receives each complete output line as a gate produces it. It may be
// nil, and is called from the reading goroutine, so implementations must not
// block.
type LineFunc func(line string)

// Runner executes a gate command. It is an interface so gate logic can be
// tested without spawning processes.
type Runner interface {
	// Run executes the gate. onLine, when non-nil, is called with each
	// complete line as it arrives, so a long suite can report progress
	// instead of appearing to hang until it finishes.
	Run(ctx context.Context, gate workflow.Gate, dir string, onLine LineFunc) RunOutcome
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
	// onLine, when set, receives each complete line. pending holds the
	// trailing partial line between writes, since a process may flush
	// mid-line.
	onLine  LineFunc
	pending []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	if *c.budget <= 0 {
		*c.truncated = true
	} else {
		take := len(p)
		if take > *c.budget {
			take = *c.budget
			*c.truncated = true
		}
		c.buf = append(c.buf, p[:take]...)
		*c.budget -= take
	}

	// Collect completed lines while holding the lock, but deliver them after
	// releasing it: the callback is caller-supplied and must not be able to
	// deadlock the other stream's writer.
	var lines []string
	if c.onLine != nil {
		c.pending = append(c.pending, p...)
		for {
			idx := bytes.IndexByte(c.pending, '\n')
			if idx < 0 {
				break
			}
			lines = append(lines, string(bytes.TrimRight(c.pending[:idx], "\r")))
			c.pending = c.pending[idx+1:]
		}
	}
	c.mu.Unlock()

	for _, line := range lines {
		c.onLine(line)
	}
	return len(p), nil
}

// flush delivers any trailing line that never ended in a newline.
func (c *cappedBuffer) flush() {
	if c.onLine == nil {
		return
	}
	c.mu.Lock()
	rest := string(bytes.TrimRight(c.pending, "\r"))
	c.pending = nil
	c.mu.Unlock()
	if rest != "" {
		c.onLine(rest)
	}
}

// Run executes the gate, enforcing its timeout and output cap.
func (ExecRunner) Run(ctx context.Context, gate workflow.Gate, dir string, onLine LineFunc) RunOutcome {
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
	stdout := &cappedBuffer{mu: &mu, budget: &budget, truncated: &truncated, onLine: onLine}
	stderr := &cappedBuffer{mu: &mu, budget: &budget, truncated: &truncated, onLine: onLine}
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
	stdout.flush()
	stderr.flush()
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
