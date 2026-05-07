package ui

import "github.com/charmbracelet/lipgloss"

// Terminal color palette — uses ANSI 256 for broad compatibility.
var (
	// User messages: bold cyan
	userStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	// User prompt prefix "> "
	promptStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	// Separator line
	sepStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238"))

	// Status bar: dim, no background
	statusStyle = lipgloss.NewStyle().
			Faint(true).
			Foreground(lipgloss.Color("240"))

	// Command completions shown on separator row
	completionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")) // amber

	// Error text
	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	// Picker selected row
	pickerSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("6"))

	// Picker normal row
	pickerRowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	// Dim text for picker meta info (age)
	dimStyle = lipgloss.NewStyle().
			Faint(true).
			Foreground(lipgloss.Color("240"))

	// Picker title
	pickerTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("252"))
)
