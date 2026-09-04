package enforce

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lmurphy/agentwarden/internal/workflow"
	"gopkg.in/yaml.v3"
)

// fakeClock advances one second per read for deterministic timestamps.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

// fakeFingerprinter returns a mutable fingerprint, letting a test simulate the
// tree moving at an exact moment.
type fakeFingerprinter struct {
	current workflow.Fingerprint
	calls   int
	// onCall mutates state between the before and after reads of a gate.
	onCall func(call int, f *fakeFingerprinter)
}

func newFakeFingerprinter() *fakeFingerprinter {
	return &fakeFingerprinter{current: workflow.Fingerprint{Head: "h1", Digest: "d1"}}
}

func (f *fakeFingerprinter) Fingerprint(context.Context) (workflow.Fingerprint, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall(f.calls, f)
	}
	return f.current, nil
}

// fakeRunner records invocations and returns scripted outcomes, so gate logic
// is tested without spawning processes.
type fakeRunner struct {
	calls []string
	// outcomes maps a gate ID to the outcome it should produce.
	outcomes map[string]RunOutcome
	// onRun fires before returning, to mutate external state mid-gate.
	onRun func(gateID string)
	// lines maps a gate ID to output lines it should stream as it runs.
	lines map[string][]string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outcomes: map[string]RunOutcome{}, lines: map[string][]string{}}
}

func exitCode(n int) *int { return &n }

func (r *fakeRunner) Run(_ context.Context, gate workflow.Gate, _ string, onLine LineFunc) RunOutcome {
	r.calls = append(r.calls, gate.ID)
	if onLine != nil {
		for _, line := range r.lines[gate.ID] {
			onLine(line)
		}
	}
	if r.onRun != nil {
		r.onRun(gate.ID)
	}
	if outcome, ok := r.outcomes[gate.ID]; ok {
		return outcome
	}
	return RunOutcome{ExitCode: exitCode(0)}
}

// recordingProgress captures progress callbacks for assertions.
type recordingProgress struct {
	started  []string
	finished []workflow.Receipt
	output   []string
}

func (p *recordingProgress) GateStarted(id string, _ []string) { p.started = append(p.started, id) }

func (p *recordingProgress) GateOutput(id, line string) {
	p.output = append(p.output, id+": "+line)
}

func (p *recordingProgress) GateFinished(r workflow.Receipt) { p.finished = append(p.finished, r) }

func mustPolicy(t *testing.T, doc string) *workflow.Policy {
	t.Helper()
	p, err := workflow.ParsePolicy(strings.NewReader(doc), yaml.Unmarshal)
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	return p
}

const twoGatePolicy = `
version: 1
roles:
  orchestrator: orchestrator
  planner: tech-lead
  implementer: engineer
  reviewer: qa-engineer
gates:
  - id: unit
    command: ["true"]
    required: true
  - id: integration
    command: ["true"]
    required: true
`

// newTask builds a task already bound to a policy hash.
func newTask(state workflow.State, policy *workflow.Policy) *workflow.Task {
	return &workflow.Task{
		ID:         "t1",
		State:      state,
		PolicyHash: policy.Hash(),
		Receipts:   map[string]workflow.Receipt{},
	}
}

// passingReceipt builds a receipt that satisfies the staleness predicate.
func passingReceipt(gateID string, policy *workflow.Policy, f workflow.Fingerprint) workflow.Receipt {
	return workflow.Receipt{
		GateID:     gateID,
		Success:    true,
		ExitCode:   exitCode(0),
		PolicyHash: policy.Hash(),
		Repository: f,
	}
}
