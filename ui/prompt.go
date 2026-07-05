package ui

// The base system prompt lives in system-prompt.md, embedded as the default
// and written to ~/.config/cx/system-prompt.md on first run so the user can
// edit it. Delete the file to reset to the shipped default.

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"

	"cx/config"
)

//go:embed system-prompt.md
var defaultSystemPrompt string

func systemPromptPath() string {
	return filepath.Join(config.Dir(), "system-prompt.md")
}

// loadBasePrompt reads the user's system prompt, seeding it with the shipped
// default on first run.
func loadBasePrompt() string {
	path := systemPromptPath()
	if data, err := os.ReadFile(path); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" {
			return p
		}
	}
	os.WriteFile(path, []byte(defaultSystemPrompt), 0o644)
	return strings.TrimSpace(defaultSystemPrompt)
}

// BuildSystemPrompt combines the base prompt with the memory file.
func BuildSystemPrompt(memory string) string {
	base := loadBasePrompt()
	if strings.TrimSpace(memory) == "" {
		return base
	}
	return base + "\n\n## Things you know about the user\n" + memory
}
