package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"cx/config"
)

// ModelInfo describes an available model from a remote provider.
type ModelInfo struct {
	ID   string // e.g. "anthropic/claude-opus-4"
	Name string // human-readable name
}

// FetchOpenRouterModels returns models from the OpenRouter /models endpoint.
func FetchOpenRouterModels(apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// A hung endpoint would otherwise pin the model picker on its spinner forever
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OpenRouter %s: %s", resp.Status, b)
	}

	var result struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, ModelInfo{ID: m.ID, Name: m.Name})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models, nil
}

// Message is a single chat message.
type Message struct {
	Role    string
	Content string
	Images  []string // base64 data URLs (e.g. "data:image/png;base64,...")
}

// Provider streams responses from an LLM.
type Provider interface {
	// Stream calls onToken for each token and returns the full content when done.
	Stream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error)
	// Complete is a non-streaming one-shot call. Used for background tasks (memory, compaction).
	Complete(ctx context.Context, model string, msgs []Message) (string, error)
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
		return &openAIProvider{apiKey: cfg.OpenRouter.APIKey, baseURL: baseURL, maxTokens: cfg.MaxTokens}, nil

	case strings.HasPrefix(m, "gemini"):
		if cfg.Gemini.APIKey == "" {
			return nil, fmt.Errorf("no Gemini API key — set GEMINI_API_KEY or [gemini] api_key in config.toml")
		}
		baseURL := cfg.Gemini.BaseURL
		if baseURL == "" {
			baseURL = "https://generativelanguage.googleapis.com/v1beta/openai/"
		}
		return &openAIProvider{apiKey: cfg.Gemini.APIKey, baseURL: baseURL, maxTokens: cfg.MaxTokens}, nil

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
		return &openAIProvider{apiKey: cfg.OpenAI.APIKey, baseURL: baseURL, maxTokens: cfg.MaxTokens}, nil

	default:
		// Ollama or any OpenAI-compatible local server
		baseURL := cfg.Ollama.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		return &openAIProvider{apiKey: "", baseURL: baseURL, maxTokens: cfg.MaxTokens}, nil
	}
}
