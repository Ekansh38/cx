package llm

import (
	"context"
	"fmt"
	"strings"

	"cx/config"
)

// Message is a single chat message.
type Message struct {
	Role    string
	Content string
}

// Provider streams responses from an LLM.
type Provider interface {
	// Stream calls onToken for each token and returns the full content when done.
	Stream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error)
}

// ForModel returns the right provider for the given model name using cfg.
func ForModel(model string, cfg *config.Config) (Provider, error) {
	m := strings.ToLower(model)

	switch {
	case strings.Contains(m, "/"):
		// OpenRouter models are always "provider/model" (e.g. anthropic/claude-opus-4)
		if cfg.OpenRouter.APIKey == "" {
			return nil, fmt.Errorf("no OpenRouter API key — set OPENROUTER_API_KEY or [openrouter] api_key in config.toml")
		}
		baseURL := cfg.OpenRouter.BaseURL
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
		return &openAIProvider{apiKey: cfg.OpenRouter.APIKey, baseURL: baseURL}, nil

	case strings.HasPrefix(m, "gemini"):
		if cfg.Gemini.APIKey == "" {
			return nil, fmt.Errorf("no Gemini API key — set GEMINI_API_KEY or [gemini] api_key in config.toml")
		}
		baseURL := cfg.Gemini.BaseURL
		if baseURL == "" {
			baseURL = "https://generativelanguage.googleapis.com/v1beta/openai/"
		}
		return &openAIProvider{apiKey: cfg.Gemini.APIKey, baseURL: baseURL}, nil

	case strings.HasPrefix(m, "gpt") ||
		strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "chatgpt"):
		if cfg.OpenAI.APIKey == "" {
			return nil, fmt.Errorf("no OpenAI API key — set OPENAI_API_KEY or [openai] api_key in config.toml")
		}
		baseURL := cfg.OpenAI.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return &openAIProvider{apiKey: cfg.OpenAI.APIKey, baseURL: baseURL}, nil

	default:
		// Ollama or any OpenAI-compatible local server
		baseURL := cfg.Ollama.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		return &openAIProvider{apiKey: "", baseURL: baseURL}, nil
	}
}
