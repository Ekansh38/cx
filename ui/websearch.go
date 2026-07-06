package ui

// Web tools for the agent loop: the model decides when to search or fetch,
// crafts its own queries, and can run as many rounds as it needs (capped).
// Search execution reuses the OpenRouter key via a cheap grounded model
// (:online); page fetching goes through the r.jina.ai reader (no key).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"cx/config"
	"cx/llm"
)

const maxToolRounds = 6

func webTools() []llm.Tool {
	return []llm.Tool{
		{
			Name:        "discard_pending_edits",
			Description: "Discard EVERY edit currently under review (in the user's editor) and any queued behind it. Use when the user redirects the conversation instead of reviewing — 'let's discuss this instead', 'wait, before you change anything', a question about the doc, or any message that isn't approval — so your obsolete proposals don't clutter their editor. After discarding, respond in prose only; do NOT propose new edits in the same response.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "apply_all_pending_edits",
			Description: "Apply every edit currently under review at once. Use only when the user has clearly approved everything ('apply all', 'looks good, do it', 'yes to all').",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "web_search",
			Description: "Search the live web. Use for anything current: news, prices, rankings, recent releases, or facts you are not certain about. Craft a specific search query; do not pass the user's message verbatim. Returns findings with source URLs.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "A focused search query, e.g. 'best specialty coffee shops Singapore 2026'",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "fetch_url",
			Description: "Fetch a web page and return its readable content as markdown. Use to read a specific page in depth, e.g. one surfaced by web_search or linked by the user.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The full http(s) URL to fetch",
					},
				},
				"required": []string{"url"},
			},
		},
	}
}

// execWebTool runs one tool call, returning a short status line (shown in the
// transcript) and the result text handed back to the model.
func execWebTool(ctx context.Context, cfg *config.Config, call llm.ToolCall) (status, result string) {
	switch call.Name {
	case "discard_pending_edits":
		RequestReviewDiscard()
		return "discarding pending edits", "OK, discarded every pending edit. The user's editor is clean."

	case "apply_all_pending_edits":
		applyAllExternalReview()
		return "applying all pending edits", "OK, applied every pending edit."

	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil || args.Query == "" {
			return "searching the web", "error: invalid web_search arguments"
		}
		status = fmt.Sprintf("searching the web: %q", args.Query)
		return status, runGroundedSearch(ctx, cfg, args.Query)

	case "fetch_url":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil || args.URL == "" {
			return "fetching a page", "error: invalid fetch_url arguments"
		}
		status = "reading " + args.URL
		return status, fetchReadable(ctx, args.URL)

	default:
		return "unknown tool " + call.Name, "error: unknown tool " + call.Name
	}
}

// runGroundedSearch executes a search by asking a cheap grounded model
// (OpenRouter :online) to research the query and report findings.
func runGroundedSearch(ctx context.Context, cfg *config.Config, query string) string {
	model := cfg.MemoryModel
	if model == "" {
		model = "google/gemini-2.5-flash"
	}
	prov, err := llm.ForModel(model, cfg)
	if err != nil {
		return "error: search unavailable: " + err.Error()
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(
		"Search the web for: %q\n\nToday's date: %s. Report the most relevant, current findings as concise bullet points. Include a source URL for every claim. If results look off-topic, say so instead of inventing.",
		query, time.Now().Format("2006-01-02"),
	)
	out, err := prov.Complete(cctx, model+":online", []llm.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return "error: search failed: " + err.Error()
	}
	return out
}

// fetchReadable pulls a page as markdown via the r.jina.ai reader.
func fetchReadable(ctx context.Context, url string) string {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "https://r.jina.ai/"+url, nil)
	if err != nil {
		return "error: " + err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "error: fetch failed: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 80*1024))
	if err != nil {
		return "error: read failed: " + err.Error()
	}
	text := string(body)
	if len(text) > 20000 {
		text = text[:20000] + "\n\n[truncated]"
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("error: HTTP %d fetching %s: %s", resp.StatusCode, url, text)
	}
	return text
}
