package llm

import "context"

type Message struct {
	Role    string // "user" or "assistant" or "system"
	Content string
}

type Response struct {
	Content      string
	InputTokens  int
	OutputTokens int
}

type Provider interface {
	Stream(ctx context.Context, model string, messages []Message, onToken func(string)) (Response, error)
}
