package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"cx/config"
	"cx/llm"
	"cx/store"
	"cx/ui"
)

func main() {
	// cx doc [file] — auto-launch a split: editor on the left, cx on the right.
	// Without a file, cx starts in the fuzzy document picker.
	var docPath string
	startDocPicker := false
	if len(os.Args) >= 2 && os.Args[1] == "doc" {
		if len(os.Args) >= 3 {
			docPath = launchDocSplit(os.Args[2])
		} else {
			ensureTmuxSession("doc") // so the editor split can open after picking
			startDocPicker = true
		}
	}

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

	// Load or create conversation
	conv, err := st.MostRecent()
	if err != nil {
		log.Fatal(err)
	}

	// Resolve model: env var > last explicitly selected > most recent
	// conversation > config > fallback
	model := cfg.Model
	if conv != nil && conv.Model != "" {
		model = conv.Model
	}
	if lm := config.LastModel(); lm != "" {
		model = lm
	}
	if m := os.Getenv("CX_MODEL"); m != "" {
		model = m
	}
	if model == "" {
		model = "llama3.2" // last resort fallback
	}

	prov, err := llm.ForModel(model, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provider: %v\n", err)
		os.Exit(1)
	}

	if conv == nil {
		conv, err = st.CreateConversation(model)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Doc mode: reuse the conversation already connected to this file, or start one
	if docPath != "" {
		dc, _ := st.FindConversationByDoc(docPath)
		if dc == nil {
			dc, err = st.CreateConversation(model)
			if err != nil {
				log.Fatal(err)
			}
			st.AddDoc(dc.ID, docPath)
		}
		ui.SaveLastDoc(docPath)
		conv = dc
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
	if startDocPicker {
		m = m.StartInDocPicker()
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
}

// ensureTmuxSession re-execs cx inside a new tmux session when run outside
// one (and exits). Inside tmux, or when tmux isn't installed, it's a no-op —
// without tmux the doc view still works, just without the editor split.
func ensureTmuxSession(args ...string) {
	if os.Getenv("TMUX") != "" {
		return
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command("tmux", append([]string{"new-session", exe}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// launchDocSplit resolves the doc path and sets up the tmux split view.
// Outside tmux it re-execs itself inside a new tmux session and exits;
// inside tmux it opens the editor in a left pane and returns the path.
func launchDocSplit(path string) string {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doc: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "doc: %v\n", err)
		os.Exit(1)
	}

	ensureTmuxSession("doc", abs)

	// Inside tmux — open the editor in a pane to the left of cx,
	// wired up with the cx bridge (RPC socket + review lua) when it's neovim
	args := append([]string{"split-window", "-h", "-b", "-l", "70%"}, ui.EditorArgs(abs)...)
	exec.Command("tmux", args...).Run()
	return abs
}

func buildSystemPrompt(memory string) string {
	base := "You are a helpful, direct, and thoughtful buddy. No need to sugar-coat, be BLUNT. No need to agree for no reason."
	if strings.TrimSpace(memory) == "" {
		return base
	}
	return base + "\n\n## Things you know about the user\n" + memory
}
