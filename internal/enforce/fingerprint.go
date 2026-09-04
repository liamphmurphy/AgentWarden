// Package enforce is where workflow policy meets the operating system: tool
// masking, gate execution, receipt validity and publication blocking.
package enforce

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lmurphy/agentwarden/internal/workflow"
)

// UnbornHead is the sentinel Head value for a repository with no commits yet,
// so a fresh `git init` still produces a usable fingerprint.
const UnbornHead = "UNBORN"

// ErrNotAGitRepository is returned when the target is outside a work tree.
// Fingerprinting fails closed: with no way to detect a moving tree, gate
// evidence cannot be trusted, so governance refuses to run at all.
var ErrNotAGitRepository = errors.New("not a git repository")

// Fingerprinter captures the identity of a working tree.
type Fingerprinter interface {
	Fingerprint(ctx context.Context) (workflow.Fingerprint, error)
}

// DefaultExclusions are paths omitted from every fingerprint.
//
// Agentwarden writes task records and receipts under .agentwarden/state, inside
// the repository it is fingerprinting. Without this exclusion, persisting a
// receipt would itself change the tree that receipt was bound to and
// immediately invalidate it.
var DefaultExclusions = []string{
	".agentwarden/state",
}

// GitFingerprinter fingerprints a git work tree, covering tracked and
// non-ignored untracked files. Ignored files are deliberately excluded so
// build output does not invalidate evidence.
type GitFingerprinter struct {
	Dir string
	// Exclude lists path prefixes to omit, relative to the repository root.
	Exclude []string
}

// NewGitFingerprinter returns a Fingerprinter rooted at dir, omitting the
// agentwarden's own state directory.
func NewGitFingerprinter(dir string) *GitFingerprinter {
	return &GitFingerprinter{Dir: dir, Exclude: DefaultExclusions}
}

// excluded reports whether a repository-relative path is omitted.
func (g *GitFingerprinter) excluded(rel string) bool {
	for _, prefix := range g.Exclude {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

func (g *GitFingerprinter) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Fingerprint returns the current tree identity.
func (g *GitFingerprinter) Fingerprint(ctx context.Context) (workflow.Fingerprint, error) {
	top, err := g.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return workflow.Fingerprint{}, ErrNotAGitRepository
	}
	root := strings.TrimSpace(top)

	head := UnbornHead
	if out, err := g.git(ctx, "rev-parse", "HEAD"); err == nil {
		if h := strings.TrimSpace(out); h != "" {
			head = h
		}
	}

	listing, err := g.git(ctx, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return workflow.Fingerprint{}, err
	}

	paths := make([]string, 0, 64)
	seen := make(map[string]bool)
	for _, p := range strings.Split(listing, "\x00") {
		if p == "" || seen[p] || g.excluded(p) {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, rel := range paths {
		abs := filepath.Join(root, rel)
		fmt.Fprintf(h, "PATH\x00%s\x00", rel)

		info, err := os.Lstat(abs)
		if err != nil {
			// A path git knows about but which is not on disk is itself a
			// meaningful difference, so record it rather than skipping.
			h.Write([]byte("MISSING\x00"))
			continue
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(abs)
			if err != nil {
				h.Write([]byte("MISSING\x00"))
				continue
			}
			fmt.Fprintf(h, "LINK\x00%s\x00", target)
		case info.Mode().IsRegular():
			fmt.Fprintf(h, "MODE\x00%o\x00", info.Mode().Perm())
			data, err := os.ReadFile(abs)
			if err != nil {
				h.Write([]byte("MISSING\x00"))
				continue
			}
			fmt.Fprintf(h, "SIZE\x00%d\x00", len(data))
			h.Write(data)
			h.Write([]byte("\x00"))
		default:
			h.Write([]byte("OTHER\x00"))
		}
	}

	return workflow.Fingerprint{Head: head, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}
