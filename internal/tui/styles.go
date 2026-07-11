package tui

import "github.com/charmbracelet/lipgloss"

// Palette centralizes the TUI's lipgloss styles so the view code reads as
// layout, not decoration.
var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	dirStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	cursorBg   = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	badgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	extStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))
)

// barColor returns a lipgloss color chosen by magnitude, mirroring the text
// renderer's severity coloring.
func barColor(pct float64) lipgloss.Color {
	switch {
	case pct >= 50:
		return lipgloss.Color("203") // red
	case pct >= 20:
		return lipgloss.Color("214") // orange
	case pct >= 5:
		return lipgloss.Color("45") // cyan
	default:
		return lipgloss.Color("42") // green
	}
}
