package memory

import (
	"context"
	"fmt"
	"os"
	"strings"

	"cx/llm"
	"cx/store"
)

const MemoryPath = "data/memory.md"

// BuildContext assembles the system prompt memory section from memory.md and recent summaries.
func BuildContext(summaries []*store.Conversation) string {
	var sb strings.Builder

	if data, err := os.ReadFile(MemoryPath); err == nil && len(data) > 0 {
		sb.WriteString("## About the user\n")
		sb.Write(data)
		sb.WriteString("\n\n")
	}

	if len(summaries) > 0 {
		sb.WriteString("## Recent conversations\n")
		for _, c := range summaries {
			sb.WriteString(fmt.Sprintf("- (conv %d, %d messages): %s\n", c.ID, c.MessageCount, c.Summary))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// GenerateSummary asks the model to summarize the conversation. Depth scales with message count.
func GenerateSummary(ctx context.Context, provider llm.Provider, model string, messages []*store.Message) (string, error) {
	n := len(messages)
	if n == 0 {
		return "", nil
	}

	var depth string
	switch {
	case n < 5:
		depth = "1 sentence"
	case n < 15:
		depth = "2-3 sentences"
	case n < 30:
		depth = "a short paragraph (4-6 sentences)"
	default:
		depth = "a detailed paragraph (8-10 sentences)"
	}

	var conv strings.Builder
	for _, m := range messages {
		conv.WriteString(fmt.Sprintf("%s: %s\n\n", m.Role, m.Content))
	}

	prompt := fmt.Sprintf(
		"Summarize this conversation in %s. Focus on topics discussed, decisions made, and anything that reveals the person's thinking or preferences. Be specific, not vague.\n\n%s",
		depth, conv.String(),
	)

	resp, err := provider.Stream(ctx, model, []llm.Message{
		{Role: "user", Content: prompt},
	}, func(string) {})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// UpdateMemoryFile rewrites data/memory.md with updated insights from the conversation.
// Only runs when the conversation is substantial (> 8 messages).
func UpdateMemoryFile(ctx context.Context, provider llm.Provider, model string, messages []*store.Message) error {
	if len(messages) <= 8 {
		return nil
	}

	existing, _ := os.ReadFile(MemoryPath)

	var conv strings.Builder
	for _, m := range messages {
		conv.WriteString(fmt.Sprintf("%s: %s\n\n", m.Role, m.Content))
	}

	var system string
	if len(existing) > 0 {
		system = fmt.Sprintf("You maintain a psychological and professional profile of a user based on their conversations. Current profile:\n\n%s", existing)
	} else {
		system = "You maintain a psychological and professional profile of a user based on their conversations. No profile exists yet."
	}

	prompt := fmt.Sprintf(
		"Based on this conversation, update the user profile. Add new insights, refine existing ones. Output the complete updated profile as markdown. Be specific: record actual preferences, projects, patterns, and traits you observe.\n\nConversation:\n%s",
		conv.String(),
	)

	resp, err := provider.Stream(ctx, model, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}, func(string) {})
	if err != nil {
		return err
	}

	return os.WriteFile(MemoryPath, []byte(resp.Content), 0644)
}
