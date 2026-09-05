package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lmurphy/agentwarden/internal/tool"
)

type confirmationResult struct {
	allowed bool
	err     error
}

func beginConfirmation(
	t *testing.T,
	m *Model,
	ctx context.Context,
	call tool.Call,
	action string,
	resource string,
) <-chan confirmationResult {
	t.Helper()
	result := make(chan confirmationResult, 1)
	go func() {
		allowed, err := m.confirmer.Confirm(ctx, call, action, resource)
		result <- confirmationResult{allowed: allowed, err: err}
	}()

	select {
	case msg := <-m.events:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation request did not reach the UI")
	}
	return result
}

func receiveConfirmationResult(t *testing.T, result <-chan confirmationResult) confirmationResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation did not return to the model loop")
		return confirmationResult{}
	}
}

func TestConfirmationWaitsForExplicitApproval(t *testing.T) {
	m := newModel(t, &stubRunner{})
	result := beginConfirmation(t, m, context.Background(),
		tool.Call{Name: "write"}, "edit", "examples/fib_test.go")

	if !m.confirmationPane.IsOpen() {
		t.Fatal("permission request should open the confirmation pane")
	}
	view := stripANSI(m.View())
	for _, want := range []string{"Permission required", "write", "edit", "examples/fib_test.go", "Allow once", "Deny"} {
		if !strings.Contains(view, want) {
			t.Errorf("confirmation view missing %q:\n%s", want, view)
		}
	}
	select {
	case got := <-result:
		t.Fatalf("confirmation returned before a keypress: %+v", got)
	default:
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := receiveConfirmationResult(t, result)
	if got.err != nil || !got.allowed {
		t.Fatalf("enter should allow once, got %+v", got)
	}
	if m.confirmationPane.IsOpen() {
		t.Error("answering should close the confirmation pane")
	}
}

func TestConfirmationOwnsKeyboardAndCanDeny(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.input.SetValue("draft stays here")
	result := beginConfirmation(t, m, context.Background(),
		tool.Call{Name: "bash"}, "shell", "go test ./...")

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if got := m.input.Value(); got != "draft stays here" {
		t.Errorf("typing leaked behind the confirmation pane: %q", got)
	}
	if !m.confirmationPane.IsOpen() {
		t.Fatal("an unrelated key should not dismiss the confirmation")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := receiveConfirmationResult(t, result)
	if got.err != nil || got.allowed {
		t.Fatalf("escape should deny the action, got %+v", got)
	}
}

func TestConfirmationSessionApprovalMatchesExactTarget(t *testing.T) {
	m := newModel(t, &stubRunner{})
	call := tool.Call{Name: "write"}
	result := beginConfirmation(t, m, context.Background(), call, "edit", "a.go")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if got := receiveConfirmationResult(t, result); got.err != nil || !got.allowed {
		t.Fatalf("session approval failed: %+v", got)
	}

	// The same action and resource is approved without another UI event.
	allowed, err := m.confirmer.Confirm(context.Background(), call, "edit", "a.go")
	if err != nil || !allowed {
		t.Fatalf("cached exact approval = (%t, %v), want true", allowed, err)
	}
	select {
	case msg := <-m.events:
		t.Fatalf("cached approval unexpectedly prompted: %+v", msg)
	default:
	}

	// A different path still requires an independent decision.
	other := beginConfirmation(t, m, context.Background(), call, "edit", "b.go")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if got := receiveConfirmationResult(t, other); got.err != nil || got.allowed {
		t.Fatalf("different target should be denied by the explicit choice: %+v", got)
	}
}

func TestConfirmationCancellationReleasesWaitingCall(t *testing.T) {
	m := newModel(t, &stubRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	result := beginConfirmation(t, m, ctx,
		tool.Call{Name: "bash"}, "shell", "go test ./...")
	cancel()

	got := receiveConfirmationResult(t, result)
	if !errors.Is(got.err, context.Canceled) || got.allowed {
		t.Fatalf("cancelled confirmation = %+v, want context.Canceled", got)
	}
	// The run completion owns final cleanup if cancellation wins before the
	// UI can answer.
	m.Update(doneMsg{err: context.Canceled})
	if m.confirmationPane.IsOpen() {
		t.Error("run completion should close an abandoned confirmation")
	}
}

func TestConfirmationCtrlCCancelsTheRun(t *testing.T) {
	m := newModel(t, &stubRunner{})
	m.busy = true
	cancelled := false
	m.cancel = func() { cancelled = true }
	response := make(chan confirmationDecision, 1)
	m.Update(confirmationMsg{
		tool: "bash", action: "shell", resource: "go test ./...", response: response,
	})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !cancelled {
		t.Error("ctrl+c should cancel a run waiting for permission")
	}
	if cmd != nil {
		t.Error("ctrl+c should cancel the run rather than quit the app")
	}
	select {
	case decision := <-response:
		if decision != confirmationDeny {
			t.Errorf("ctrl+c decision = %v, want deny", decision)
		}
	default:
		t.Error("ctrl+c did not release the waiting confirmation")
	}
}

type recordingApprovalSwitcher struct{ calls []bool }

func (s *recordingApprovalSwitcher) SetAutoApproval(on bool) {
	s.calls = append(s.calls, on)
}

func TestAutoCommandChangesTheLivePermissionEvaluator(t *testing.T) {
	switcher := &recordingApprovalSwitcher{}
	m := New(Options{Runner: &stubRunner{}, Approvals: switcher})

	m.command("/auto")
	m.command("/auto")
	if len(switcher.calls) != 2 || !switcher.calls[0] || switcher.calls[1] {
		t.Errorf("live auto settings = %v, want [true false]", switcher.calls)
	}
}

func TestPermissionPreviewShowsBothEndsOfLongCommands(t *testing.T) {
	command := "START " + strings.Repeat("x", 700) + " END"
	preview := permissionPreview(command)
	for _, want := range []string{"START", "omitted", "END"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q: %s", want, preview)
		}
	}
}
