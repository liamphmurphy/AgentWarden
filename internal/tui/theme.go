// Package tui renders the terminal interface.
package tui

import (
	"os"
	"regexp"
	"strings"

	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// DetectTheme resolves the terminal's background once and pins it, returning
// the glamour style name to use.
//
// This MUST be called before the Bubble Tea program starts. Detecting a dark
// background means writing an OSC 11 query to the terminal and reading the
// reply (plus a cursor-position report termenv uses as a sentinel). Once Bubble
// Tea owns stdin, those replies are read as keystrokes and appear as literal
// junk like `]11;rgb:0000/0000/0000\[1;1R` in the input box.
//
// Calling lipgloss.SetHasDarkBackground marks the value explicit, so lipgloss
// never runs the query itself when an AdaptiveColor is first rendered.
func DetectTheme() string {
	// Not a terminal: no query is possible and none is wanted.
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		lipgloss.SetHasDarkBackground(true)
		return styles.NoTTYStyle
	}

	// An explicit override skips *our* detection and is the escape hatch when
	// a terminal answers the query badly. It does not eliminate the query
	// altogether: lipgloss builds its default renderer with
	// termenv.WithColorCache(true), which eagerly reads the background colour
	// the first time lipgloss is touched. That is exactly why every path
	// through this function must run before the program starts.
	switch strings.ToLower(os.Getenv("AGENTWARDEN_THEME")) {
	case "dark":
		lipgloss.SetHasDarkBackground(true)
		return styles.DarkStyle
	case "light":
		lipgloss.SetHasDarkBackground(false)
		return styles.LightStyle
	}

	// lipgloss caches this behind a sync.Once, so asking it rather than
	// termenv directly both performs the query and warms the cache that
	// AdaptiveColor would otherwise populate later, from inside the program.
	dark := lipgloss.HasDarkBackground()
	if dark {
		return styles.DarkStyle
	}
	return styles.LightStyle
}

// FrameRate drives the animation ticker. 30fps is smooth for spinners and
// elapsed-time counters without redrawing more than a terminal can show.
const FrameRate = 30

// Palette. Adaptive colors keep the interface legible on light and dark
// terminals without the user configuring anything.
var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#B5A6FF"}
	colorMuted  = lipgloss.AdaptiveColor{Light: "#6B6B78", Dark: "#8B8B99"}
	colorOK     = lipgloss.AdaptiveColor{Light: "#1F7A3D", Dark: "#5FD68A"}
	colorFail   = lipgloss.AdaptiveColor{Light: "#B02222", Dark: "#FF8080"}
	colorWarn   = lipgloss.AdaptiveColor{Light: "#9A6400", Dark: "#F5C161"}
	colorBorder = lipgloss.AdaptiveColor{Light: "#D0D0DA", Dark: "#3A3A46"}
)

var (
	styleUser   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleMuted  = lipgloss.NewStyle().Foreground(colorMuted)
	styleOK     = lipgloss.NewStyle().Foreground(colorOK)
	styleFail   = lipgloss.NewStyle().Foreground(colorFail)
	styleWarn   = lipgloss.NewStyle().Foreground(colorWarn)
	styleAccent = lipgloss.NewStyle().Foreground(colorAccent)

	stylePane = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	styleStatusBar = lipgloss.NewStyle().
			Foreground(colorMuted).
			BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(colorBorder)
)

// spinnerFrames is a braille spinner: it animates smoothly at 30fps and needs
// only one cell.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerFrame returns the frame for a tick count.
func spinnerFrame(tick int) string {
	if len(spinnerFrames) == 0 {
		return ""
	}
	// Advance roughly every 3 frames so the spinner reads as a spin rather
	// than a blur at 30fps.
	return spinnerFrames[(tick/3)%len(spinnerFrames)]
}

// Gate status glyphs.
const (
	glyphPass    = "✓"
	glyphFail    = "✗"
	glyphPending = "·"
)

// ansiEscape matches terminal styling sequences.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// plainText strips styling so rendered output can be copied as text.
func plainText(s string) string { return ansiEscape.ReplaceAllString(s, "") }
