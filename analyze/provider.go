// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"

// Provider is the content-generation seam AnalyzeSession runs the read/evidence/discard passes
// through: one GenerateContent call per inference, carrying the task type, the assembled content, and
// the token usage. The bundled client keeps the R/E/D orchestration AND the response parsing, so you
// supply only a content generator — point it at a reference-llm service with NewReferenceLLMProvider, or
// implement Provider yourself (e.g. a fake for tests, or your own LLM transport).
type Provider = sessionanalysis.ContentGenerator

// GenerateContentRequest is the request one Provider.GenerateContent call receives.
type GenerateContentRequest = sessionanalysis.GenerateContentRequest

// GenerateContentResponse is what a Provider.GenerateContent call returns. Result is the
// task-specific JSON the bundled parser decodes.
type GenerateContentResponse = sessionanalysis.GenerateContentResponse

// TokenUsage is the token accounting one inference reported.
type TokenUsage = sessionanalysis.TokenUsage

// NewReferenceLLMProvider returns a Provider backed by a reference-llm service at baseURL
// (e.g. "http://localhost:8000"), which performs the real Gemini call. Bring your own key by booting
// reference-llm with LLM__GEMINI_API_KEY set; this process never reads the key.
func NewReferenceLLMProvider(baseURL string) Provider {
	return sessionanalysis.NewReferenceLLMContentGenerator(baseURL)
}
