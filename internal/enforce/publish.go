package enforce

import (
	"path/filepath"
	"strings"
)

// gitGlobalFlagsWithValue are `git` options that consume the following
// argument. They must be skipped to find the real subcommand, which is why
// argv parsing beats matching a regex against the raw command string:
// `git -c protocol.version=2 push` is still a push.
var gitGlobalFlagsWithValue = map[string]bool{
	"-c":             true,
	"-C":             true,
	"--exec-path":    true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--config-env":   true,
	"--super-prefix": true,
}

// gitSubcommand walks git's global options and returns the subcommand, or ""
// if there isn't one.
func gitSubcommand(argv []string) string {
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case gitGlobalFlagsWithValue[arg]:
			i++ // skip this flag's value
		case strings.HasPrefix(arg, "--") && strings.Contains(arg, "="):
			// A self-contained long option such as --git-dir=foo.
		case strings.HasPrefix(arg, "-"):
			// A valueless global flag such as --no-pager or --paginate.
		default:
			return arg
		}
	}
	return ""
}

// base strips any directory prefix so /usr/bin/git matches git.
func base(cmd string) string {
	return filepath.Base(cmd)
}

// IsPublishCommand reports whether argv publishes work outside the local
// repository. Publication stays blocked until a task is complete, so a model
// cannot ship unverified work.
//
// This is an enforcement mechanism, not a security boundary: a shell wrapper
// or an alias can still get around it. Keep the same commands as required CI
// checks.
func IsPublishCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch base(argv[0]) {
	case "git":
		switch gitSubcommand(argv) {
		case "push":
			return true
		}
	case "gh":
		// gh pr merge, gh release create.
		if len(argv) >= 3 && argv[1] == "pr" && argv[2] == "merge" {
			return true
		}
		if len(argv) >= 3 && argv[1] == "release" && argv[2] == "create" {
			return true
		}
	}
	return false
}

// IsPolicyEdit reports whether target resolves to the active policy file.
// Comparing resolved absolute paths avoids the plugin's suffix match, which
// a differently-spelled relative path could slip past.
func IsPolicyEdit(policyPath, target string) bool {
	if policyPath == "" || target == "" {
		return false
	}
	a, errA := filepath.Abs(policyPath)
	b, errB := filepath.Abs(target)
	if errA != nil || errB != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
