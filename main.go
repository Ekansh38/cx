package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"cx/config"
	"cx/llm"
	"cx/store"
	"cx/ui"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	dbPath := filepath.Join(config.DataDir(), "cx.db")
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	// Resolve model and provider
	model := cfg.Model
	if m := os.Getenv("CX_MODEL"); m != "" {
		model = m
	}
	prov, err := llm.ForModel(model, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provider: %v\n", err)
		os.Exit(1)
	}

	// Load or create conversation
	conv, err := st.MostRecent()
	if err != nil {
		log.Fatal(err)
	}
	if conv == nil {
		conv, err = st.CreateConversation(model)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Load messages
	msgs, err := st.GetMessages(conv.ID)
	if err != nil {
		log.Fatal(err)
	}

	// Build system prompt (default + memory.md)
	sysPrompt := buildSystemPrompt(config.LoadMemory())

	// Launch TUI
	m := ui.New(cfg, st, conv, msgs, prov, model, sysPrompt)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
}

func buildSystemPrompt(memory string) string {
	base := "You are a helpful, direct, and thoughtful assistant. Be concise unless detail is needed."
	if strings.TrimSpace(memory) == "" {
		return base
	}
	return base + "\n\n" + memory
}
