package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const anthropicEndpoint = "https://api.anthropic.com/v1/messages"

type Anthropic struct {
	apiKey string
}

func NewAnthropic(apiKey string) *Anthropic {
	return &Anthropic{apiKey: apiKey}
}

func (a *Anthropic) Stream(ctx context.Context, model string, messages []Message, onToken func(string)) (Response, error) {
	body, err := buildAnthropicBody(model, messages)
	if err != nil {
		return Response{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicEndpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return Response{}, fmt.Errorf("anthropic %s: %s", resp.Status, b)
	}

	var result Response
	var sb strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			result.InputTokens = ev.Message.Usage.InputTokens
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				onToken(ev.Delta.Text)
				sb.WriteString(ev.Delta.Text)
			}
		case "message_delta":
			result.OutputTokens = ev.Usage.OutputTokens
		case "message_stop":
			result.Content = sb.String()
			return result, scanner.Err()
		}
	}

	// fallback if message_stop was never received
	result.Content = sb.String()
	return result, scanner.Err()
}

// anthropicEvent handles all SSE event shapes from the Anthropic API.
// Fields not present in a given event type remain zero-valued and are ignored.
type anthropicEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicReq struct {
	Model     string   `json:"model"`
	MaxTokens int      `json:"max_tokens"`
	Stream    bool     `json:"stream"`
	System    string   `json:"system,omitempty"`
	Messages  []aMsg   `json:"messages"`
}

type aMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func buildAnthropicBody(model string, messages []Message) ([]byte, error) {
	req := anthropicReq{
		Model:     model,
		MaxTokens: 8096,
		Stream:    true,
	}
	for _, m := range messages {
		if m.Role == "system" {
			req.System = m.Content
			continue
		}
		req.Messages = append(req.Messages, aMsg{Role: m.Role, Content: m.Content})
	}
	return json.Marshal(req)
}
