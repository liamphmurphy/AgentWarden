package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Expand substitutes ${VAR} and ${VAR:-default} references from lookup.
//
// Secrets belong in the environment, not in a config file: the OpenCode config
// this tool replaces carried a live gateway key in plaintext.
//
// A reference with no value and no default is an error rather than an empty
// string, so a missing credential fails loudly at startup instead of
// producing a puzzling 401 later.
func Expand(input string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	var missing []string

	for i := 0; i < len(input); {
		// Only ${...} is a reference; a bare $VAR is left alone so shell-like
		// strings in commands survive untouched.
		if input[i] != '$' || i+1 >= len(input) || input[i+1] != '{' {
			b.WriteByte(input[i])
			i++
			continue
		}
		end := strings.IndexByte(input[i+2:], '}')
		if end < 0 {
			// An unterminated reference is literal text.
			b.WriteString(input[i:])
			break
		}
		ref := input[i+2 : i+2+end]
		i += end + 3

		name, fallback := ref, ""
		hasFallback := false
		if idx := strings.Index(ref, ":-"); idx >= 0 {
			name, fallback = ref[:idx], ref[idx+2:]
			hasFallback = true
		}

		switch value, ok := lookup(name); {
		case ok && value != "":
			b.WriteString(value)
		case hasFallback:
			b.WriteString(fallback)
		default:
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return b.String(), nil
}

// OSLookup reads from the process environment.
func OSLookup(name string) (string, bool) { return os.LookupEnv(name) }

// LoadEnvFile parses a KEY=VALUE file into a map. Blank lines and # comments
// are skipped, and surrounding quotes are stripped, so an existing .env can be
// pointed at directly.
func LoadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// chainLookup resolves against overrides first, then the process environment,
// so an envFile can supply a value without exporting it.
func chainLookup(overrides map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if v, ok := overrides[name]; ok {
			return v, true
		}
		return OSLookup(name)
	}
}
