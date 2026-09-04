package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// sseServer replays a canned SSE body and captures the request it received.
func sseServer(t *testing.T, events []string) (*httptest.Server, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func data(payload string) string { return "data: " + payload + "\n\n" }

func collect(t *testing.T, s provider.Stream) []provider.Event {
	t.Helper()
	var out []provider.Event
	for {
		event, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, event)
	}
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	return New(Options{ID: "test", BaseURL: url, HTTP: &http.Client{}})
}

func TestStreamText(t *testing.T) {
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"content":"Hello"}}]}`),
		data(`{"choices":[{"delta":{"content":", world"}}]}`),
		data(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	events := collect(t, stream)
	var text strings.Builder
	var done *provider.Event
	for i, e := range events {
		switch e.Kind {
		case provider.EventText:
			text.WriteString(e.Text)
		case provider.EventDone:
			done = &events[i]
		}
	}
	if text.String() != "Hello, world" {
		t.Errorf("text = %q", text.String())
	}
	if done == nil {
		t.Fatal("expected a done event")
	}
	if done.StopReason != "stop" {
		t.Errorf("stop reason = %q", done.StopReason)
	}
	if done.Usage == nil || done.Usage.PromptTokens != 7 || done.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", done.Usage)
	}
}

// TestToolCallAssembledAcrossChunks is the property that breaks most naive
// clients: a call's name and arguments arrive as fragments and are not valid
// JSON until the last one lands.
func TestToolCallAssembledAcrossChunks(t *testing.T) {
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}}]}`),
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`),
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a"}}]}}]}`),
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".go\"}"}}]}}]}`),
		data(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var calls []provider.ToolCall
	for _, e := range collect(t, stream) {
		if e.Kind == provider.EventToolCall {
			calls = append(calls, *e.ToolCall)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "read" {
		t.Errorf("call = %+v", calls[0])
	}
	// The assembled arguments must be valid JSON.
	var args struct{ Path string }
	if err := json.Unmarshal([]byte(calls[0].Args), &args); err != nil {
		t.Fatalf("assembled args are not valid JSON (%q): %v", calls[0].Args, err)
	}
	if args.Path != "a.go" {
		t.Errorf("path = %q, want a.go", args.Path)
	}
}

func TestParallelToolCallsKeptSeparate(t *testing.T) {
	// SSE requires each data: payload on a single line.
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"read","arguments":"{\"path\":"}},{"index":1,"id":"c1","function":{"name":"grep","arguments":"{\"q\":"}}]}}]}`),
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}},{"index":1,"function":{"arguments":"\"foo\"}"}}]}}]}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	got := map[string]string{}
	for _, e := range collect(t, stream) {
		if e.Kind == provider.EventToolCall {
			got[e.ToolCall.Name] = e.ToolCall.Args
		}
	}
	if got["read"] != `{"path":"a.go"}` {
		t.Errorf("read args = %q", got["read"])
	}
	if got["grep"] != `{"q":"foo"}` {
		t.Errorf("grep args = %q", got["grep"])
	}
}

// TestToolCallWithoutIndex covers endpoints that repeat the id instead of
// sending a stable index.
func TestToolCallWithoutIndex(t *testing.T) {
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`),
		data(`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"arguments":"\"a.go\"}"}}]}}]}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var calls []provider.ToolCall
	for _, e := range collect(t, stream) {
		if e.Kind == provider.EventToolCall {
			calls = append(calls, *e.ToolCall)
		}
	}
	if len(calls) != 1 || calls[0].Args != `{"path":"a.go"}` {
		t.Fatalf("fragments keyed only by id should merge, got %+v", calls)
	}
}

func TestEmptyArgumentsBecomeValidJSON(t *testing.T) {
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"status"}}]}}]}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	for _, e := range collect(t, stream) {
		if e.Kind == provider.EventToolCall {
			if e.ToolCall.Args != "{}" {
				t.Errorf("a no-argument call should yield {}, got %q", e.ToolCall.Args)
			}
			return
		}
	}
	t.Fatal("expected a tool call")
}

// TestMalformedChunkIsSkipped: one bad chunk should not abort a long
// generation.
func TestMalformedChunkIsSkipped(t *testing.T) {
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"content":"a"}}]}`),
		data(`{not json at all`),
		data(`{"choices":[{"delta":{"content":"b"}}]}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	for _, e := range collect(t, stream) {
		if e.Kind == provider.EventText {
			text.WriteString(e.Text)
		}
	}
	if text.String() != "ab" {
		t.Errorf("text = %q, want the surviving deltas", text.String())
	}
}

func TestStreamEndsWithoutDoneSentinel(t *testing.T) {
	srv, _ := sseServer(t, []string{
		data(`{"choices":[{"delta":{"content":"partial"}}]}`),
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	events := collect(t, stream)
	if len(events) == 0 || events[len(events)-1].Kind != provider.EventDone {
		t.Error("a truncated stream should still terminate with a done event")
	}
}

func TestCommentAndUnknownFieldsIgnored(t *testing.T) {
	srv, _ := sseServer(t, []string{
		": keepalive comment\n\n",
		"event: message\n",
		data(`{"choices":[{"delta":{"content":"x"}}]}`),
		"data: [DONE]\n\n",
	})

	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	for _, e := range collect(t, stream) {
		if e.Kind == provider.EventText && e.Text == "x" {
			return
		}
	}
	t.Error("SSE comments and unknown fields should be skipped, not fatal")
}

// TestToolChoiceOnTheWire is the check that the whole small-model strategy
// depends on: the pin must actually reach the endpoint in the documented shape.
func TestToolChoiceOnTheWire(t *testing.T) {
	tests := []struct {
		name   string
		choice *provider.ToolChoice
		verify func(t *testing.T, body map[string]any)
	}{
		{
			name:   "pinned function",
			choice: &provider.ToolChoice{Mode: provider.ToolChoiceFunction, Name: "task"},
			verify: func(t *testing.T, body map[string]any) {
				tc, ok := body["tool_choice"].(map[string]any)
				if !ok {
					t.Fatalf("tool_choice = %#v, want an object", body["tool_choice"])
				}
				if tc["type"] != "function" {
					t.Errorf("type = %v", tc["type"])
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok || fn["name"] != "task" {
					t.Errorf("function = %#v, want name task", tc["function"])
				}
			},
		},
		{
			name:   "auto",
			choice: &provider.ToolChoice{Mode: provider.ToolChoiceAuto},
			verify: func(t *testing.T, body map[string]any) {
				if body["tool_choice"] != "auto" {
					t.Errorf("tool_choice = %v, want auto", body["tool_choice"])
				}
			},
		},
		{
			name:   "required",
			choice: &provider.ToolChoice{Mode: provider.ToolChoiceRequired},
			verify: func(t *testing.T, body map[string]any) {
				if body["tool_choice"] != "required" {
					t.Errorf("tool_choice = %v, want required", body["tool_choice"])
				}
			},
		},
		{
			name:   "unset",
			choice: nil,
			verify: func(t *testing.T, body map[string]any) {
				if _, present := body["tool_choice"]; present {
					t.Error("tool_choice should be omitted when not constrained")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
			stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{
				Model:      "m",
				ToolChoice: tc.choice,
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			stream.Close()

			var body map[string]any
			if err := json.Unmarshal(*captured, &body); err != nil {
				t.Fatalf("captured body: %v", err)
			}
			tc.verify(t, body)
		})
	}
}

func TestPinnedToolChoiceRequiresName(t *testing.T) {
	srv, _ := sseServer(t, []string{"data: [DONE]\n\n"})
	_, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{
		Model:      "m",
		ToolChoice: &provider.ToolChoice{Mode: provider.ToolChoiceFunction},
	})
	if err == nil {
		t.Error("pinning without a tool name should be rejected")
	}
}

// TestMaskedToolsOnTheWire proves masking reaches the payload: only the tools
// handed to the client appear in the request.
func TestMaskedToolsOnTheWire(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{
		Model: "m",
		Tools: []provider.ToolDef{
			{Name: "read", Description: "read a file", Parameters: map[string]any{"type": "object"}},
			{Name: "task", Description: "delegate"},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("captured body: %v", err)
	}
	if len(body.Tools) != 2 {
		t.Fatalf("want 2 tools on the wire, got %d", len(body.Tools))
	}
	for _, tool := range body.Tools {
		if tool.Type != "function" {
			t.Errorf("tool type = %q", tool.Type)
		}
	}
	if body.Tools[0].Function.Name != "read" || body.Tools[1].Function.Name != "task" {
		t.Errorf("tool order should be preserved: %+v", body.Tools)
	}
}

func TestToolsOmittedWhenEmpty(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body map[string]any
	json.Unmarshal(*captured, &body)
	if _, present := body["tools"]; present {
		t.Error("an empty tool set should be omitted entirely")
	}
}

func TestMessagesAndToolResultsSerialized(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	stream, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{
		Model: "m",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Text: "be helpful"},
			{Role: provider.RoleUser, Text: "hi"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "read", Args: `{"path":"a.go"}`},
			}},
			{Role: provider.RoleTool, ToolCallID: "c1", Name: "read", Text: "file contents"},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body struct {
		Messages []wireMessage `json:"messages"`
	}
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("captured body: %v", err)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d", len(body.Messages))
	}
	assistant := body.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != "read" {
		t.Errorf("assistant tool call not serialized: %+v", assistant)
	}
	result := body.Messages[3]
	if result.Role != "tool" || result.ToolCallID != "c1" || result.Content != "file contents" {
		t.Errorf("tool result not serialized: %+v", result)
	}
}

func TestHeadersAndAPIKeySent(t *testing.T) {
	var gotAuth, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("x-api-key")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New(Options{
		ID:      "gw",
		BaseURL: srv.URL,
		APIKey:  "bearer-token",
		Headers: map[string]string{"x-api-key": "gateway-key"},
		HTTP:    &http.Client{},
	})
	stream, err := client.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	if gotAuth != "Bearer bearer-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCustom != "gateway-key" {
		t.Errorf("x-api-key = %q", gotCustom)
	}
}

// TestExtraMergedForGuidedDecoding covers the passthrough that fixes
// malformed tool JSON on local endpoints.
func TestExtraMergedForGuidedDecoding(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	client := New(Options{
		ID:      "vllm",
		BaseURL: srv.URL,
		Extra:   map[string]any{"provider_level": true, "shared": "from-provider"},
		HTTP:    &http.Client{},
	})
	stream, err := client.Stream(context.Background(), provider.Request{
		Model: "m",
		Extra: map[string]any{"guided_json": map[string]any{"type": "object"}, "shared": "from-request"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("captured body: %v", err)
	}
	if body["provider_level"] != true {
		t.Error("provider-level extras should be sent")
	}
	if _, ok := body["guided_json"]; !ok {
		t.Error("request extras should be sent")
	}
	// Request extras are merged last so a per-call knob wins.
	if body["shared"] != "from-request" {
		t.Errorf("shared = %v, want the request value to win", body["shared"])
	}
}

// TestErrorResponseIncludesBody: endpoints report model-not-found and context
// overflow in the body, which is the only useful diagnostic.
func TestErrorResponseIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"model \"nope\" not found"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Stream(context.Background(), provider.Request{Model: "nope"})
	if err == nil {
		t.Fatal("a non-200 response should be an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should include the response body: %v", err)
	}
}

// recordingLogger captures payloads for the --log-requests path.
type recordingLogger struct{ bodies [][]byte }

func (l *recordingLogger) LogRequest(_ string, body []byte) {
	l.bodies = append(l.bodies, body)
}

func TestRequestLogger(t *testing.T) {
	srv, _ := sseServer(t, []string{"data: [DONE]\n\n"})
	logger := &recordingLogger{}
	client := New(Options{ID: "test", BaseURL: srv.URL, HTTP: &http.Client{}, Logger: logger})

	stream, err := client.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	if len(logger.bodies) != 1 {
		t.Fatalf("want 1 logged request, got %d", len(logger.bodies))
	}
	if !strings.Contains(string(logger.bodies[0]), `"model":"m"`) {
		t.Errorf("logged body should be the real payload: %s", logger.bodies[0])
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	srv, _ := sseServer(t, []string{"data: [DONE]\n\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := newTestClient(t, srv.URL).Stream(ctx, provider.Request{Model: "m"}); err == nil {
		t.Error("a cancelled context should fail the request")
	}
}

func TestNameFallsBackToID(t *testing.T) {
	if got := New(Options{ID: "ollama"}).Name(); got != "ollama" {
		t.Errorf("Name() = %q, want the id", got)
	}
	if got := New(Options{ID: "ollama", Name: "Ollama"}).Name(); got != "Ollama" {
		t.Errorf("Name() = %q, want the label", got)
	}
}

var _ provider.Provider = (*Client)(nil)

// A streamed response carries no usage unless it is requested, and the status
// panel reports token spend and context pressure from it.
func TestStreamRequestsUsage(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	stream, err := newTestClient(t, srv.URL).Stream(context.Background(),
		provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("captured body: %v", err)
	}
	opts, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want an object", body["stream_options"])
	}
	if opts["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", opts["include_usage"])
	}
}

// An endpoint that rejects an unknown field cannot be satisfied by sending
// false, so a null in "extra" must remove the key altogether.
func TestExtraNullRemovesDefault(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	client := New(Options{
		ID:      "test",
		BaseURL: srv.URL,
		HTTP:    &http.Client{},
		Extra:   map[string]any{"stream_options": nil},
	})
	stream, err := client.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("captured body: %v", err)
	}
	if _, present := body["stream_options"]; present {
		t.Errorf("stream_options survived a null override: %#v", body["stream_options"])
	}
	// The removal must not take the rest of the request with it.
	if body["model"] != "m" {
		t.Errorf("model = %v, want m", body["model"])
	}
}

// A per-request null removes a provider-level default too, since the request
// is merged last.
func TestRequestExtraNullRemovesProviderExtra(t *testing.T) {
	srv, captured := sseServer(t, []string{"data: [DONE]\n\n"})
	client := New(Options{
		ID:      "test",
		BaseURL: srv.URL,
		HTTP:    &http.Client{},
		Extra:   map[string]any{"guided_json": map[string]any{"type": "object"}},
	})
	stream, err := client.Stream(context.Background(), provider.Request{
		Model: "m",
		Extra: map[string]any{"guided_json": nil},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("captured body: %v", err)
	}
	if _, present := body["guided_json"]; present {
		t.Errorf("guided_json survived a null override")
	}
}
