package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

var debug = true // set false to disable prompt logging to data/debug.log

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
	// background save mode: cx --save-memory <convID>
	if len(os.Args) == 3 && os.Args[1] == "--save-memory" {
		convID, err := strconv.ParseInt(os.Args[2], 10, 64)
		if err != nil {
			os.Exit(1)
		}
		s, err := store.New("data/cx.db")
		if err != nil {
			os.Exit(1)
		}
		defer s.Close()
		saveMemory(s, llm.NewOpenAI("", ollamaURL), convID)
		return
	}

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

	convID, err := pickConversation(s, model)
	if err != nil {
		log.Fatal(err)
	}

	for {
		conv, err := s.GetConversation(convID)
		if err != nil {
			log.Fatal(err)
		}

		t, err := tui.New(fmt.Sprintf("%s  #%d  %s", model, conv.ID, conv.Title))
		if err != nil {
			log.Fatal(err)
		}

		history, err := s.GetMessages(conv.ID)
		if err != nil {
			log.Fatal(err)
		}
		var tuiHistory []tui.Message
		for _, m := range history {
			tuiHistory = append(tuiHistory, tui.Message{Role: m.Role, Content: m.Content})
		}
		t.PrintHistory(tuiHistory)

		summaries, err := s.GetSummaries(10)
		if err != nil {
			log.Fatal(err)
		}
		memCtx := memory.BuildContext(summaries)
		systemPrompt := "You are cx. You are talking directly with Ekansh. Be concise and direct. Only reference things you actually know -- never invent context, memories, or events."
		if memCtx != "" {
			systemPrompt += "\n\n" + memCtx
		}

		isFirstExchange := len(history) == 0
		var newMessages int
		nextConvID := int64(-1) // -1 = quit, anything else = switch to that conv

		for {
			input, err := t.ReadLine()
			if err != nil {
				break
			}
			if input == "" {
				continue
			}

			// commands
			if input == ":q" {
				break
			}
			if input == ":new" {
				newConv, err := s.CreateConversation(model)
				if err != nil {
					log.Fatal(err)
				}
				nextConvID = newConv.ID
				break
			}
			if input == ":list" {
				t.Close()
				id, err := pickConversation(s, model)
				if err != nil {
					log.Fatal(err)
				}
				nextConvID = id
				break
			}
			if strings.HasPrefix(input, ":rename ") {
				title := strings.TrimPrefix(input, ":rename ")
				if err := s.UpdateTitle(conv.ID, title); err != nil {
					log.Fatal(err)
				}
				conv.Title = title
				t.SetStatus(fmt.Sprintf("%s  #%d  %s", model, conv.ID, conv.Title))
				continue
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
			messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
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
				// auto-title after first exchange
				if isFirstExchange && conv.Title == "New conversation" {
					isFirstExchange = false
					go autoTitle(s, provider, conv.ID, input)
				}
			}
		}

		t.Close()

		if newMessages > 0 {
			spawnSave(conv.ID)
		}

		if nextConvID == -1 {
			break
		}
		convID = nextConvID
	}
}

// spawnSave launches cx --save-memory <convID> as a detached subprocess.
// The current process exits immediately; the child handles the slow LLM work.
func spawnSave(convID int64) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	devNull, _ := os.Open(os.DevNull)
	cmd := exec.Command(exe, "--save-memory", strconv.FormatInt(convID, 10))
	cmd.Stdin = nil
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Start() // intentionally not waiting
}

func saveMemory(s *store.Store, provider llm.Provider, convID int64) {
	msgs, err := s.GetMessages(convID)
	if err != nil || len(msgs) == 0 {
		return
	}
	fmt.Print("saving memory... ")
	summary, err := memory.GenerateSummary(context.Background(), provider, model, msgs)
	if err == nil && summary != "" {
		if err := s.UpdateSummary(convID, summary, len(msgs)); err != nil {
			fmt.Fprintf(os.Stderr, "summary save: %v\n", err)
		}
	}
	if err := memory.UpdateMemoryFile(context.Background(), provider, model, msgs); err != nil {
		fmt.Fprintf(os.Stderr, "memory update: %v\n", err)
	}
	fmt.Println("done.")
}

func autoTitle(s *store.Store, provider llm.Provider, convID int64, firstMsg string) {
	prompt := fmt.Sprintf("Give a 4-6 word title for a conversation that starts with this message: %q\nReply with only the title, no quotes, no punctuation at the end.", firstMsg)
	resp, err := provider.Stream(context.Background(), model, []llm.Message{
		{Role: "user", Content: prompt},
	}, func(string) {})
	if err != nil {
		return
	}
	title := strings.TrimSpace(resp.Content)
	if title != "" {
		s.UpdateTitle(convID, title)
	}
}
