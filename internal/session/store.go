// Package session persists task records and the audit log as plain files.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lmurphy/agentwarden/internal/provider"
	"github.com/lmurphy/agentwarden/internal/workflow"
)

// Layout under the state directory.
const (
	tasksDir    = "tasks"
	eventsDir   = "events"
	messagesDir = "messages"
)

// Store persists tasks as JSON and their audit history as append-only JSONL.
//
// Files rather than a database: the records are small, a human can inspect
// them with cat, and there is no driver or cgo dependency.
type Store struct {
	root string
	mu   sync.Mutex
	// locks serializes mutations per task, so two concurrent submissions
	// cannot interleave a read-modify-write.
	locks map[string]*sync.Mutex
}

// NewStore returns a Store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{root: dir, locks: map[string]*sync.Mutex{}}
}

// lockFor returns the mutex guarding a task.
func (s *Store) lockFor(taskID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock, ok := s.locks[taskID]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	s.locks[taskID] = lock
	return lock
}

func (s *Store) taskPath(taskID string) string {
	return filepath.Join(s.root, tasksDir, taskID+".json")
}

func (s *Store) eventsPath(taskID string) string {
	return filepath.Join(s.root, eventsDir, taskID+".jsonl")
}

func (s *Store) messagesPath(taskID string) string {
	return filepath.Join(s.root, messagesDir, taskID+".json")
}

// ErrNotFound is returned when a task does not exist.
var ErrNotFound = fmt.Errorf("task not found")

// Save writes a task record atomically, so an interrupted write cannot leave a
// truncated record behind.
func (s *Store) Save(task *workflow.Task) error {
	if task.ID == "" {
		return fmt.Errorf("task has no id")
	}
	path := s.taskPath(task.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	raw, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write task: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit task: %w", err)
	}
	return nil
}

// Load reads a task record.
func (s *Store) Load(taskID string) (*workflow.Task, error) {
	raw, err := os.ReadFile(s.taskPath(taskID))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read task: %w", err)
	}
	var task workflow.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, fmt.Errorf("parse task %s: %w", taskID, err)
	}
	if task.Receipts == nil {
		task.Receipts = map[string]workflow.Receipt{}
	}
	return &task, nil
}

// SaveMessages persists the conversation checkpoint for a task atomically.
// It is separate from workflow.Task because provider messages are runtime
// data, not part of the pure workflow domain.
func (s *Store) SaveMessages(taskID string, messages []provider.Message) error {
	path := s.messagesPath(taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create message dir: %w", err)
	}
	raw, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write messages: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit messages: %w", err)
	}
	return nil
}

// LoadMessages reads a saved conversation, returning an empty conversation
// when the task predates conversation persistence.
func (s *Store) LoadMessages(taskID string) ([]provider.Message, error) {
	raw, err := os.ReadFile(s.messagesPath(taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	var messages []provider.Message
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, fmt.Errorf("parse messages for %s: %w", taskID, err)
	}
	return messages, nil
}

// List returns every task ID, sorted.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, tasksDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// AppendEvents adds entries to a task's audit log.
//
// The log is append-only and separate from the task record, so history
// survives even if a record is rewritten, and it can be tailed while a
// workflow runs.
func (s *Store) AppendEvents(taskID string, events []workflow.Event) error {
	if len(events) == 0 {
		return nil
	}
	path := s.eventsPath(taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create events dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open events log: %w", err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return fmt.Errorf("append event: %w", err)
		}
	}
	return nil
}

// Events reads a task's audit history.
func (s *Store) Events(taskID string) ([]workflow.Event, error) {
	raw, err := os.ReadFile(s.eventsPath(taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read events log: %w", err)
	}
	var out []workflow.Event
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event workflow.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("parse event log for %s: %w", taskID, err)
		}
		out = append(out, event)
	}
	return out, nil
}

// Update applies mutate to a task under its lock and persists the result,
// including any newly appended audit events.
//
// Holding the lock across the whole read-modify-write is what makes two
// concurrent submissions safe: one wins and the other sees the updated state.
func (s *Store) Update(taskID string, mutate func(task *workflow.Task) error) (*workflow.Task, error) {
	lock := s.lockFor(taskID)
	lock.Lock()
	defer lock.Unlock()

	task, err := s.Load(taskID)
	if err != nil {
		return nil, err
	}
	before := len(task.Events)

	if err := mutate(task); err != nil {
		return nil, err
	}
	if err := s.Save(task); err != nil {
		return nil, err
	}
	if len(task.Events) > before {
		if err := s.AppendEvents(taskID, task.Events[before:]); err != nil {
			return nil, err
		}
	}
	return task, nil
}

// Create persists a new task, refusing to overwrite an existing one.
func (s *Store) Create(task *workflow.Task) error {
	lock := s.lockFor(task.ID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := os.Stat(s.taskPath(task.ID)); err == nil {
		return fmt.Errorf("task %s already exists", task.ID)
	}
	if task.Receipts == nil {
		task.Receipts = map[string]workflow.Receipt{}
	}
	if err := s.Save(task); err != nil {
		return err
	}
	return s.AppendEvents(task.ID, task.Events)
}
