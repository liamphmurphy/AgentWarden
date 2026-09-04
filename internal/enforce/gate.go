package enforce

import (
	"context"
	"fmt"
	"time"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Failure reasons, in the precedence order applied by classify below. A tree
// that moved during the run outranks the command's own verdict, because a pass
// against a shifting tree proves nothing.
const (
	ReasonWorktreeChanged    = "worktree_changed"
	ReasonCommandStartFailed = "command_start_failed"
	ReasonTimedOut           = "timed_out"
	ReasonCommandFailed      = "command_failed"
)

// GateProgress reports gate lifecycle to the UI. Implementations must be safe
// to call from the gate goroutine.
type GateProgress interface {
	GateStarted(gateID string, command []string)
	GateOutput(gateID string, chunk string)
	GateFinished(receipt workflow.Receipt)
}

// NopProgress discards progress reports.
type NopProgress struct{}

func (NopProgress) GateStarted(string, []string)  {}
func (NopProgress) GateOutput(string, string)     {}
func (NopProgress) GateFinished(workflow.Receipt) {}

// GateRunner executes policy gates and produces receipts. It is the only
// producer of receipts in the system: an agent's assertion that a command
// passed is never accepted as evidence.
type GateRunner struct {
	runner        Runner
	fingerprinter Fingerprinter
	dir           string
	clock         workflow.Clock
	progress      GateProgress
}

// NewGateRunner wires a GateRunner.
func NewGateRunner(r Runner, f Fingerprinter, dir string, clock workflow.Clock, progress GateProgress) *GateRunner {
	if progress == nil {
		progress = NopProgress{}
	}
	return &GateRunner{runner: r, fingerprinter: f, dir: dir, clock: clock, progress: progress}
}

// RunGate executes one gate, fingerprinting the tree before and after so a
// receipt is rejected if the command mutated the very tree it was verifying.
func (g *GateRunner) RunGate(ctx context.Context, gate workflow.Gate, policyHash string) (workflow.Receipt, error) {
	before, err := g.fingerprinter.Fingerprint(ctx)
	if err != nil {
		return workflow.Receipt{}, fmt.Errorf("fingerprint before gate %s: %w", gate.ID, err)
	}

	g.progress.GateStarted(gate.ID, gate.Command)
	// Stream each line to the UI as it arrives, so a long suite shows
	// progress rather than appearing to hang until it finishes.
	outcome := g.runner.Run(ctx, gate, g.dir, func(line string) {
		g.progress.GateOutput(gate.ID, line)
	})

	after, err := g.fingerprinter.Fingerprint(ctx)
	if err != nil {
		return workflow.Receipt{}, fmt.Errorf("fingerprint after gate %s: %w", gate.ID, err)
	}

	receipt := workflow.Receipt{
		GateID:          gate.ID,
		Command:         gate.Command,
		ExitCode:        outcome.ExitCode,
		TimedOut:        outcome.TimedOut,
		OutputTruncated: outcome.OutputTruncated,
		Stdout:          outcome.Stdout,
		Stderr:          outcome.Stderr,
		DurationMS:      outcome.Duration.Milliseconds(),
		PolicyHash:      policyHash,
		Repository:      before,
		RanAt:           g.clock.Now(),
	}
	receipt.Success, receipt.FailureReason = classify(outcome, before, after)
	g.progress.GateFinished(receipt)
	return receipt, nil
}

// classify decides whether an outcome counts as a pass, applying the fixed
// precedence of failure reasons.
func classify(outcome RunOutcome, before, after workflow.Fingerprint) (bool, string) {
	switch {
	case !before.Same(after):
		return false, ReasonWorktreeChanged
	case outcome.StartFailed:
		return false, ReasonCommandStartFailed
	case outcome.TimedOut:
		return false, ReasonTimedOut
	case outcome.ExitCode == nil || *outcome.ExitCode != 0:
		return false, ReasonCommandFailed
	default:
		return true, ""
	}
}

// GateSet is the result of running a batch of gates.
type GateSet struct {
	Receipts []workflow.Receipt
	// Passed is false when any *required* gate failed. Optional gates run and
	// report but never flip this.
	Passed bool
	// FirstFailure names the required gate that stopped the run, if any.
	FirstFailure string
}

// RunGates executes every gate scheduled for a state. Required gates short
// circuit the batch on first failure; optional gates always run so their
// output is available, which is the behavior the plugin's config promised but
// never delivered.
func (g *GateRunner) RunGates(ctx context.Context, gates []workflow.Gate, policyHash string) (GateSet, error) {
	set := GateSet{Passed: true}
	for _, gate := range gates {
		if err := ctx.Err(); err != nil {
			return set, err
		}
		receipt, err := g.RunGate(ctx, gate, policyHash)
		if err != nil {
			return set, err
		}
		set.Receipts = append(set.Receipts, receipt)
		if !receipt.Success && gate.IsRequired() {
			set.Passed = false
			set.FirstFailure = gate.ID
			return set, nil
		}
	}
	return set, nil
}

// RunGatesAsync runs a batch in a goroutine and delivers the result on the
// returned channel. This keeps a long suite from blocking a tool call for its
// full timeout, and lets the TUI cancel via ctx.
func (g *GateRunner) RunGatesAsync(ctx context.Context, gates []workflow.Gate, policyHash string) <-chan GateSetResult {
	ch := make(chan GateSetResult, 1)
	go func() {
		defer close(ch)
		set, err := g.RunGates(ctx, gates, policyHash)
		ch <- GateSetResult{Set: set, Err: err}
	}()
	return ch
}

// GateSetResult pairs a batch result with its error for channel delivery.
type GateSetResult struct {
	Set GateSet
	Err error
}

// Elapsed is a small helper for progress rendering.
func Elapsed(since time.Time) time.Duration { return time.Since(since) }
