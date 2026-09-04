package tui

import "github.com/charmbracelet/lipgloss"

// lipglossWidth reports the rendered cell width of s.
func lipglossWidth(s string) int { return lipgloss.Width(s) }

// lipglossHeight reports the rendered line count of s.
func lipglossHeight(s string) int {
	if s == "" {
		return 0
	}
	return lipgloss.Height(s)
}
