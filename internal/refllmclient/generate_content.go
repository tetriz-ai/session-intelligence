// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

import (
	"context"
	"encoding/json"
	"fmt"
)

// GenerateContentRequest is the shared generate-content request (SessionContext is used
// only by analyze_prompt).
type GenerateContentRequest struct {
	Content        string         `json:"content"`
	TaskType       string         `json:"task_type"`
	OrgID          string         `json:"org_id"`
	SourceID       string         `json:"source_id"`
	SessionContext map[string]any `json:"session_context,omitempty"`
	// UseSystemPromptCache opts this call into reference-llm's Gemini system-prompt
	// caching. Valid on any task type; engages only when
	// the call routes to Gemini. The field is always sent (no omitempty): reference-llm
	// reads it as a tri-state where an absent value falls back to the per-task default,
	// so an explicit false must reach the wire to disable caching on a task that defaults
	// it on — omitempty would drop that false and silently re-enable caching.
	UseSystemPromptCache bool `json:"use_system_prompt_cache"`
}

// GenerateContentResponse is the common generate-content envelope. Result is the
// task-specific JSON the caller parses; Raw is the verbatim body.
type GenerateContentResponse struct {
	Result   string          `json:"result"`
	TaskType string          `json:"task_type"`
	Model    string          `json:"model"`
	Provider string          `json:"provider"`
	Usage    *TokenUsage     `json:"usage,omitempty"`
	CostUSD  *float64        `json:"cost_usd,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Raw      []byte          `json:"-"`
}

// GenerateContent POSTs the request, returns the decoded envelope, and fires the
// SuccessHook on success.
func (c *Client) GenerateContent(ctx context.Context, req GenerateContentRequest) (*GenerateContentResponse, error) {
	var response GenerateContentResponse
	body, err := c.postJSON(ctx, generateContentAPI, req, &response, DefaultMaxBody)
	if err != nil {
		return nil, fmt.Errorf("generate-content: %w", err)
	}
	response.Raw = body
	if c.hook != nil {
		c.hook.OnGenerateContent(ctx, req, &response)
	}
	return &response, nil
}
