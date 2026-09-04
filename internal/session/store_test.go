package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

func newTask(id string) *workflow.Task {
	return &workflow.Task{
		ID:         id,
		Objective:  "do the thing",
		State:      workflow.StatePlanning,
		PolicyHash: "hash",
		Receipts:   map[string]workflow.Receipt{},
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	task := newTask("t1")
	code := 0
	task.Receipts["unit"] = workflow.Receipt{
		GateID:     "unit",
		Success:    true,
		ExitCode:   &code,
		PolicyHash: "hash",
		Repository: workflow.Fingerprint{Head: "h1", Digest: "d1"},
	}
	task.QA = &workflow.QA{Verdict: "approved", Actor: "qa-engineer"}

	if err := store.Save(task); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.ID != task.ID || loaded.State != task.State || loaded.Objective != task.Objective {
		t.Errorf("core fields did not round trip: %+v", loaded)
	}
	receipt, ok := loaded.Receipts["unit"]
	if !ok || !receipt.Success || receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		t.Errorf("receipt did not round trip: %+v", loaded.Receipts)
	}
	if !receipt.Repository.Same(workflow.Fingerprint{Head: "h1", Digest: "d1"}) {
		t.Error("the receipt fingerprint must survive, or evidence cannot be validated")
	}
	if loaded.QA == nil || loaded.QA.Verdict != "approved" {
		t.Errorf("QA did not round trip: %+v", loaded.QA)
	}
}

func TestLoadMissingTask(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Load("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	want := []provider.Message{
		{Role: provider.RoleUser, Text: "inspect the service"},
		{Role: provider.RoleAssistant, Text: "I found the handler."},
		{Role: provider.RoleUser, Text: "runtime correction", Internal: true},
	}

	if err := store.SaveMessages("t1", want); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}
	got, err := store.LoadMessages("t1")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadMessages(%q) = %#v, want %#v", "t1", got, want)
	}
}

func TestLoadMessagesMarksLegacyWorkflowCorrectionsInternal(t *testing.T) {
	store := NewStore(t.TempDir())
	wantVisible := "BLOCKED is a useful word in this user-authored prompt"
	messages := []provider.Message{
		{Role: provider.RoleUser, Text: wantVisible},
		{Role: provider.RoleUser, Text: "BLOCKED: the turn ended in state planning without calling workflow_submit_plan"},
	}
	if err := store.SaveMessages("t1", messages); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}

	got, err := store.LoadMessages("t1")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if got[0].Internal {
		t.Errorf("LoadMessages(user prompt).Internal = true, want false: %q", wantVisible)
	}
	if !got[1].Internal {
		t.Errorf("LoadMessages(legacy correction).Internal = false, want true: %q", got[1].Text)
	}
}

func TestLoadMessagesMissingCheckpoint(t *testing.T) {
	messages, err := NewStore(t.TempDir()).LoadMessages("t1")
	if err != nil {
		t.Fatalf("LoadMessages on an older task: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("LoadMessages on an older task = %#v, want empty", messages)
	}
}

func TestSaveRequiresID(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save(&workflow.Task{}); err == nil {
		t.Error("a task without an id should be rejected")
	}
}

func TestCreateRefusesDuplicate(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Create(newTask("t1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Create(newTask("t1")); err == nil {
		t.Error("creating the same task twice should be refused")
	}
}

func TestListSorted(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, id := range []string{"t3", "t1", "t2"} {
		if err := store.Create(newTask(id)); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"t1", "t2", "t3"}
	if len(ids) != 3 {
		t.Fatalf("List() = %v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestListEmptyStore(t *testing.T) {
	ids, err := NewStore(t.TempDir()).List()
	if err != nil {
		t.Fatalf("an empty store should list cleanly: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want no tasks, got %v", ids)
	}
}

// TestAuditLogIsAppendOnly is the property that makes history trustworthy:
// rewriting a task record must not rewrite what already happened.
func TestAuditLogIsAppendOnly(t *testing.T) {
	store := NewStore(t.TempDir())
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	machine := workflow.NewMachine(clock)

	task := newTask("t1")
	if err := store.Create(task); err != nil {
		t.Fatal(err)
	}

	// Two successive transitions, each persisted through Update.
	for _, action := range []workflow.Action{
		workflow.ActionPlanSubmitted,
		workflow.ActionImplementationSubmitted,
	} {
		if _, err := store.Update("t1", func(task *workflow.Task) error {
			return machine.Transition(task, action, "actor", nil)
		}); err != nil {
			t.Fatalf("Update(%s): %v", action, err)
		}
	}

	events, err := store.Events("t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 logged events, got %d", len(events))
	}
	if events[0].Action != workflow.ActionPlanSubmitted || events[1].Sequence != 2 {
		t.Errorf("events out of order: %+v", events)
	}

	// Overwriting the record entirely must leave the log intact.
	replacement := newTask("t1")
	replacement.State = workflow.StateCancelled
	if err := store.Save(replacement); err != nil {
		t.Fatal(err)
	}
	events, err = store.Events("t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("history should survive a record rewrite, got %d events", len(events))
	}
}

func TestEventsMissingLog(t *testing.T) {
	events, err := NewStore(t.TempDir()).Events("nope")
	if err != nil {
		t.Fatalf("a missing log should read as empty: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want no events, got %d", len(events))
	}
}

func TestUpdateMissingTask(t *testing.T) {
	store := NewStore(t.TempDir())
	_, err := store.Update("nope", func(*workflow.Task) error { return nil })
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestUpdatePropagatesMutateError checks a rejected transition is not
// persisted.
func TestUpdatePropagatesMutateError(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Create(newTask("t1")); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("refused")
	if _, err := store.Update("t1", func(task *workflow.Task) error {
		task.State = workflow.StateComplete
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("want the mutate error, got %v", err)
	}

	loaded, err := store.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != workflow.StatePlanning {
		t.Errorf("a failed update must not be persisted, state = %s", loaded.State)
	}
}

// TestConcurrentUpdatesSerialize is the replacement for the plugin's
// promise-chain lock: exactly one of two racing submissions may win.
func TestConcurrentUpdatesSerialize(t *testing.T) {
	store := NewStore(t.TempDir())
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	machine := workflow.NewMachine(clock)

	if err := store.Create(newTask("t1")); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update("t1", func(task *workflow.Task) error {
				return machine.Transition(task, workflow.ActionPlanSubmitted, "planner", nil)
			})
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Only the first can apply planning -> implementing; the rest find the
	// task already advanced and are rejected by the state machine.
	if succeeded != 1 {
		t.Errorf("want exactly 1 successful submission, got %d", succeeded)
	}

	loaded, err := store.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != workflow.StateImplementing {
		t.Errorf("state = %s, want implementing", loaded.State)
	}
	if loaded.Revision != 1 {
		t.Errorf("revision = %d, want exactly one applied transition", loaded.Revision)
	}
	events, err := store.Events("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("want 1 logged event, got %d", len(events))
	}
}

// TestSaveIsAtomic checks no temporary file is left behind for a reader to
// trip over.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(newTask("t1")); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, tasksDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Errorf("a temporary file was left behind: %s", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("want exactly one record, got %d", len(entries))
	}
}

func TestLoadCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	path := filepath.Join(dir, tasksDir, "bad.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("{not json"), 0o644)

	if _, err := store.Load("bad"); err == nil {
		t.Error("a corrupt record should be an error, not a zero task")
	}
}

func TestLoadRestoresNilReceiptMap(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	path := filepath.Join(dir, tasksDir, "t1.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	// A record written without a receipts key.
	os.WriteFile(path, []byte(`{"id":"t1","state":"planning"}`), 0o644)

	task, err := store.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Callers assign into this map, so it must never come back nil.
	if task.Receipts == nil {
		t.Error("receipts should be initialized")
	}
	task.Receipts["x"] = workflow.Receipt{}
}

func TestRecordIsHumanReadable(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(newTask("t1")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, tasksDir, "t1.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Indented output is the point of using files: cat should be useful.
	if !containsLine(string(raw), `  "id": "t1",`) {
		t.Errorf("record should be indented JSON:\n%s", raw)
	}
}

func containsLine(haystack, needle string) bool {
	for _, line := range splitLines(haystack) {
		if line == needle {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// fakeClock advances one second per read.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

var _ = fmt.Sprintf
