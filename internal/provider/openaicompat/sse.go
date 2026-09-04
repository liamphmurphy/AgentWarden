package openaicompat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// maxLine bounds a single SSE line. Tool arguments can be large, so this is
// well above the default scanner limit.
const maxLine = 8 * 1024 * 1024

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// pending accumulates one tool call whose name and arguments arrive across
// many chunks.
type pending struct {
	id   string
	name string
	args strings.Builder
}

// stream decodes an SSE response into provider events.
//
// Tool calls are emitted only once the stream finishes, because a call's
// arguments arrive as fragments spread over many chunks and are not valid JSON
// until the last one lands.
type stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner

	// queue holds events ready to hand to the caller.
	queue []provider.Event
	// calls accumulates partial tool calls in arrival order.
	calls []*pending
	// byIndex and byID locate a partial call as fragments arrive. Endpoints
	// differ: some send a stable index, others only repeat the id.
	byIndex map[int]*pending
	byID    map[string]*pending

	usage      *provider.Usage
	stopReason string
	finished   bool
	done       bool
}

func newStream(body io.ReadCloser) *stream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	return &stream{
		body:    body,
		scanner: scanner,
		byIndex: map[int]*pending{},
		byID:    map[string]*pending{},
	}
}

// Recv returns the next event, or io.EOF when the stream is exhausted.
func (s *stream) Recv() (provider.Event, error) {
	for {
		if len(s.queue) > 0 {
			event := s.queue[0]
			s.queue = s.queue[1:]
			return event, nil
		}
		if s.done {
			return provider.Event{}, io.EOF
		}
		if s.finished {
			s.flush()
			s.done = true
			continue
		}
		if err := s.readMore(); err != nil {
			return provider.Event{}, err
		}
	}
}

// readMore consumes SSE lines until at least one event is queued or the stream
// ends.
func (s *stream) readMore() error {
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			// Ignore other SSE fields such as event: or id:.
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			s.finished = true
			return nil
		}

		var c chunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			// A single unparseable chunk should not abort a long generation.
			continue
		}
		s.consume(c)
		if len(s.queue) > 0 {
			return nil
		}
	}

	if err := s.scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	// The endpoint closed without sending [DONE].
	s.finished = true
	return nil
}

// consume folds one chunk into the accumulating state.
func (s *stream) consume(c chunk) {
	if c.Usage != nil {
		s.usage = &provider.Usage{
			PromptTokens:     c.Usage.PromptTokens,
			CompletionTokens: c.Usage.CompletionTokens,
		}
	}
	for _, choice := range c.Choices {
		if text := choice.Delta.Content; text != "" {
			s.queue = append(s.queue, provider.Event{Kind: provider.EventText, Text: text})
		}
		for _, tc := range choice.Delta.ToolCalls {
			s.mergeToolCall(tc)
		}
		if choice.FinishReason != "" {
			s.stopReason = choice.FinishReason
		}
	}
}

// mergeToolCall routes a fragment to the call it belongs to, creating one on
// first sight.
func (s *stream) mergeToolCall(tc wireToolCall) {
	var p *pending

	switch {
	case tc.Index != nil:
		if existing, ok := s.byIndex[*tc.Index]; ok {
			p = existing
		}
	case tc.ID != "":
		if existing, ok := s.byID[tc.ID]; ok {
			p = existing
		}
	default:
		// No index and no id: assume it continues the most recent call.
		if len(s.calls) > 0 {
			p = s.calls[len(s.calls)-1]
		}
	}

	if p == nil {
		p = &pending{id: tc.ID, name: tc.Function.Name}
		s.calls = append(s.calls, p)
		if tc.Index != nil {
			s.byIndex[*tc.Index] = p
		}
		if tc.ID != "" {
			s.byID[tc.ID] = p
		}
	}

	// Later fragments may be the first to carry the id or the name.
	if p.id == "" && tc.ID != "" {
		p.id = tc.ID
		s.byID[tc.ID] = p
	}
	if tc.Function.Name != "" && p.name == "" {
		p.name = tc.Function.Name
	}
	p.args.WriteString(tc.Function.Arguments)
}

// flush emits the assembled tool calls followed by the terminating event.
func (s *stream) flush() {
	for i, p := range s.calls {
		if p.name == "" {
			// A fragment stream that never named a tool is unusable.
			continue
		}
		args := p.args.String()
		if strings.TrimSpace(args) == "" {
			// A no-argument call still needs valid JSON downstream.
			args = "{}"
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		s.queue = append(s.queue, provider.Event{
			Kind:     provider.EventToolCall,
			ToolCall: &provider.ToolCall{ID: id, Name: p.name, Args: args},
		})
	}
	s.queue = append(s.queue, provider.Event{
		Kind:       provider.EventDone,
		Usage:      s.usage,
		StopReason: s.stopReason,
	})
}

// Close releases the underlying response body.
func (s *stream) Close() error { return s.body.Close() }
