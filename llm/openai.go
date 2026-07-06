package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type openAIProvider struct {
	apiKey    string
	baseURL   string
	maxTokens int
}

// oMsg supports plain text, multimodal content, and tool exchanges.
type oMsg struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content,omitempty"` // string or []contentPart
	ToolCalls  []oToolCall `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
	File     *filePart `json:"file,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type filePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"` // data URL, e.g. application/pdf base64
}

type oToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

func buildTools(tools []Tool) []oTool {
	out := make([]oTool, 0, len(tools))
	for _, t := range tools {
		var ot oTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		out = append(out, ot)
	}
	return out
}

// buildMessages converts llm.Messages to OpenAI API format, using content
// arrays when images/files are present and tool fields for tool exchanges.
func buildMessages(msgs []Message) []oMsg {
	out := make([]oMsg, 0, len(msgs))
	for _, m := range msgs {
		om := oMsg{Role: m.Role, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var otc oToolCall
			otc.ID = tc.ID
			otc.Type = "function"
			otc.Function.Name = tc.Name
			otc.Function.Arguments = tc.Args
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		if len(m.Images) == 0 && len(m.Files) == 0 {
			om.Content = m.Content
			out = append(out, om)
			continue
		}
		parts := []contentPart{{Type: "text", Text: m.Content}}
		for _, img := range m.Images {
			parts = append(parts, contentPart{
				Type:     "image_url",
				ImageURL: &imageURL{URL: img},
			})
		}
		for i, f := range m.Files {
			parts = append(parts, contentPart{
				Type: "file",
				File: &filePart{Filename: fmt.Sprintf("document-%d.pdf", i+1), FileData: f},
			})
		}
		om.Content = parts
		out = append(out, om)
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
	content, _, err := p.StreamTools(ctx, model, msgs, nil, onToken)
	return content, err
}

func (p *openAIProvider) StreamTools(ctx context.Context, model string, msgs []Message, tools []Tool, onToken func(string)) (string, []ToolCall, error) {
	type reqBody struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		MaxTokens     int    `json:"max_tokens,omitempty"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []oMsg  `json:"messages"`
		Tools    []oTool `json:"tools,omitempty"`
	}

	maxTok := p.maxTokens
	if maxTok <= 0 {
		maxTok = 16384
	}
	body := reqBody{Model: model, Stream: true, Messages: buildMessages(msgs), MaxTokens: maxTok}
	if len(tools) > 0 {
		body.Tools = buildTools(tools)
	}
	body.StreamOptions.IncludeUsage = true

	buf, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", nil, err
	}
	p.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("API %s: %s", resp.Status, b)
	}

	var full strings.Builder
	// tool-call deltas arrive fragmented and index-keyed; accumulate them
	calls := map[int]*ToolCall{}
	scanner := bufio.NewScanner(resp.Body)
	// SSE lines can far exceed the 64KB default (large final chunks, long
	// error payloads); ErrTooLong would kill the stream mid-response.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
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
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			d := chunk.Choices[0].Delta
			if d.Content != "" {
				onToken(d.Content)
				full.WriteString(d.Content)
			}
			for _, tc := range d.ToolCalls {
				c := calls[tc.Index]
				if c == nil {
					c = &ToolCall{}
					calls[tc.Index] = c
				}
				if tc.ID != "" {
					c.ID = tc.ID
				}
				if tc.Function.Name != "" {
					c.Name = tc.Function.Name
				}
				c.Args += tc.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return full.String(), nil, err
	}

	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]ToolCall, 0, len(calls))
	for _, i := range idxs {
		if calls[i].Name != "" {
			out = append(out, *calls[i])
		}
	}
	return full.String(), out, nil
}
