package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"cx/llm"
	"cx/memory"
	"cx/store"
	"cx/tui"
)

const (
	model     = "llama3.2"
	ollamaURL = "http://localhost:11434/v1"
)

var debug = true

var debugLog *os.File

func initDebug() {
	if !debug {
		return
	}
	f, err := os.OpenFile("data/debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("debug log: %v", err)
		return
	}
	debugLog = f
}

func logPrompt(messages []llm.Message) {
	if debugLog == nil {
		return
	}
	b, _ := json.MarshalIndent(messages, "", "  ")
	fmt.Fprintf(debugLog, "\n- %s -\n%s\n", time.Now().Format("15:04:05"), b)
}

func main() {
	initDebug()
	if debugLog != nil {
		defer debugLog.Close()
	}

	s, err := store.New("data/cx.db")
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	provider := llm.NewOpenAI("", ollamaURL)

	conv, err := s.GetConversation(1)
	if err == store.ErrNotFound {
		conv, err = s.CreateConversation(model)
	}
	if err != nil {
		log.Fatal(err)
	}

	t, err := tui.New(fmt.Sprintf("%s  conv:%d", model, conv.ID))
	if err != nil {
		log.Fatal(err)
	}
	defer t.Close()

	history, err := s.GetMessages(conv.ID)
	if err != nil {
		log.Fatal(err)
	}
	var tuiHistory []tui.Message
	for _, m := range history {
		tuiHistory = append(tuiHistory, tui.Message{Role: m.Role, Content: m.Content})
	}
	t.PrintHistory(tuiHistory)

	// build system prompt with memory context
	summaries, err := s.GetSummaries(10)
	if err != nil {
		log.Fatal(err)
	}
	memCtx := memory.BuildContext(summaries)
	systemPrompt := "You are cx, Ekansh's personal terminal assistant. Be concise."
	if memCtx != "" {
		systemPrompt += "\n\n" + memCtx
	}

	var newMessages int
	for {
		input, err := t.ReadLine()
		if err != nil {
			break
		}
		if input == "" {
			continue
		}
		if input == ":q" {
			break
		}

		if _, err := s.AddMessage(conv.ID, "user", input); err != nil {
			log.Fatal(err)
		}
		newMessages++
		t.PrintUserMessage(input)

		stored, err := s.GetMessages(conv.ID)
		if err != nil {
			log.Fatal(err)
		}

		messages := make([]llm.Message, 0, len(stored)+1)
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: systemPrompt,
		})
		for _, m := range stored {
			messages = append(messages, llm.Message{Role: m.Role, Content: m.Content})
		}

		logPrompt(messages)

		t.BeginStream()
		resp, err := provider.Stream(context.Background(), model, messages, t.WriteToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
		t.EndStream()

		if resp.Content != "" {
			if _, err := s.AddMessage(conv.ID, "assistant", resp.Content); err != nil {
				log.Fatal(err)
			}
		}
	}

	// reset terminal before memory work so output is clean
	t.Close()

	if newMessages == 0 {
		return
	}

	msgs, err := s.GetMessages(conv.ID)
	if err != nil || len(msgs) == 0 {
		return
	}

	fmt.Print("saving memory... ")
	summary, err := memory.GenerateSummary(context.Background(), provider, model, msgs)
	if err == nil && summary != "" {
		if err := s.UpdateSummary(conv.ID, summary, len(msgs)); err != nil {
			fmt.Fprintf(os.Stderr, "summary save: %v\n", err)
		}
	}
	if err := memory.UpdateMemoryFile(context.Background(), provider, model, msgs); err != nil {
		fmt.Fprintf(os.Stderr, "memory update: %v\n", err)
	}
	fmt.Println("done.")
}
