package agent

import "encoding/json"

// unmarshalLoose decodes JSON, tolerating an empty argument string so a
// no-argument tool call is not reported as malformed.
func unmarshalLoose(raw string, into any) error {
	if raw == "" {
		raw = "{}"
	}
	return json.Unmarshal([]byte(raw), into)
}
