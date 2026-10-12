// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logredact"
	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
)

// The public seams the single-session front door (and the analyze package) consume WITHOUT importing
// anything under internal/. They are public mirrors of the internal reference-llm client surface: the
// R/E/D orchestration and response parsing stay bundled in LLMClient, so a caller supplies only a
// content generator (or its own) rather than reimplementing the parsers.

// ContentGenerator is the public content-generation seam: one GenerateContent call carrying the
// task type, the assembled content, and the token usage. It is the public version of the internal
// client's generate-content surface. A real caller gets one from NewReferenceLLMContentGenerator; a
// fake drives the unit tests. NewContentGeneratorProducer wraps it into the bundled LLMProducer that
// owns the R/E/D orchestration and response parsing.
type ContentGenerator interface {
	GenerateContent(ctx context.Context, req GenerateContentRequest) (*GenerateContentResponse, error)
}

// GenerateContentRequest is the public generate-content request (a mirror of the internal shape).
type GenerateContentRequest struct {
	Content              string
	TaskType             string
	OrgID                string
	SourceID             string
	SessionContext       map[string]any
	UseSystemPromptCache bool
}

// GenerateContentResponse is the public generate-content envelope. Result is the task-specific JSON
// the bundled parser decodes.
type GenerateContentResponse struct {
	Result   string
	TaskType string
	Model    string
	Provider string
	Usage    *TokenUsage
}

// TokenUsage is the public token accounting one inference reported.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	CacheReadTokens  int
}

// NewContentGeneratorProducer builds the bundled LLMProducer (R/E/D orchestration + response
// parsing) over a public ContentGenerator, so a caller never reimplements the parsers.
func NewContentGeneratorProducer(generator ContentGenerator) *LLMClient {
	return NewLLMClient(contentGeneratorAdapter{generator: generator})
}

// NewReferenceLLMContentGenerator returns a ContentGenerator backed by a reference-llm service at
// baseURL (e.g. "http://localhost:8000"). The reference-llm service runs the real Gemini call.
func NewReferenceLLMContentGenerator(baseURL string) ContentGenerator {
	return referenceLLMContentGenerator{client: refllmclient.New(baseURL)}
}

// contentGeneratorAdapter adapts a public ContentGenerator to the internal client surface LLMClient
// consumes, so LLMClient keeps driving the R/E/D orchestration and parsing unchanged.
type contentGeneratorAdapter struct{ generator ContentGenerator }

func (a contentGeneratorAdapter) GenerateContent(ctx context.Context, req refllmclient.GenerateContentRequest) (*refllmclient.GenerateContentResponse, error) {
	resp, err := a.generator.GenerateContent(ctx, toPublicRequest(req))
	if err != nil {
		return nil, fmt.Errorf("content generator (task %s): %w", req.TaskType, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("content generator (task %s): nil response", req.TaskType)
	}
	return toInternalResponse(resp), nil
}

// referenceLLMContentGenerator wraps the internal client as a public ContentGenerator.
type referenceLLMContentGenerator struct{ client *refllmclient.Client }

func (w referenceLLMContentGenerator) GenerateContent(ctx context.Context, req GenerateContentRequest) (*GenerateContentResponse, error) {
	resp, err := w.client.GenerateContent(ctx, toInternalRequest(req))
	if err != nil {
		return nil, fmt.Errorf("reference-llm generate-content (task %s): %w", req.TaskType, err)
	}
	return toPublicResponse(resp), nil
}

// The request/response/usage converters between the public seam and the internal client surface, so
// the mirror field-copies live once.

func toPublicRequest(req refllmclient.GenerateContentRequest) GenerateContentRequest {
	return GenerateContentRequest{
		Content:              req.Content,
		TaskType:             req.TaskType,
		OrgID:                req.OrgID,
		SourceID:             req.SourceID,
		SessionContext:       req.SessionContext,
		UseSystemPromptCache: req.UseSystemPromptCache,
	}
}

func toInternalRequest(req GenerateContentRequest) refllmclient.GenerateContentRequest {
	return refllmclient.GenerateContentRequest{
		Content:              req.Content,
		TaskType:             req.TaskType,
		OrgID:                req.OrgID,
		SourceID:             req.SourceID,
		SessionContext:       req.SessionContext,
		UseSystemPromptCache: req.UseSystemPromptCache,
	}
}

func toInternalResponse(resp *GenerateContentResponse) *refllmclient.GenerateContentResponse {
	if resp == nil {
		return nil
	}
	return &refllmclient.GenerateContentResponse{
		Result:   resp.Result,
		TaskType: resp.TaskType,
		Model:    resp.Model,
		Provider: resp.Provider,
		Usage:    toInternalUsage(resp.Usage),
	}
}

func toPublicResponse(resp *refllmclient.GenerateContentResponse) *GenerateContentResponse {
	if resp == nil {
		return nil
	}
	return &GenerateContentResponse{
		Result:   resp.Result,
		TaskType: resp.TaskType,
		Model:    resp.Model,
		Provider: resp.Provider,
		Usage:    toPublicUsage(resp.Usage),
	}
}

func toInternalUsage(usage *TokenUsage) *refllmclient.TokenUsage {
	if usage == nil {
		return nil
	}
	return &refllmclient.TokenUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CacheReadTokens:  usage.CacheReadTokens,
	}
}

func toPublicUsage(usage *refllmclient.TokenUsage) *TokenUsage {
	if usage == nil {
		return nil
	}
	return &TokenUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CacheReadTokens:  usage.CacheReadTokens,
	}
}

// RedactSecret masks well-known secret shapes (tokens, emails, home-dir paths) in a free-form string
// — the same defensive backstop the security reconcile applies to a flag's excerpt. Exposed so the
// public analyze package can redact per-turn finding notes without importing an internal package.
func RedactSecret(note string) string {
	return logredact.Redact(note)
}

// RoundTripPair is the public round-trip detector input (a mirror of the internal pair), so a
// caller's RoundTripDetector never imports anything under internal/. P is the earlier human prompt,
// Q the later one, AgentResponse the assistant turns between them, and Prior the human turn before P.
type RoundTripPair struct {
	PromptIndex   int32
	P             string
	AgentResponse string
	Q             string
	Prior         string
}

// RoundTripDetector is the public, internal-free round-trip detection seam the single-session path
// accepts. It scores the spine's guard-valid pairs and returns the top-N above-threshold candidates.
// A caller can plug its own, or get the reference-llm-backed one from NewReferenceLLMRoundTripDetector.
type RoundTripDetector interface {
	Detect(ctx context.Context, orgID, sessionID string, pairs []RoundTripPair, topN, ceiling int) (*DetectorResult, error)
}

// NewReferenceLLMRoundTripDetector returns a RoundTripDetector backed by a reference-llm service at
// baseURL. The shipped classifier is an uncalibrated placeholder and /detect-round-trips returns 503
// unless the detector bundle is baked and armed.
func NewReferenceLLMRoundTripDetector(baseURL string) RoundTripDetector {
	return referenceLLMRoundTripDetector{inner: NewPairDetectorClient(refllmclient.New(baseURL))}
}

// referenceLLMRoundTripDetector adapts the internal PairDetectorClient to the public seam.
type referenceLLMRoundTripDetector struct{ inner *PairDetectorClient }

func (d referenceLLMRoundTripDetector) Detect(ctx context.Context, orgID, sessionID string, pairs []RoundTripPair, topN, ceiling int) (*DetectorResult, error) {
	internalPairs := make([]refllmclient.RoundTripPair, len(pairs))
	for i, pair := range pairs {
		internalPairs[i] = refllmclient.RoundTripPair{
			PromptIndex:   pair.PromptIndex,
			P:             pair.P,
			AgentResponse: pair.AgentResponse,
			Q:             pair.Q,
			Prior:         pair.Prior,
		}
	}
	result, err := d.inner.Detect(ctx, orgID, sessionID, internalPairs, topN, ceiling)
	if err != nil {
		return nil, fmt.Errorf("reference-llm detect round trips (session %s): %w", sessionID, err)
	}
	return result, nil
}

// ParseConversation parses a session transcript (one JSON object in the Conversation protojson shape)
// into a Conversation, keeping the parse-boundary model backfill. It is the public entry the internal
// parser is reached through, so a caller never imports the internal package.
func ParseConversation(content []byte) (*sessionintelligencepb.Conversation, error) {
	conv, err := sessionintelligence.ParseConversation(content)
	if err != nil {
		return nil, fmt.Errorf("parse conversation: %w", err)
	}
	return conv, nil
}
