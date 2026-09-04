// Package fake provides a scripted Provider for deterministic tests.
//
// It records every request it receives, which is how a test asserts that tool
// masking and tool_choice actually reached the payload, and it replays a
// prepared sequence of turns so an enforcement scenario runs with no network
// and no model.
package fake

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// Turn is one scripted model response.
type Turn struct {
	// Text is emitted as a single text delta when non-empty.
	Text string
	// ToolCalls are emitted after the text.
	ToolCalls []provider.ToolCall
	// StopReason is reported on the done event.
	StopReason string
	// Err, when set, makes Stream fail for this turn.
	Err error
}

// TextTurn is a turn that only speaks.
func TextTurn(text string) Turn {
	return Turn{Text: text, StopReason: "stop"}
}

// CallTurn is a turn that requests one tool call.
func CallTurn(id, name, args string) Turn {
	return Turn{
		ToolCalls:  []provider.ToolCall{{ID: id, Name: name, Args: args}},
		StopReason: "tool_calls",
	}
}

// Provider replays scripted turns and records the requests it saw.
type Provider struct {
	mu       sync.Mutex
	turns    []Turn
	index    int
	requests []provider.Request
}

// New returns a Provider that will replay turns in order.
func New(turns ...Turn) *Provider {
	return &Provider{turns: turns}
}

// Name identifies the provider.
func (p *Provider) Name() string { return "fake" }

// Requests returns every request received, in order.
func (p *Provider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.requests...)
}

// LastRequest returns the most recent request.
func (p *Provider) LastRequest() (provider.Request, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		return provider.Request{}, false
	}
	return p.requests[len(p.requests)-1], true
}

// Remaining reports how many scripted turns are unused, so a test can assert
// the scenario played out as intended.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.turns) - p.index
}

// Stream records the request and replays the next scripted turn.
func (p *Provider) Stream(_ context.Context, req provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, req)
	if p.index >= len(p.turns) {
		return nil, fmt.Errorf("fake provider: no scripted turn for request %d", len(p.requests))
	}
	turn := p.turns[p.index]
	p.index++

	if turn.Err != nil {
		return nil, turn.Err
	}
	return newStream(turn), nil
}

// stream replays one turn's events.
type stream struct {
	events []provider.Event
	pos    int
}

func newStream(turn Turn) *stream {
	var events []provider.Event
	if turn.Text != "" {
		events = append(events, provider.Event{Kind: provider.EventText, Text: turn.Text})
	}
	for i := range turn.ToolCalls {
		call := turn.ToolCalls[i]
		events = append(events, provider.Event{Kind: provider.EventToolCall, ToolCall: &call})
	}
	events = append(events, provider.Event{
		Kind:       provider.EventDone,
		StopReason: turn.StopReason,
		Usage:      &provider.Usage{PromptTokens: 1, CompletionTokens: 1},
	})
	return &stream{events: events}
}

// Recv returns the next event or io.EOF.
func (s *stream) Recv() (provider.Event, error) {
	if s.pos >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	event := s.events[s.pos]
	s.pos++
	return event, nil
}

// Close is a no-op.
func (s *stream) Close() error { return nil }

// ToolNames returns the tool names offered in a request, for masking
// assertions.
func ToolNames(req provider.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	return out
}

// OffersTool reports whether a request advertised a tool.
func OffersTool(req provider.Request, name string) bool {
	for _, t := range req.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

var _ provider.Provider = (*Provider)(nil)
