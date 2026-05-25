package llm

// EstimateTokens returns an approximate token count for a string.
// Heuristic: ~4 chars per token for English text with GPT-class tokenizers.
func EstimateTokens(s string) int {
	return len(s)/4 + 1
}

// EstimateMessagesTokens sums token estimates for a slice of Messages.
func EstimateMessagesTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m.Content) + 4 // +4 for role/formatting overhead
	}
	return total
}
