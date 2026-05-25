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

func (p *openAIProvider) Complete(ctx context.Context, model string, msgs []Message) (string, error) {
	type oMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type reqBody struct {
		Model    string `json:"model"`
		Messages []oMsg `json:"messages"`
	}

	body := reqBody{Model: model}
	for _, m := range msgs {
		body.Messages = append(body.Messages, oMsg{Role: m.Role, Content: m.Content})
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if strings.Contains(p.baseURL, "openrouter.ai") {
		req.Header.Set("HTTP-Referer", "https://github.com/Ekansh38/cx")
		req.Header.Set("X-OpenRouter-Title", "cx")
	}

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
	type oMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type reqBody struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []oMsg `json:"messages"`
	}

	body := reqBody{Model: model, Stream: true}
	body.StreamOptions.IncludeUsage = true
	for _, m := range msgs {
		body.Messages = append(body.Messages, oMsg{Role: m.Role, Content: m.Content})
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	// OpenRouter requires these for some accounts
	if strings.Contains(p.baseURL, "openrouter.ai") {
		req.Header.Set("HTTP-Referer", "https://github.com/Ekansh38/cx")
		req.Header.Set("X-OpenRouter-Title", "cx")
	}

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
