// Package openaicompat implements provider.Provider against any endpoint
// speaking the OpenAI /v1/chat/completions protocol: OpenAI itself, Ollama,
// vLLM, llama.cpp, LM Studio, LiteLLM and gateways in front of them.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lmurphy/agentwarden/internal/provider"
)

// defaultTimeout bounds a whole streamed response. Local models on cold start
// can be slow, so this is generous.
const defaultTimeout = 10 * time.Minute

// RequestLogger records outgoing payloads. Masking and tool_choice are only
// verifiable on the wire, so this is the mechanism behind --log-requests.
type RequestLogger interface {
	LogRequest(providerID string, body []byte)
}

// Client is an OpenAI-compatible endpoint.
type Client struct {
	id      string
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	extra   map[string]any
	http    *http.Client
	logger  RequestLogger
}

// Options configures a Client.
type Options struct {
	ID      string
	Name    string
	BaseURL string
	APIKey  string
	Headers map[string]string
	// Extra is merged into every request body for endpoint-specific knobs
	// such as llama.cpp grammars or vLLM guided decoding.
	Extra  map[string]any
	HTTP   *http.Client
	Logger RequestLogger
}

// New returns a Client.
func New(opts Options) *Client {
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	name := opts.Name
	if name == "" {
		name = opts.ID
	}
	return &Client{
		id:      opts.ID,
		name:    name,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		apiKey:  opts.APIKey,
		headers: opts.Headers,
		extra:   opts.Extra,
		http:    httpClient,
		logger:  opts.Logger,
	}
}

// Name identifies the provider.
func (c *Client) Name() string { return c.name }

// wire types -----------------------------------------------------------------

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Index    *int         `json:"index,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireToolSchema `json:"function"`
}

type wireToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// buildBody renders the request payload. Extra keys are merged last so an
// endpoint-specific knob can override a default.
func (c *Client) buildBody(req provider.Request) (map[string]any, error) {
	messages := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		wm := wireMessage{
			Role:       string(m.Role),
			Content:    m.Text,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireFunction{Name: tc.Name, Arguments: tc.Args},
			})
		}
		messages = append(messages, wm)
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   true,
		// A streamed response carries no usage unless it is asked for, and
		// the interface reports token spend and context pressure from it. An
		// endpoint that rejects the field outright can drop it with
		// "stream_options": null under its "extra" config.
		"stream_options": map[string]any{"include_usage": true},
	}

	if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, wireTool{
				Type: "function",
				Function: wireToolSchema{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
		body["tools"] = tools
	}

	if tc := req.ToolChoice; tc != nil {
		switch tc.Mode {
		case provider.ToolChoiceFunction:
			if tc.Name == "" {
				return nil, fmt.Errorf("tool_choice function requires a name")
			}
			// Pinning the next call is what makes delegation reliable on
			// small models, so it is expressed explicitly rather than as
			// "required".
			body["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": tc.Name},
			}
		case provider.ToolChoiceAuto, provider.ToolChoiceNone, provider.ToolChoiceRequired:
			body["tool_choice"] = string(tc.Mode)
		}
	}

	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	mergeExtra(body, c.extra)
	mergeExtra(body, req.Extra)
	return body, nil
}

// mergeExtra folds endpoint-specific knobs into a request body.
//
// An explicit null removes the key instead of sending null. That is the only
// way to satisfy an endpoint which rejects an unknown field rather than
// ignoring it, since a value of false still sends the field.
func mergeExtra(body map[string]any, extra map[string]any) {
	for k, v := range extra {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
}

// Stream issues the request and returns its event stream.
func (c *Client) Stream(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body, err := c.buildBody(req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if c.logger != nil {
		c.logger.LogRequest(c.id, raw)
	}

	url := c.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for name, value := range c.headers {
		httpReq.Header.Set(name, value)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		// Include the body: endpoints report model-not-found and context
		// overflow here, and the message is the only useful diagnostic.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s returned %s: %s", url, resp.Status, strings.TrimSpace(string(snippet)))
	}
	return newStream(resp.Body), nil
}
