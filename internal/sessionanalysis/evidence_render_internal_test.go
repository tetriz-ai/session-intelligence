// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestShortIntent pins how E renders a resolved intent: a real class renders its short name, while
// UNSPECIFIED (never classified), the classifier's AMBIGUOUS abstain, and an unresolved slug all
// render nothing (no intent= token) — the intent-classify sweep's AMBIGUOUS is terminal but hidden.
func TestShortIntent(t *testing.T) {
	resolved := map[string]coreaidlcv1.ArtifactIntentClass{
		"reviewer":   coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_REVIEW,
		"unsure":     coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_AMBIGUOUS,
		"unwritten":  coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_UNSPECIFIED,
		"mechanical": coreaidlcv1.ArtifactIntentClass_ARTIFACT_INTENT_CLASS_MECHANICAL,
	}
	assert.Equal(t, "review", shortIntent(resolved, "reviewer"))
	assert.Equal(t, "mechanical", shortIntent(resolved, "mechanical"))
	assert.Empty(t, shortIntent(resolved, "unsure"), "AMBIGUOUS is a terminal abstain — not surfaced")
	assert.Empty(t, shortIntent(resolved, "unwritten"), "UNSPECIFIED renders nothing")
	assert.Empty(t, shortIntent(resolved, "missing"), "an unresolved slug renders nothing")
}

// evidenceFixture builds a small session with one "deploy" SKILL invocation (carrying an injected
// body and a file path, so the send-transform can be checked) and one detected round-trip pair.
func evidenceFixture() *spineResult {
	return &spineResult{
		prompts: []*sessionintelligencepb.Prompt{
			{Role: sessionintelligencepb.Role_ROLE_USER, Content: "run the deploy skill on src/auth/login.go", Metadata: &sessionintelligencepb.PromptMetadata{}},
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "deployed", Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 10, OutputTokens: 5,
				TokenFidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED,
			}},
			{Role: sessionintelligencepb.Role_ROLE_USER, Content: "no, redo it", Metadata: &sessionintelligencepb.PromptMetadata{}},
		},
		usage:               &sessionintelligencepb.UsageMetadata{},
		ideType:             sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		roundTripCandidates: []DetectedPair{{PromptIndex: 0, Score: 0.9}},
		ledgers: spineLedgers{
			artifactInvocations: 1,
			invocations: []invocationDetail{
				{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, slug: "deploy", promptIndex: 2, injectedBody: "the deploy skill body"},
			},
		},
	}
}

// TestBuildEvidenceContent_AssemblesFenceBlocks pins that E receives a compact, E-shaped request
// (the ANALYZE_EVIDENCE_FENCE vocabulary) rather than R's raw transcript: the LEDGER with artifact
// identity + counts, the per-invocation INVOCATIONS block with its injected body, the detected
// PAIRS, and CAPABILITIES — and that it is NOT the raw sp.llmContent and is anonymized the same way.
func TestBuildEvidenceContent_AssemblesFenceBlocks(t *testing.T) {
	sp := evidenceFixture()
	// The spine renders sp.llmContent once before any producer; the E request must not be that.
	rawContent, _ := buildSpineContent("org-1", "rr-1", sp)
	sp.llmContent = anonymizePaths(rawContent)

	content := buildEvidenceContent("org-1", "rr-1", sp, false, nil)

	tests := []struct {
		name string
		want string
	}{
		{name: "ledger marker", want: "=== LEDGER ==="},
		{name: "ledger identity + count", want: "artifact: deploy kind=skill invocations=1"},
		{name: "invocations marker", want: "=== INVOCATIONS ==="},
		{name: "invocation with prompt_index", want: "invocation: deploy kind=skill prompt_index=2"},
		{name: "injected body", want: "body: the deploy skill body"},
		{name: "pairs marker", want: "=== PAIRS ==="},
		{name: "the detected pair renders", want: "pair prompt_index=0"},
		{name: "pair Q renders", want: "Q: no, redo it"},
		{name: "capabilities marker", want: "=== CAPABILITIES ==="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, content, tt.want)
		})
	}

	// The regression guard against the raw-passthrough gap: E must NOT be handed R's transcript.
	assert.NotEqual(t, sp.llmContent, content, "E content must differ from R's raw transcript (sp.llmContent)")

	// The same send-transform sp.llmContent gets: a raw file path never reaches the model.
	assert.NotContains(t, content, "login.go", "file paths must be anonymized before reaching the model")
	assert.NotContains(t, content, "src/auth", "file paths must be anonymized before reaching the model")
}

// TestBuildEvidenceContent_EmptyLedgerAndPairs renders every block with explicit no-signal lines
// rather than a bare header, so a round-trip-only or an empty E request is never ambiguous.
func TestBuildEvidenceContent_EmptyLedgerAndPairs(t *testing.T) {
	sp := &spineResult{
		prompts: []*sessionintelligencepb.Prompt{{Role: sessionintelligencepb.Role_ROLE_USER, Content: "hi", Metadata: &sessionintelligencepb.PromptMetadata{}}},
		usage:   &sessionintelligencepb.UsageMetadata{},
		ideType: sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
	}
	content := buildEvidenceContent("org-1", "rr-1", sp, false, nil)
	require.Contains(t, content, "=== LEDGER ===")
	require.Contains(t, content, "no artifact invocations")
	require.Contains(t, content, "=== INVOCATIONS ===")
	require.Contains(t, content, "no invocations")
	require.Contains(t, content, "=== PAIRS ===")
	require.Contains(t, content, "no round-trip pairs")
	require.Contains(t, content, "=== CAPABILITIES ===")
}

// TestBuildInvocationDetails maps session-event kinds and sub-agent runs to resolvable invocations
// and excludes non-artifact kinds — the detail behind the E fire tally, narrower on purpose.
func TestBuildInvocationDetails(t *testing.T) {
	conv := &sessionintelligencepb.Conversation{
		SessionEvents: []*sessionintelligencepb.SessionEvent{
			{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL, OperationName: "Deploy", RedactedSummary: "body"},
			{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMMAND, OperationName: "review"},
			{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_HOOK_CALL, OperationName: "pre-commit"},
			{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL, OperationName: "Bash"}, // not an artifact
			{Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL, OperationName: "   "}, // empty slug dropped
		},
		SessionMeta: &sessionintelligencepb.SessionMeta{SubAgentRuns: []*sessionintelligencepb.SubAgentRun{{Name: "Context-Gatherer"}}},
	}
	got := buildInvocationDetails(conv)
	require.Len(t, got, 4)
	assert.Equal(t, invocationDetail{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, slug: "deploy", injectedBody: "body"}, got[0])
	assert.Equal(t, invocationDetail{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND, slug: "review"}, got[1])
	assert.Equal(t, invocationDetail{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK, slug: "pre-commit"}, got[2])
	assert.Equal(t, invocationDetail{artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT, slug: "context-gatherer"}, got[3])
}
