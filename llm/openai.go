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

type OpenAI struct {
	apiKey  string
	baseURL string
}

// NewOpenAI works for both OpenAI and Ollama.
//
// OpenAI: NewOpenAI(apiKey, "https://api.openai.com/v1")
// Ollama: NewOpenAI("",     "http://localhost:11434/v1")
func NewOpenAI(apiKey, baseURL string) *OpenAI {
	return &OpenAI{apiKey: apiKey, baseURL: baseURL}
}

func (o *OpenAI) Stream(ctx context.Context, model string, messages []Message, onToken func(string)) (Response, error) {
	body, err := buildOpenAIBody(model, messages)
	if err != nil {
		return Response{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("content-type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("authorization", "Bearer "+o.apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return Response{}, fmt.Errorf("openai %s: %s", resp.Status, b)
	}

	var result Response
	var sb strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk openaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			if t := chunk.Choices[0].Delta.Content; t != "" {
				onToken(t)
				sb.WriteString(t)
			}
		}
		// usage arrives in the final chunk before [DONE] when stream_options.include_usage is set.
		// Ollama may not send it -- chunk.Usage stays nil and token counts remain 0.
		if chunk.Usage != nil {
			result.InputTokens = chunk.Usage.PromptTokens
			result.OutputTokens = chunk.Usage.CompletionTokens
		}
	}

	result.Content = sb.String()
	return result, scanner.Err()
}

type openaiReq struct {
	Model         string      `json:"model"`
	Stream        bool        `json:"stream"`
	StreamOptions streamOpts  `json:"stream_options"`
	Messages      []oMsg      `json:"messages"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func buildOpenAIBody(model string, messages []Message) ([]byte, error) {
	req := openaiReq{
		Model:         model,
		Stream:        true,
		StreamOptions: streamOpts{IncludeUsage: true},
	}
	for _, m := range messages {
		req.Messages = append(req.Messages, oMsg{Role: m.Role, Content: m.Content})
	}
	return json.Marshal(req)
}
