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

// LoadBasePromptOnly returns just the base personality prompt, with no
// memory.md or external memory injection. Used by incognito mode so the
// model is still a decent assistant but knows nothing about the user.
func LoadBasePromptOnly() string {
	return loadBasePrompt()
}

// BuildSystemPrompt combines the base prompt with the memory file and any
// external memory files the user has attached via /mem.
func BuildSystemPrompt(memory string) string {
	base := loadBasePrompt()
	parts := []string{base}
	if strings.TrimSpace(memory) != "" {
		parts = append(parts, "## Things you know about the user\n"+memory)
	}
	if ext := externalMemorySection(); ext != "" {
		parts = append(parts, ext)
	}
	return strings.Join(parts, "\n\n")
}

// externalMemorySection lists external memory files (added via /mem) with
// their full contents inline. READ-ONLY context: the model is told not to
// propose edits against these paths — they're the user's own notes, curated
// by hand, not something cx should rewrite.
func externalMemorySection() string {
	paths := config.LoadExternalMemoryPaths()
	if len(paths) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## External memory files (read-only)\n")
	sb.WriteString("These are the user's persistent external notes, curated by hand across sessions. Their full contents are shown below so you have the context. Treat them as authoritative for the topics they cover, but do NOT propose `<edit>` blocks against these paths — the user maintains them themselves.\n\n")
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := strings.TrimRight(string(data), "\n")
		sb.WriteString("### " + p + "\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}
