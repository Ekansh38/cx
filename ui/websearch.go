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
			Description: "Discard edits that are STILL PENDING under review (not yet applied). Use only when the user explicitly cancels — 'never mind', 'cancel those', 'let's not change anything'. Do NOT call this when edits have already been applied (you'll see 'applied N/N edits' in the transcript) — those are already in the file and need undo_last_review, not discard. Do NOT call this when the user wants more or different edits; just propose new blocks instead.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "undo_last_review",
			Description: "Revert ALL edits that were applied during the PREVIOUS review — i.e. edits that are already IN the file (you'll see 'applied N/N edits' in the transcript). Use when the user asks to undo, revert, or take back changes after a review has finished: 'undo that', 'revert those changes', 'i didn't get to review it', 'go back'. This is the correct tool when edits were already applied; discard_pending_edits cannot reach already-applied edits.",
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
			Name:        "apply_pending_edit",
			Description: "Apply ONE specific pending edit by its 1-based number (the same number shown in the neovim review footer 'cx X/N'). Use when the user says 'yes to edit 3', 'accept that one', 'apply the Backlog change', etc. — you decide which index they mean based on context.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index": map[string]any{
						"type":        "integer",
						"description": "1-based edit number",
					},
				},
				"required": []string{"index"},
			},
		},
		{
			Name:        "reject_pending_edit",
			Description: "Reject ONE specific pending edit by its 1-based number, with an optional reason. If a reason is given, cx sends it back to you so you can propose a revised edit in the same response. Use for 'no not that one', 'skip edit 4', 'reject the OSTEP change because ...'.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index":  map[string]any{"type": "integer", "description": "1-based edit number"},
					"reason": map[string]any{"type": "string", "description": "why (optional)"},
				},
				"required": []string{"index"},
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
	case "undo_last_review":
		RequestReviewUndo()
		return "reverting applied edits", "OK. The applied edits will be reverted when this response finishes streaming — no further action needed from the user. Respond with a brief confirmation that the revert happened."

	case "discard_pending_edits":
		RequestReviewDiscard()
		return "discarding pending edits", "OK, discarded every pending edit. The user's editor is clean."

	case "apply_all_pending_edits":
		applyAllExternalReview()
		return "applying all pending edits", "OK, applied every pending edit."

	case "apply_pending_edit":
		var args struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil || args.Index < 1 {
			return "applying edit", "error: apply_pending_edit needs a positive 1-based index"
		}
		if !applyPendingEditByIndex(args.Index) {
			return fmt.Sprintf("applying edit %d", args.Index), fmt.Sprintf("could not reach neovim to apply edit %d", args.Index)
		}
		return fmt.Sprintf("applying edit %d", args.Index), fmt.Sprintf("OK, applied edit %d.", args.Index)

	case "reject_pending_edit":
		var args struct {
			Index  int    `json:"index"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil || args.Index < 1 {
			return "rejecting edit", "error: reject_pending_edit needs a positive 1-based index"
		}
		if !rejectPendingEditByIndex(args.Index, args.Reason) {
			return fmt.Sprintf("rejecting edit %d", args.Index), fmt.Sprintf("could not reach neovim to reject edit %d", args.Index)
		}
		note := fmt.Sprintf("OK, rejected edit %d", args.Index)
		if args.Reason != "" {
			note += ": " + args.Reason
		}
		return fmt.Sprintf("rejecting edit %d", args.Index), note + "."

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
	// 30s per search. 60s was too generous — a stalled OpenRouter :online
	// call would freeze the whole conversation while the user stared at a
	// motionless "searching the web:" line. Fail fast, let the agent loop
	// keep going with the searches that did return.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
