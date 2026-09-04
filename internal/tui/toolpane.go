package tui

import (
	"fmt"
	"strings"
)

// toolCallView is the user-facing record of one invocation. It is kept apart
// from tool.Call so the transcript can retain a compact header while exposing
// the raw arguments and returned content on demand.
type toolCallView struct {
	Name            string
	Args            string
	Content         string
	Running         bool
	IsError         bool
	Collapsed       bool
	TranscriptIndex int
}

const toolDetailLines = 8

// renderToolCall renders a clickable transcript block for one invocation.
func (m *Model) renderToolCall(index int) string {
	call := m.toolCalls[index]
	marker := "▾"
	if call.Collapsed {
		marker = "▸"
	}
	glyph, state := glyphPending, "running"
	if !call.Running {
		glyph, state = glyphPass, "ok"
		if call.IsError {
			glyph, state = glyphFail, "failed"
		}
	}
	header := fmt.Sprintf("  %s %s %s %s", styleMuted.Render(marker),
		statusGlyph(glyph, call.IsError), styleMuted.Render(call.Name),
		styleMuted.Render(state))
	if call.Collapsed {
		return header
	}

	rows := []string{header}
	if strings.TrimSpace(call.Args) != "" {
		rows = append(rows, "    "+styleMuted.Render("args: "+truncate(call.Args, 100)))
	}
	if strings.TrimSpace(call.Content) != "" {
		lines := splitToolContent(call.Content)
		if len(lines) > toolDetailLines {
			lines = lines[:toolDetailLines]
		}
		for _, line := range lines {
			style := styleMuted
			if call.IsError {
				style = styleFail
			}
			rows = append(rows, "    "+style.Render(truncate(line, 100)))
		}
	}
	return strings.Join(rows, "\n")
}

func statusGlyph(glyph string, failed bool) string {
	if failed {
		return styleFail.Render(glyph)
	}
	if glyph == glyphPass {
		return styleOK.Render(glyph)
	}
	return styleAccent.Render(glyph)
}

func splitToolContent(content string) []string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
