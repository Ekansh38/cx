package ui

import "github.com/charmbracelet/lipgloss"

// Terminal color palette — uses ANSI 256 for broad compatibility.
var (
	// User messages: bold cyan
	userStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	// Assistant role label
	assistantLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("5")) // magenta

	// User prompt prefix "> "
	promptStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	// Separator line
	sepStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238"))

	// Status bar: subtle background
	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			Background(lipgloss.Color("236"))

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

	// Dim text for picker meta info, :help, system annotations
	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("246"))

	// Picker title
	pickerTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("252"))

	// Streaming cursor
	cursorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6")).
			Bold(true)
)
