package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lmurphy/agentwarden/internal/tool"
)

type confirmationDecision uint8

const (
	confirmationDeny confirmationDecision = iota
	confirmationAllowOnce
	confirmationAllowSession
)

type confirmationMsg struct {
	tool     string
	action   string
	resource string
	response chan<- confirmationDecision
}

type approvalKey struct {
	action   string
	resource string
}

// confirmer transfers a permission decision from the Bubble Tea goroutine to
// the model loop. The response channel is buffered so the UI never blocks if
// cancellation wins the race while a keypress is already being handled.
type confirmer struct {
	events chan<- tea.Msg

	mu      sync.RWMutex
	allowed map[approvalKey]struct{}
}

func newConfirmer(events chan<- tea.Msg) *confirmer {
	return &confirmer{
		events:  events,
		allowed: make(map[approvalKey]struct{}),
	}
}

// Confirm waits for the UI to answer or the run context to be cancelled.
func (c *confirmer) Confirm(
	ctx context.Context,
	call tool.Call,
	action string,
	resource string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	key := approvalKey{action: action, resource: resource}
	c.mu.RLock()
	_, allowed := c.allowed[key]
	c.mu.RUnlock()
	if allowed {
		return true, nil
	}

	response := make(chan confirmationDecision, 1)
	request := confirmationMsg{
		tool:     call.Name,
		action:   action,
		resource: resource,
		response: response,
	}
	select {
	case c.events <- request:
	case <-ctx.Done():
		return false, ctx.Err()
	}

	select {
	case decision := <-response:
		if decision == confirmationAllowSession {
			c.mu.Lock()
			c.allowed[key] = struct{}{}
			c.mu.Unlock()
		}
		return decision != confirmationDeny, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

type confirmationPane struct {
	tool     string
	action   string
	resource string
	cursor   int
	open     bool
}

type confirmationChoice struct {
	decision confirmationDecision
	label    string
	key      string
}

var confirmationChoices = []confirmationChoice{
	{decision: confirmationAllowOnce, label: "Allow once", key: "enter / y"},
	{decision: confirmationAllowSession, label: "Allow this exact target for the session", key: "a"},
	{decision: confirmationDeny, label: "Deny", key: "esc / n"},
}

func (p *confirmationPane) Open(toolName, action, resource string) {
	p.tool = toolName
	p.action = action
	p.resource = resource
	p.cursor = 0
	p.open = true
}

func (p *confirmationPane) Close() { p.open = false }

func (p *confirmationPane) IsOpen() bool { return p.open }

func (p *confirmationPane) Move(delta int) {
	p.cursor += delta
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(confirmationChoices) {
		p.cursor = len(confirmationChoices) - 1
	}
}

func (p *confirmationPane) Selected() confirmationDecision {
	if p.cursor < 0 || p.cursor >= len(confirmationChoices) {
		return confirmationDeny
	}
	return confirmationChoices[p.cursor].decision
}

func (p *confirmationPane) View(width int) string {
	if !p.open {
		return ""
	}
	inner := max(width-6, 20)
	resource := permissionPreview(p.resource)

	var b strings.Builder
	b.WriteString(styleWarn.Render("Permission required"))
	b.WriteString(styleMuted.Render(fmt.Sprintf("   %s wants to perform %s", p.tool, p.action)))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render(wrapPermissionText(resource, inner)))
	for i, choice := range confirmationChoices {
		b.WriteString("\n")
		marker, row := "  ", styleMuted
		if i == p.cursor {
			marker, row = "› ", styleAccent
		}
		b.WriteString(row.Render(fmt.Sprintf("%s%s  [%s]", marker, choice.label, choice.key)))
	}
	return stylePane.Render(b.String())
}

// permissionPreview keeps pathological generated shell commands from taking
// over the screen while showing both ends, where redirects and chained
// effects usually live.
func permissionPreview(resource string) string {
	const (
		maxRunes = 600
		head     = 400
		tail     = 160
	)
	resource = strings.TrimSpace(resource)
	runes := []rune(resource)
	if len(runes) <= maxRunes {
		return resource
	}
	omitted := len(runes) - head - tail
	return string(runes[:head]) +
		fmt.Sprintf("\n… %d characters omitted …\n", omitted) +
		string(runes[len(runes)-tail:])
}

func wrapPermissionText(text string, width int) string {
	if width < 1 {
		return text
	}
	var wrapped []string
	for _, line := range strings.Split(text, "\n") {
		runes := []rune(line)
		if len(runes) == 0 {
			wrapped = append(wrapped, "")
			continue
		}
		for len(runes) > width {
			wrapped = append(wrapped, string(runes[:width]))
			runes = runes[width:]
		}
		wrapped = append(wrapped, string(runes))
	}
	return strings.Join(wrapped, "\n")
}
