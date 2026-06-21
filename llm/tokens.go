package llm

// EstimateTokens returns an approximate token count for a string.
// Heuristic: ~4 chars per token for English text with GPT-class tokenizers.
func EstimateTokens(s string) int {
	return len(s)/4 + 1
}
