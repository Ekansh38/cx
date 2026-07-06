package ui

// Background memory curation on quit.
//
// The naive approach (run the curation in a tea.Cmd goroutine, then wait for
// it in main.go before exiting) made quitting cx feel slow — you'd sit at the
// old prompt for the length of an LLM round trip. Instead, we spawn a
// detached child process to run the curation, and the parent exits
// immediately. The child re-reads config.toml and memory.md on its own; it
// takes only the exchange text as input.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cx/config"
	"cx/llm"
	"cx/memory"
)

// flushState is written to a temp file and passed to the detached child.
// Everything the curation prompt template needs is precomputed here so the
// child only has to fill %s slots and call the LLM.
type flushState struct {
	ConvTitle string `json:"conv_title"`
	Today     string `json:"today"`
	Other     string `json:"other"`
	Already   string `json:"already"`
	Fresh     string `json:"fresh"`
}

// spawnMemoryFlush serializes a curation task and launches a detached copy of
// cx (`cx _curate <state>`) that does the LLM call and writes memory.md, then
// exits. Returns true when the child was launched.
func spawnMemoryFlush(state flushState) bool {
	if strings.TrimSpace(state.Fresh) == "" {
		return false
	}
	statePath := filepath.Join(config.DataDir(), fmt.Sprintf("flush-%d.json", time.Now().UnixNano()))
	data, err := json.Marshal(state)
	if err != nil {
		return false
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "_curate", statePath)
	// Detach: new session, no controlling terminal, no stdio inheritance.
	// Without this the child would be killed with the parent's tmux pane.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		os.Remove(statePath)
		return false
	}
	// Don't Wait: let it run independently.
	return true
}

// RunFlushChild is the `cx _curate <state>` subcommand entry point. It reads
// the state file, runs curation against the configured memory model, writes
// memory.md, and exits. Silent — this process has no terminal.
func RunFlushChild(statePath string) int {
	data, err := os.ReadFile(statePath)
	os.Remove(statePath)
	if err != nil {
		return 1
	}
	var s flushState
	if err := json.Unmarshal(data, &s); err != nil {
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	memModel := cfg.MemoryModel
	if memModel == "" {
		memModel = "google/gemini-2.5-flash"
	}
	prov, err := llm.ForModel(memModel, cfg)
	if err != nil {
		return 1
	}
	memPath := config.MemoryPath()
	existing, _ := memory.Raw(memPath)
	if existing == "" {
		existing = "(empty — this is a new memory file)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := prov.Complete(ctx, memModel, []llm.Message{{
		Role:    "user",
		Content: fmt.Sprintf(curationPrompt, s.ConvTitle, s.Today, existing, s.ConvTitle, s.Other, s.Already, s.Fresh),
	}})
	if err != nil {
		return 1
	}
	result = strings.TrimSpace(result)
	result = strings.TrimPrefix(result, "```markdown")
	result = strings.TrimPrefix(result, "```md")
	result = strings.TrimPrefix(result, "```")
	result = strings.TrimSuffix(result, "```")
	result = strings.TrimSpace(result)
	if result == "" || strings.HasPrefix(result, "NO_CHANGES") {
		return 0
	}
	// Same shrink guard as the interactive path: never wipe good memory on
	// a bad reply
	oldLines := strings.Count(existing, "\n") + 1
	newLines := strings.Count(result, "\n") + 1
	if oldLines >= 20 && newLines*3 < oldLines {
		return 0
	}
	if err := memory.SaveRaw(memPath, result); err != nil {
		return 1
	}
	return 0
}
