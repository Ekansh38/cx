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
			Foreground(lipgloss.Color("240"))

	// Status bar: plain dim gray, no background
	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))

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

	// Dim text for picker meta info (age) and :help output
	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("246"))

	// Picker title
	pickerTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("252"))
)
