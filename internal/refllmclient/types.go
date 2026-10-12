// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

// TokenUsage is reference-llm's token breakdown on LLM responses. The reasoning field
// is tagged thoughts_tokens (NOT thinking_tokens) — a wrong tag silently decodes it
// as 0.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ThinkingTokens   int `json:"thoughts_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CacheReadTokens is the Gemini system-prompt-cache hit count reference-llm reports
	// when a call engages the cache. Zero when the call
	// did not opt into caching or missed.
	CacheReadTokens int `json:"cache_read_tokens"`
}
