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

type openAIProvider struct {
	apiKey  string
	baseURL string
}

// oMsg supports both plain text and multimodal content.
type oMsg struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []contentPart
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// buildMessages converts llm.Messages to OpenAI API format,
// using content arrays when images are present.
func buildMessages(msgs []Message) []oMsg {
	out := make([]oMsg, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Images) == 0 {
			out = append(out, oMsg{Role: m.Role, Content: m.Content})
			continue
		}
		// Multimodal: text + images
		parts := []contentPart{{Type: "text", Text: m.Content}}
		for _, img := range m.Images {
			parts = append(parts, contentPart{
				Type:     "image_url",
				ImageURL: &imageURL{URL: img},
			})
		}
		out = append(out, oMsg{Role: m.Role, Content: parts})
	}
	return out
}

func (p *openAIProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if strings.Contains(p.baseURL, "openrouter.ai") {
		req.Header.Set("HTTP-Referer", "https://github.com/Ekansh38/cx")
		req.Header.Set("X-OpenRouter-Title", "cx")
	}
}

func (p *openAIProvider) Complete(ctx context.Context, model string, msgs []Message) (string, error) {
	type reqBody struct {
		Model    string `json:"model"`
		Messages []oMsg `json:"messages"`
	}

	body := reqBody{Model: model, Messages: buildMessages(msgs)}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	p.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API %s: %s", resp.Status, b)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return result.Choices[0].Message.Content, nil
}

func (p *openAIProvider) Stream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error) {
	type reqBody struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		MaxTokens     int    `json:"max_tokens,omitempty"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []oMsg `json:"messages"`
	}

	body := reqBody{Model: model, Stream: true, Messages: buildMessages(msgs), MaxTokens: 16384}
	body.StreamOptions.IncludeUsage = true

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	p.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API %s: %s", resp.Status, b)
	}

	var full strings.Builder
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

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			if t := chunk.Choices[0].Delta.Content; t != "" {
				onToken(t)
				full.WriteString(t)
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return full.String(), err
	}
	return full.String(), nil
}
