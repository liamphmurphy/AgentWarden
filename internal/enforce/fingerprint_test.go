package enforce

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// gitRepo creates a real repository, because fingerprinting is defined by
// git's own view of the tree (tracked, untracked, ignored) and a fake cannot
// establish that behavior.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		// Signing would otherwise depend on the developer's gpg setup.
		{"config", "commit.gpgsign", "false"},
		{"config", "tag.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func fingerprint(t *testing.T, dir string) workflow.Fingerprint {
	t.Helper()
	fp, err := NewGitFingerprinter(dir).Fingerprint(context.Background())
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

// TestFingerprintUnbornHead covers a fresh repo with no commits, which must
// still yield a usable fingerprint rather than an error.
func TestFingerprintUnbornHead(t *testing.T) {
	dir := gitRepo(t)
	fp := fingerprint(t, dir)
	if fp.Head != UnbornHead {
		t.Errorf("head = %q, want %q", fp.Head, UnbornHead)
	}
	if fp.Digest == "" {
		t.Error("digest should be computed even with no commits")
	}
}

func TestFingerprintFailsClosedOutsideGit(t *testing.T) {
	// A bare temp dir with no repository above it.
	dir := t.TempDir()
	_, err := NewGitFingerprinter(dir).Fingerprint(context.Background())
	if err == nil {
		t.Skip("temp dir unexpectedly sits inside a git repository")
	}
	if !errors.Is(err, ErrNotAGitRepository) {
		t.Errorf("want ErrNotAGitRepository, got %v", err)
	}
}

func TestFingerprintStableAcrossReads(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.txt", "hello")
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-qm", "init")

	first := fingerprint(t, dir)
	second := fingerprint(t, dir)
	if !first.Same(second) {
		t.Error("fingerprint must be stable when nothing changes")
	}
	if first.Head == UnbornHead {
		t.Error("head should be a real commit after committing")
	}
}

// TestFingerprintDetectsChanges is the core guarantee behind receipt
// invalidation. Ignored files are deliberately excluded so build output does
// not invalidate evidence.
func TestFingerprintDetectsChanges(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, dir string)
		wantChange bool
	}{
		{
			name:       "tracked file edited",
			mutate:     func(t *testing.T, dir string) { write(t, dir, "a.txt", "changed") },
			wantChange: true,
		},
		{
			name:       "untracked file added",
			mutate:     func(t *testing.T, dir string) { write(t, dir, "new.txt", "new") },
			wantChange: true,
		},
		{
			name:       "tracked file deleted",
			mutate:     func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "a.txt")) },
			wantChange: true,
		},
		{
			name:       "new commit",
			mutate:     func(t *testing.T, dir string) { gitRun(t, dir, "commit", "-qm", "empty", "--allow-empty") },
			wantChange: true,
		},
		{
			name:       "permissions changed",
			mutate:     func(t *testing.T, dir string) { os.Chmod(filepath.Join(dir, "a.txt"), 0o755) },
			wantChange: true,
		},
		{
			name:       "ignored file added",
			mutate:     func(t *testing.T, dir string) { write(t, dir, "build/out.bin", "junk") },
			wantChange: false,
		},
		{
			name:       "ignored file changed",
			mutate:     func(t *testing.T, dir string) { write(t, dir, "ignored.log", "more noise") },
			wantChange: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitRepo(t)
			write(t, dir, "a.txt", "hello")
			write(t, dir, ".gitignore", "build/\n*.log\n")
			write(t, dir, "ignored.log", "noise")
			gitRun(t, dir, "add", "a.txt", ".gitignore")
			gitRun(t, dir, "commit", "-qm", "init")

			before := fingerprint(t, dir)
			tc.mutate(t, dir)
			after := fingerprint(t, dir)

			if changed := !before.Same(after); changed != tc.wantChange {
				t.Errorf("fingerprint changed = %v, want %v", changed, tc.wantChange)
			}
		})
	}
}

// TestFingerprintDistinguishesContentFromName guards against a digest that
// hashes only names or only bytes.
func TestFingerprintDistinguishesContentFromName(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.txt", "one")
	write(t, dir, "b.txt", "two")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "init")
	original := fingerprint(t, dir)

	// Swapping contents between two files keeps the multiset of bytes and the
	// set of names identical; only the pairing changes.
	write(t, dir, "a.txt", "two")
	write(t, dir, "b.txt", "one")
	swapped := fingerprint(t, dir)

	if original.Same(swapped) {
		t.Error("digest must bind each path to its own content")
	}
}

func TestFingerprintHandlesSymlinks(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.txt", "hello")
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "init")
	before := fingerprint(t, dir)

	os.Remove(filepath.Join(dir, "link"))
	if err := os.Symlink("other.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	after := fingerprint(t, dir)

	if before.Same(after) {
		t.Error("retargeting a symlink must change the digest")
	}
}

// TestFingerprintExcludesAgentwardenState is a regression test for a real bug:
// agentwarden persists receipts under .agentwarden/state inside the repository it
// fingerprints, so without this exclusion saving a receipt changed the very
// tree that receipt was bound to and invalidated it immediately.
func TestFingerprintExcludesAgentwardenState(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.txt", "hello")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "init")

	before := fingerprint(t, dir)

	// Simulate the store writing a task record and its audit log.
	write(t, dir, ".agentwarden/state/tasks/t1.json", `{"id":"t1","state":"verifying"}`)
	write(t, dir, ".agentwarden/state/events/t1.jsonl", `{"sequence":1}`)

	after := fingerprint(t, dir)
	if !before.Same(after) {
		t.Error("agentwarden bookkeeping must not change the fingerprint")
	}

	// Everything else under .agentwarden still counts, since policy and config
	// live there and a user edit to them is meaningful.
	write(t, dir, ".agentwarden/workflow.yml", "version: 1\n")
	if fingerprint(t, dir).Same(after) {
		t.Error("a policy file change should still register")
	}
}

func TestFingerprinterExclusionsAreConfigurable(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.txt", "hello")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "init")

	fp := &GitFingerprinter{Dir: dir, Exclude: []string{"generated"}}
	before, err := fp.Fingerprint(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	write(t, dir, "generated/out.txt", "machine written")
	after, err := fp.Fingerprint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !before.Same(after) {
		t.Error("an excluded prefix should not affect the digest")
	}

	// A path that merely shares a prefix must not be swept up.
	write(t, dir, "generated-notes.txt", "hand written")
	if final, _ := fp.Fingerprint(context.Background()); final.Same(after) {
		t.Error("exclusions must match whole path segments, not bare prefixes")
	}
}
