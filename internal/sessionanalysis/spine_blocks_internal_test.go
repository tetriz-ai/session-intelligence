// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestBuildSpineContent_TurnGroupedBodyWellFormed is the request-snapshot for the shared
// turn-grouped body: one rendered block per human turn — machine-turn filtered — each
// carrying its index/sequence, author, and measured telemetry tagged MEASURED —
// authoritative, followed by the session's capabilities block.
func TestBuildSpineContent_TurnGroupedBodyWellFormed(t *testing.T) {
	sp := &spineResult{
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "on it", Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 100, OutputTokens: 20,
				TokenFidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED,
				Model:         "claude-sonnet-4-6",
				FilesEdited:   []string{"/f.go"}, RelevantFiles: []string{"/f.go", "/r.go"},
			}},
			// A second human turn, delegated to a sub-agent — the machine-turn filter must
			// drop it from the render even though the measured writer still rows it.
			{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{IsSubAgent: true}},
		},
		usage: &sessionintelligencepb.UsageMetadata{},
	}

	content, _ := buildSpineContent("org-1", "rr-1", sp)

	require.Contains(t, content, "=== TURN 0 (author=SESSION_EVENT_AUTHOR_HUMAN) ===")
	assert.NotContains(t, content, "=== TURN 1 ", "the delegated turn-group is machine-turn-filtered out of the render (only turn 0 remains)")
	// The taggable header exposes ONLY the render sequence — never the raw whole-session
	// index — so R has one unambiguous number to echo as prompt_index.
	assert.NotContains(t, content, "index=", "the raw whole-session index must not leak into the taggable turn header")
	assert.Contains(t, content, "model: claude-sonnet-4-6 ["+measuredTag+"]")
	assert.Contains(t, content, "tokens: input=100 output=20 cache_read=0 cache_write=0 ["+measuredTag+"]")
	assert.Contains(t, content, "files_edited: /f.go ["+measuredTag+"]")
	assert.Contains(t, content, "files_read: /r.go ["+measuredTag+"]")
	assert.Contains(t, content, "=== CAPABILITIES ===")
	assert.Contains(t, content, "token_fidelity_capability: full ["+measuredTag+"]")
}

// TestPrefixHumanTurnFilter_RoleAndSubagentTable pins the machine-turn filter's role/
// is_subagent test AND shows it deliberately diverges from the measured writer's author
// derivation: the filter inspects the group's OPENING prompt's role AND is_subagent
// (sessionintelligence.IsHumanPrompt), while buildMeasuredTurnRows' author looks at is_subagent alone
// across every message in the group, regardless of role. An assistant-opened group with no
// sub-agent flag is therefore filtered out (it never opened with a human prompt) yet still
// carries author=HUMAN on its measured row — two different questions, not a shared verdict.
func TestPrefixHumanTurnFilter_RoleAndSubagentTable(t *testing.T) {
	tests := []struct {
		name       string
		role       sessionintelligencepb.Role
		isSubagent bool
		wantKept   bool
		wantAuthor string
	}{
		{name: "user, not subagent -> human-authored, kept", role: sessionintelligencepb.Role_ROLE_USER, isSubagent: false, wantKept: true, wantAuthor: "SESSION_EVENT_AUTHOR_HUMAN"},
		{name: "user, subagent -> delegated agent folded into user role, dropped", role: sessionintelligencepb.Role_ROLE_USER, isSubagent: true, wantKept: false, wantAuthor: "SESSION_EVENT_AUTHOR_DELEGATED_AGENT"},
		{name: "assistant, not subagent -> not a human-opened group, dropped", role: sessionintelligencepb.Role_ROLE_ASSISTANT, isSubagent: false, wantKept: false, wantAuthor: "SESSION_EVENT_AUTHOR_HUMAN"},
		{name: "assistant, subagent -> not a human-opened group, dropped", role: sessionintelligencepb.Role_ROLE_ASSISTANT, isSubagent: true, wantKept: false, wantAuthor: "SESSION_EVENT_AUTHOR_DELEGATED_AGENT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group := []*sessionintelligencepb.Prompt{{Role: tt.role, Metadata: &sessionintelligencepb.PromptMetadata{IsSubAgent: tt.isSubagent}}}
			assert.Equal(t, tt.wantKept, prefixHumanTurnFilter(group))

			rows := buildMeasuredTurnRows("org", "rr", &spineResult{prompts: group})
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].Author)
			assert.Equal(t, tt.wantAuthor, *rows[0].Author)
		})
	}
}

// TestPrefixHumanTurnFilter_PreambleOpenedGroupKept guards the H5 regression: splitHumanTurns
// folds a leading non-user preamble into the FIRST group ahead of its user prompt (its own
// TestSplitHumanTurns_LeadingNonUserFoldsIntoFirstTurn pins groups[0] = [asst, user, asst]),
// so the filter must find the group's OPENING user prompt rather than inspecting group[0] —
// else the whole first turn is wrongly dropped as machine-authored.
func TestPrefixHumanTurnFilter_PreambleOpenedGroupKept(t *testing.T) {
	group := []*sessionintelligencepb.Prompt{
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT}, // a leading preamble folded ahead of the user turn
		userPrompt(),
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "on it"},
	}
	assert.True(t, prefixHumanTurnFilter(group), "a preamble-opened group is still human-authored via its opening user prompt")
}

// TestPrefixHumanTurnFilter_NoUserPromptDropped guards the degenerate case splitHumanTurns
// also produces (an all-assistant body with no user prompt at all, kept as one group so the
// content still renders): such a group has no opening user prompt to judge, so it is never
// human-authored.
func TestPrefixHumanTurnFilter_NoUserPromptDropped(t *testing.T) {
	group := []*sessionintelligencepb.Prompt{
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT},
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT},
	}
	assert.False(t, prefixHumanTurnFilter(group))
}

// TestBuildSpineContent_PreambleOpenedFirstTurnRendered is the end-to-end H5 regression: a
// session whose first turn-group opens with a non-user preamble (splitHumanTurns folds it
// ahead of the user prompt) must still render that turn as turn 0, not drop the whole first
// human turn because prefixHumanTurnFilter inspected the preamble instead of the group's
// opening user prompt.
func TestBuildSpineContent_PreambleOpenedFirstTurnRendered(t *testing.T) {
	sp := &spineResult{
		prompts: []*sessionintelligencepb.Prompt{
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "preamble"}, // folds into turn 0 ahead of the user prompt
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "on it"},
		},
		usage: &sessionintelligencepb.UsageMetadata{},
	}
	content, _ := buildSpineContent("org-1", "rr-1", sp)
	assert.Contains(t, content, "=== TURN 0 (author=SESSION_EVENT_AUTHOR_HUMAN) ===",
		"the preamble-opened first turn is rendered, not dropped")
}

// TestCapabilityFor_TokenFidelityTable pins the four-row design table (measured→full,
// native-partial→partial, estimate→estimated, unspecified→none) over a message that DID
// report tokens — capability reads the worst reported tier, never an enablement gate or an
// IDE-type allowlist.
func TestCapabilityFor_TokenFidelityTable(t *testing.T) {
	tests := []struct {
		name     string
		fidelity sessionintelligencepb.TokenFidelity
		want     capability
	}{
		{name: "measured -> full", fidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED, want: capabilityFull},
		{name: "native-partial -> partial", fidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_NATIVE_PARTIAL, want: capabilityPartial},
		{name: "estimate -> estimated", fidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_ESTIMATE, want: capabilityEstimated},
		{name: "unspecified (tokens present, no fidelity reported) -> none", fidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_UNSPECIFIED, want: capabilityNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := []*sessionintelligencepb.Prompt{
				userPrompt(),
				{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
					MessageId: "m1", InputTokens: 10, OutputTokens: 5, TokenFidelity: tt.fidelity,
				}},
			}
			assert.Equal(t, tt.want, capabilityFor(prompts))
		})
	}
}

// TestCapabilityFor_NoTelemetryOldDesktop pins the "no session_events / old desktop" floor:
// a conversation whose messages carry no token-bearing metadata at all has no fidelity
// signal to rank, and must report none rather than a false measured zero.
func TestCapabilityFor_NoTelemetryOldDesktop(t *testing.T) {
	prompts := []*sessionintelligencepb.Prompt{
		userPrompt(),
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "done"},
	}
	assert.Equal(t, capabilityNone, capabilityFor(prompts))
}

// TestCapabilityFor_TokenBearingMessageMissingFidelityDegradesToNone is the H3 regression: a
// turn whose token-bearing messages carry BOTH a MEASURED report and a missing (UNSPECIFIED)
// one must read capability none, even though the turn-collapsed measured row would read
// "full" — applyMeasuredTurnColumns (turngrouper.go) deliberately skips an UNSPECIFIED-
// reporting message so it does not drag the whole turn's stored fidelity, which is correct
// for that DB row but wrong for capability: any missing fidelity among counted (token-
// bearing) messages must degrade the session to none.
func TestCapabilityFor_TokenBearingMessageMissingFidelityDegradesToNone(t *testing.T) {
	sp := &spineResult{prompts: []*sessionintelligencepb.Prompt{
		userPrompt(),
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
			MessageId: "m1", InputTokens: 10, OutputTokens: 5,
			TokenFidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED,
		}},
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
			MessageId: "m2", InputTokens: 3, OutputTokens: 1,
			// TokenFidelity left at its zero value (UNSPECIFIED) — a token-bearing message
			// with no reported fidelity.
		}},
	}}
	// Documents the divergence this fix corrects: the turn-collapsed measured row still
	// reads MEASURED (m2's missing fidelity does not drag the turn — turngrouper.go), but
	// capability must not inherit that collapse.
	rows := buildMeasuredTurnRows("org", "rr", sp)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].TokenFidelity)
	assert.Equal(t, sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED, *rows[0].TokenFidelity)

	assert.Equal(t, capabilityNone, capabilityFor(sp.prompts))
}

// TestCapabilityFor_EmptyConversation guards the vacuous case (no messages at all — e.g. a
// body with no prompts) against a panic or a spuriously-non-none result.
func TestCapabilityFor_EmptyConversation(t *testing.T) {
	assert.Equal(t, capabilityNone, capabilityFor(nil))
}

// TestProducerTags_CarriesVersionStamp pins that the version-stamp plumbing
// (task_type/organization_id/vocabulary_version) survived the content-builder refactor
// unchanged — producerTags is untouched by this change, and ProducerRequest.Tags is built
// from it exactly as before.
func TestProducerTags_CarriesVersionStamp(t *testing.T) {
	tags := producerTags("analyze_read", "org-1", 7)
	assert.Equal(t, "analyze_read", tags["task_type"])
	assert.Equal(t, "org-1", tags["organization_id"])
	assert.Equal(t, "7", tags["vocabulary_version"])
}
