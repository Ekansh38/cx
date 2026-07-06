package ui

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"cx/config"
	"cx/llm"
)

func TestLiveAgentLoop(t *testing.T) {
	if os.Getenv("CX_LIVE") == "" {
		t.Skip("live test")
	}
	cfg, err := config.Load()
	if err != nil || cfg.OpenRouter.APIKey == "" {
		t.Skip("no key")
	}
	model := "anthropic/claude-haiku-4.5"
	prov, err := llm.ForModel(model, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	msgs := []llm.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "search the web for what day of the week it is today and answer in one short sentence"},
	}
	rounds, searched := 0, false
	for round := 0; round < 4; round++ {
		rounds++
		content, calls, err := prov.StreamTools(ctx, model, msgs, webTools(), func(string) {})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(calls) == 0 {
			fmt.Printf("FINAL (%d rounds): %s\n", rounds, content)
			if !searched {
				t.Fatal("model never called web_search")
			}
			return
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: content, ToolCalls: calls})
		for _, c := range calls {
			status, result := execWebTool(ctx, cfg, c)
			fmt.Printf("TOOL: %s -> %d chars\n", status, len(result))
			if c.Name == "web_search" {
				searched = true
			}
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: c.ID, Content: result})
		}
	}
	t.Fatal("never finished")
}
