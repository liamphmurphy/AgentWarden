package config

// StripJSONC removes // and /* */ comments and trailing commas, producing
// strict JSON.
//
// The config this replaces is not valid JSON — it carries trailing commas
// after each model block — so tolerating JSONC avoids a confusing parse error
// on a file the user reasonably considers fine.
func StripJSONC(input []byte) []byte {
	out := make([]byte, 0, len(input))

	var (
		inString bool
		escaped  bool
		inLine   bool
		inBlock  bool
	)

	for i := 0; i < len(input); i++ {
		c := input[i]

		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out = append(out, c)
			}
			continue

		case inBlock:
			if c == '*' && i+1 < len(input) && input[i+1] == '/' {
				inBlock = false
				i++
			}
			continue

		case inString:
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		// Outside a string: comments start, strings start, commas may be trailing.
		switch {
		case c == '/' && i+1 < len(input) && input[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(input) && input[i+1] == '*':
			inBlock = true
			i++
		case c == '"':
			inString = true
			out = append(out, c)
		case c == ',':
			// Drop the comma if the next meaningful byte closes a container.
			if next := nextMeaningful(input, i+1); next == '}' || next == ']' {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// nextMeaningful returns the next byte that is not whitespace or a comment,
// or 0 at end of input.
func nextMeaningful(input []byte, from int) byte {
	for i := from; i < len(input); i++ {
		switch c := input[i]; c {
		case ' ', '\t', '\r', '\n':
			continue
		case '/':
			if i+1 >= len(input) {
				return c
			}
			switch input[i+1] {
			case '/':
				for i < len(input) && input[i] != '\n' {
					i++
				}
			case '*':
				i += 2
				for i+1 < len(input) && !(input[i] == '*' && input[i+1] == '/') {
					i++
				}
				i++
			default:
				return c
			}
		default:
			return c
		}
	}
	return 0
}
