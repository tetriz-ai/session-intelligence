// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao/mocks"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
)

func i32(v int32) *int32     { return &v }
func f64(v float64) *float64 { return &v }
func userPrompt() *sessionintelligencepb.Prompt {
	return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{}}
}

// TestBuildMeasuredTurnRows_FoldsPerTurn pins the grouper's per-turn fold: one turn per
// human prompt (the shared splitHumanTurns cut), token breakdown summed with message_id
// dedup, worst fidelity, unioned files (edited beats read), summed lines, and the peak
// context percentage normalized to 0–100.
func TestBuildMeasuredTurnRows_FoldsPerTurn(t *testing.T) {
	t.Parallel()
	sha := "sha-1"
	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		contentSha256:         sha,
		repoName:              "owner/repo",
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 5,
				TokenFidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED,
				FilesEdited:   []string{"/f.ts"}, RelevantFiles: []string{"/r.ts", "/f.ts"},
				LinesAdded: i32(3), LinesRemoved: i32(1), ContextUsagePercent: f64(0.42),
			}},
			// A repeated message_id must not double-count tokens; a NATIVE_PARTIAL message
			// drags the turn's fidelity to the worse tier.
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 100, OutputTokens: 20,
				TokenFidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_NATIVE_PARTIAL,
				LinesAdded:    i32(2), ContextUsagePercent: f64(0.55),
			}},
			// A second human turn, sub-agent, no edits.
			{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{IsSubAgent: true}},
		},
	}

	rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
	require.Len(t, rows, 2, "two human turns → two rows")

	turn0 := rows[0]
	assert.Equal(t, int32(0), turn0.PromptIndex)
	require.NotNil(t, turn0.SourceContentSha256)
	assert.Equal(t, sha, *turn0.SourceContentSha256)
	require.NotNil(t, turn0.TokenBreakdown)
	assert.Equal(t, int64(100), turn0.TokenBreakdown.GetInputTokens(), "repeated message_id deduped, not summed")
	assert.Equal(t, int64(20), turn0.TokenBreakdown.GetOutputTokens())
	assert.Equal(t, int64(5), turn0.TokenBreakdown.GetCacheReadTokens())
	require.NotNil(t, turn0.TokenFidelity)
	assert.Equal(t, sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_NATIVE_PARTIAL, *turn0.TokenFidelity, "worst tier wins")
	require.NotNil(t, turn0.LinesAdded)
	assert.Equal(t, int32(5), *turn0.LinesAdded, "3 + 2 across the turn's messages")
	require.NotNil(t, turn0.LinesRemoved)
	assert.Equal(t, int32(1), *turn0.LinesRemoved)
	require.NotNil(t, turn0.ContextPct)
	assert.InDelta(t, 55.0, *turn0.ContextPct, 0.001, "peak 0.55 → 55.0")
	require.NotNil(t, turn0.IsSubagent)
	assert.False(t, *turn0.IsSubagent)
	// files_touched: /f.ts EDITED (edit beats read), /r.ts READ.
	require.NotNil(t, turn0.FilesTouched)
	kinds := map[string]sessionintelligencepb.TouchKind{}
	for _, it := range turn0.FilesTouched.GetItems() {
		kinds[it.GetPath()] = it.GetTouchKind()
	}
	assert.Equal(t, sessionintelligencepb.TouchKind_TOUCH_KIND_EDITED, kinds["/f.ts"])
	assert.Equal(t, sessionintelligencepb.TouchKind_TOUCH_KIND_READ, kinds["/r.ts"])
	require.NotNil(t, turn0.RepoFullName)
	assert.Equal(t, "owner/repo", *turn0.RepoFullName)

	turn1 := rows[1]
	assert.Equal(t, int32(1), turn1.PromptIndex)
	require.NotNil(t, turn1.IsSubagent)
	assert.True(t, *turn1.IsSubagent, "a sub-agent turn is flagged")
	require.NotNil(t, turn1.Author)
	assert.Equal(t, "SESSION_EVENT_AUTHOR_DELEGATED_AGENT", *turn1.Author)
	assert.Nil(t, turn1.TokenBreakdown, "a turn with no token-bearing message carries no breakdown")
}

// TestNormalizeSurface pins the raw-session-source → run-surface mapping across every provider's
// real stamped values, plus case/whitespace normalization, the sdk-* prefix rule, and the
// unrecognized/empty → NULL rule (an unknown surface is never invented).
func TestNormalizeSurface(t *testing.T) {
	t.Parallel()
	const null = sessionintelligencepb.SessionSurface_SESSION_SURFACE_UNSPECIFIED // sentinel: assert nil (NULL)
	tests := []struct {
		raw  string
		want sessionintelligencepb.SessionSurface // UNSPECIFIED means "assert NULL"
	}{
		{"cli", sessionintelligencepb.SessionSurface_SESSION_SURFACE_CLI},
		{"kiro-cli", sessionintelligencepb.SessionSurface_SESSION_SURFACE_CLI},
		{"CLI", sessionintelligencepb.SessionSurface_SESSION_SURFACE_CLI},
		{" cli ", sessionintelligencepb.SessionSurface_SESSION_SURFACE_CLI},
		{"vscode", sessionintelligencepb.SessionSurface_SESSION_SURFACE_IDE},
		{"kiro-ide", sessionintelligencepb.SessionSurface_SESSION_SURFACE_IDE},
		{"claude-vscode", sessionintelligencepb.SessionSurface_SESSION_SURFACE_IDE},
		{"ide", sessionintelligencepb.SessionSurface_SESSION_SURFACE_IDE},
		{"kiro", sessionintelligencepb.SessionSurface_SESSION_SURFACE_DESKTOP},
		{"claude-desktop", sessionintelligencepb.SessionSurface_SESSION_SURFACE_DESKTOP},
		{"exec", sessionintelligencepb.SessionSurface_SESSION_SURFACE_SDK},
		{"sdk-cli", sessionintelligencepb.SessionSurface_SESSION_SURFACE_SDK},
		{"sdk-py", sessionintelligencepb.SessionSurface_SESSION_SURFACE_SDK},
		{"cowork", sessionintelligencepb.SessionSurface_SESSION_SURFACE_COWORK},
		{"subagent", sessionintelligencepb.SessionSurface_SESSION_SURFACE_SUBAGENT},
		// kiro-crew is a real stamped value, but Crew has no turn grain (grouping-excluded) so it
		// never reaches a turn row — NULL is the documented intent, not an omission.
		{"kiro-crew", null},
		// bare "sdk" / "sdkfoo" are NOT the hyphenated CC entrypoints (sdk-cli/sdk-py) → NULL.
		{"sdk", null},
		{"sdkfoo", null},
		{"", null},
		{"copilot", null},
		{"antigravity", null},
		{"nonsense", null},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got := normalizeSurface(tt.raw)
			if tt.want == null {
				assert.Nil(t, got, "%q → NULL", tt.raw)
				return
			}
			require.NotNil(t, got, "%q → %v", tt.raw, tt.want)
			assert.Equal(t, tt.want, *got, "%q", tt.raw)
		})
	}
}

// TestBuildMeasuredTurnRows_ClaudeCodeContextPctFromModelWindow pins the Claude Code
// per-turn context% derivation: CC reports per-message token counts but no vendor context
// indicator, so the turn's context% comes from effective_tokens / model_window using the
// authoritative model→window table (sessionintelligence.ContextWindowForModel). Only a KNOWN model
// contributes — an unknown/foreign model id leaves the field unset (honest-unset, distinct
// from a fabricated 0). A vendor-supplied percent, when present, still wins.
//
// The "known model" case is grounded in a recorded coding-agent turn
// (input=2, cache_read=147615, cache_creation=4104 → effective 151721; window 1,000,000):
// 151721 / 1_000_000 * 100 = 15.1721.
func TestBuildMeasuredTurnRows_ClaudeCodeContextPctFromModelWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		model          string
		input          int64
		cacheRead      int64
		cacheCreation  int64
		vendorPct      *float64 // PromptMetadata.context_usage_percent (0.0–1.0), when the source supplies one
		wantContextSet bool
		wantContextPct float64
	}{
		{
			name:           "known model derives from window (real opus-4-8 turn)",
			model:          "claude-opus-4-8",
			input:          2,
			cacheRead:      147615,
			cacheCreation:  4104,
			wantContextSet: true,
			wantContextPct: 15.1721, // 151721 / 1_000_000 * 100
		},
		{
			name:           "dated haiku id resolves via date-strip; heavy cache_read clamps to 100",
			model:          "claude-haiku-4-5-20251001", // → claude-haiku-4-5, 200K window
			cacheRead:      250000,                      // 125% of window → clamped
			wantContextSet: true,
			wantContextPct: 100,
		},
		{
			name:           "unknown model leaves context unset",
			model:          "some-unknown-model-xyz",
			input:          5000,
			wantContextSet: false,
		},
		{
			name:           "vendor-supplied percent wins over the model-window derivation",
			model:          "claude-opus-4-8",
			input:          2,
			cacheRead:      147615,
			cacheCreation:  4104,
			vendorPct:      f64(0.30),
			wantContextSet: true,
			wantContextPct: 30.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sp := &spineResult{
				conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				contentSha256:         "sha",
				ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
				prompts: []*sessionintelligencepb.Prompt{
					userPrompt(),
					{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
						MessageId:                "m1",
						Model:                    tt.model,
						InputTokens:              tt.input,
						CacheReadInputTokens:     tt.cacheRead,
						CacheCreationInputTokens: tt.cacheCreation,
						ContextUsagePercent:      tt.vendorPct,
					}},
				},
			}
			rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
			require.Len(t, rows, 1)
			if !tt.wantContextSet {
				assert.Nil(t, rows[0].ContextPct, "unknown model → context% unset")
				return
			}
			require.NotNil(t, rows[0].ContextPct)
			assert.InDelta(t, tt.wantContextPct, *rows[0].ContextPct, 0.001)
		})
	}
}

// TestBuildMeasuredTurnRows_Surface pins that the grouper reads UsageMetadata.session_source and
// stamps the normalized surface on the turn row (a per-session value, so every turn carries it),
// NULL when the source is empty.
func TestBuildMeasuredTurnRows_Surface(t *testing.T) {
	t.Parallel()
	mk := func(src string) *spineResult {
		return &spineResult{
			conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			contentSha256:         "sha-1",
			ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
			usage:                 &sessionintelligencepb.UsageMetadata{SessionSource: src},
			prompts:               []*sessionintelligencepb.Prompt{userPrompt(), userPrompt()},
		}
	}
	rows := buildMeasuredTurnRows("org-1", "rr-1", mk("claude-desktop"))
	require.Len(t, rows, 2)
	for i, row := range rows {
		require.NotNil(t, row.Surface, "turn %d surface", i)
		assert.Equal(t, sessionintelligencepb.SessionSurface_SESSION_SURFACE_DESKTOP, *row.Surface, "turn %d surface stamped on every turn", i)
	}

	empty := buildMeasuredTurnRows("org-1", "rr-2", mk(""))
	require.Len(t, empty, 2)
	assert.Nil(t, empty[0].Surface, "empty session_source → NULL surface")
}

// TestBuildMeasuredTurnRows_ContextTokens pins context_tokens as the turn's PEAK single-message
// context footprint (input + cache_read + cache_creation), grounded in a real CC turn whose peak
// message was 2 + 117146 + 748 = 117896 tokens; a smaller earlier message must not win. A turn
// whose messages carry a percent but no token counts (a vendor-percent-only provider) yields NULL,
// since the raw footprint is not computable there.
func TestBuildMeasuredTurnRows_ContextTokens(t *testing.T) {
	t.Parallel()
	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		contentSha256:         "sha-1",
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				InputTokens: 5, CacheReadInputTokens: 1000, CacheCreationInputTokens: 20, // 1025, not the peak
			}},
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				InputTokens: 2, CacheReadInputTokens: 117146, CacheCreationInputTokens: 748, // 117896, the peak
			}},
			// Turn 1: a percent but no token counts → footprint not computable → NULL.
			{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{ContextUsagePercent: f64(0.5)}},
		},
	}
	rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].ContextTokens)
	assert.Equal(t, int64(117896), *rows[0].ContextTokens, "peak single-message footprint, not the smaller earlier message")
	assert.Nil(t, rows[1].ContextTokens, "percent but no token counts → NULL footprint")
}

// TestBuildMeasuredTurnRows_StartedEndedAt pins the turn-span columns: started_at = the earliest
// message timestamp in the turn, ended_at = the latest (min/max — both extremes sit at non-final
// source positions so the assertion catches a first/last regression on either bound), the span is
// folded independent of per-message metadata (a timestamped nil-metadata message still bounds the
// turn), and both bounds are NULL when no message in the turn carries a timestamp. Only the span
// BOUNDS (04:23:32.323Z … 04:28:07.419Z) come from a real CC subagent transcript; the out-of-order
// interleaving, the intermediate time, the nil-metadata message, and the no-timestamp turn are
// synthetic — constructed to pin min/max, the metadata-independent fold, and the NULL path.
func TestBuildMeasuredTurnRows_StartedEndedAt(t *testing.T) {
	t.Parallel()
	ts := func(s string) *timestamppb.Timestamp {
		tm, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return timestamppb.New(tm)
	}
	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		contentSha256:         "sha-1",
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			// Turn 0: timestamps out of source order — both the min and the max sit at non-final
			// positions, so ended_at=max is distinguished from ended_at=last (and started_at=min
			// from first). The min is carried by a message with NIL metadata, proving the span is
			// folded before the metadata guard.
			{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{}, Timestamp: ts("2026-08-13T04:25:00Z")},
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{}, Timestamp: ts("2026-08-13T04:28:07.419Z")}, // max, non-final
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Timestamp: ts("2026-08-13T04:23:32.323Z")},                                                    // min, non-final, NIL metadata
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{}, Timestamp: ts("2026-08-13T04:26:00Z")},     // final, not an extreme
			// Turn 1: no message carries a timestamp → both bounds NULL.
			{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{}},
		},
	}
	rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
	require.Len(t, rows, 2)

	require.NotNil(t, rows[0].StartedAt)
	require.NotNil(t, rows[0].EndedAt)
	assert.True(t, ts("2026-08-13T04:23:32.323Z").AsTime().Equal(*rows[0].StartedAt), "started_at = earliest ts (a nil-metadata, non-first message)")
	assert.True(t, ts("2026-08-13T04:28:07.419Z").AsTime().Equal(*rows[0].EndedAt), "ended_at = latest ts (a non-last message)")

	assert.Nil(t, rows[1].StartedAt, "no message timestamp → NULL")
	assert.Nil(t, rows[1].EndedAt, "no message timestamp → NULL")
}

// TestBuildMeasuredTurnRows_ClaudeCodeContextPctPeakAcrossMessages pins the derived path's
// central invariant — the turn's context% is the PEAK across its distinct messages, and an
// earlier peak is not overwritten by a later smaller message (nor vice-versa). Both orderings
// are asserted so a first-seen / last-seen / summed regression would fail. A known-model
// message that is context-empty (output-only) contributes nothing.
func TestBuildMeasuredTurnRows_ClaudeCodeContextPctPeakAcrossMessages(t *testing.T) {
	t.Parallel()
	// Two distinct opus-4-8 messages (window 1,000,000): a smaller 10% turn and a larger 40%.
	small := &sessionintelligencepb.PromptMetadata{MessageId: "m1", Model: "claude-opus-4-8", CacheReadInputTokens: 100000} // 10.0
	large := &sessionintelligencepb.PromptMetadata{MessageId: "m2", Model: "claude-opus-4-8", CacheReadInputTokens: 400000} // 40.0
	empty := &sessionintelligencepb.PromptMetadata{MessageId: "m3", Model: "claude-opus-4-8", OutputTokens: 500}            // no context tokens

	tests := []struct {
		name     string
		messages []*sessionintelligencepb.PromptMetadata
		want     float64
	}{
		{"ascending — later larger message raises the peak", []*sessionintelligencepb.PromptMetadata{small, large}, 40.0},
		{"descending — earlier peak survives a later smaller message", []*sessionintelligencepb.PromptMetadata{large, small}, 40.0},
		{"context-empty message does not lower the peak", []*sessionintelligencepb.PromptMetadata{large, empty}, 40.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			prompts := []*sessionintelligencepb.Prompt{userPrompt()}
			for _, md := range tt.messages {
				prompts = append(prompts, &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: md})
			}
			sp := &spineResult{
				conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				contentSha256:         "sha",
				ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
				prompts:               prompts,
			}
			rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].ContextPct)
			assert.InDelta(t, tt.want, *rows[0].ContextPct, 0.001)
		})
	}
}

// TestBuildMeasuredTurnRows_NonClaudeCodeNoDerivedContext pins that the model-window
// derivation is Claude-Code-only: another turn-grouped source that reports tokens but no
// vendor percent (Codex, Cursor) carries no derived context% — those surfaces own their
// own context signal separately.
func TestBuildMeasuredTurnRows_NonClaudeCodeNoDerivedContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ide  sessionintelligencepb.IdeType
	}{
		{"codex", sessionintelligencepb.IdeType_IDE_TYPE_CODEX},
		{"cursor", sessionintelligencepb.IdeType_IDE_TYPE_CURSOR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sp := &spineResult{
				conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				contentSha256:         "sha",
				ideType:               tt.ide,
				prompts: []*sessionintelligencepb.Prompt{
					userPrompt(),
					{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
						MessageId: "m1", Model: "claude-opus-4-8", InputTokens: 2, CacheReadInputTokens: 147615,
					}},
				},
			}
			rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
			require.Len(t, rows, 1)
			assert.Nil(t, rows[0].ContextPct, "non-Claude-Code source gets no model-window derivation")
		})
	}
}

// TestBuildMeasuredTurnRows_FoldsReasoningTokens pins that the per-message reasoning bucket
// (PromptMetadata.reasoning_tokens) is carried into the breakdown's reasoning_output_tokens,
// deduped by message_id like the other counts. It is an "of which" subset of output_tokens,
// so output_tokens is NOT inflated by it.
func TestBuildMeasuredTurnRows_FoldsReasoningTokens(t *testing.T) {
	t.Parallel()
	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 100, OutputTokens: 40, ReasoningTokens: proto.Int64(12),
			}},
			// Repeated message_id must not double-count the reasoning bucket either.
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 100, OutputTokens: 40, ReasoningTokens: proto.Int64(12),
			}},
		},
	}

	rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].TokenBreakdown)
	assert.Equal(t, int64(12), rows[0].TokenBreakdown.GetReasoningOutputTokens(), "reasoning bucket folded, message_id deduped")
	assert.Equal(t, int64(40), rows[0].TokenBreakdown.GetOutputTokens(), "reasoning is an of-which subset; output_tokens is not inflated")
}

// TestBuildMeasuredTurnRows_FilesTouchedDeterministic pins that files_touched has a stable,
// sorted order across runs of the SAME input — the payload is fed by Go maps, whose iteration
// order is randomized, so an unsorted build would make files_touched (and any byte golden over
// it) flaky. Edited paths sort ascending first (the stronger touch), then read-only paths.
func TestBuildMeasuredTurnRows_FilesTouchedDeterministic(t *testing.T) {
	t.Parallel()
	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		contentSha256:         "sha-det",
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId:   "m1",
				InputTokens: 1,
				FilesEdited: []string{"/z.go", "/a.go", "/m.go"},
				// /a.go is both edited and read → EDITED wins; the rest are read-only.
				RelevantFiles: []string{"/a.go", "/y.txt", "/b.txt"},
			}},
		},
	}

	var want []string
	for run := 0; run < 5; run++ {
		rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].FilesTouched)
		got := make([]string, 0, len(rows[0].FilesTouched.GetItems()))
		for _, it := range rows[0].FilesTouched.GetItems() {
			got = append(got, it.GetTouchKind().String()+":"+it.GetPath())
		}
		if run == 0 {
			want = got
			// Edited sorted first, then read-only sorted; /a.go is EDITED (not repeated as READ).
			assert.Equal(t, []string{
				"TOUCH_KIND_EDITED:/a.go",
				"TOUCH_KIND_EDITED:/m.go",
				"TOUCH_KIND_EDITED:/z.go",
				"TOUCH_KIND_READ:/b.txt",
				"TOUCH_KIND_READ:/y.txt",
			}, got, "edited-then-read, each block ascending")
			continue
		}
		assert.Equal(t, want, got, "files_touched order is stable across runs of the same input")
	}
}

// TestBuildMeasuredTurnRows_FoldsModels pins that a turn whose messages carry a per-message
// model populates the row's Models (deduped, first-seen order); effort is left empty because
// per-message metadata carries the model but not its effort.
func TestBuildMeasuredTurnRows_FoldsModels(t *testing.T) {
	t.Parallel()
	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		contentSha256:         "sha-model",
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", Model: "claude-sonnet", InputTokens: 10,
			}},
			// Same model repeated → deduped; then a second distinct model.
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m2", Model: "claude-sonnet", InputTokens: 10,
			}},
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m3", Model: "claude-opus", InputTokens: 10,
			}},
		},
	}

	rows := buildMeasuredTurnRows("org-1", "rr-1", sp)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Models, "a turn whose messages carry a model populates Models")
	got := make([]string, 0)
	for _, m := range rows[0].Models.GetItems() {
		got = append(got, m.GetModel())
		assert.Empty(t, m.GetEffort(), "effort is not carried on per-message metadata")
	}
	assert.Equal(t, []string{"claude-sonnet", "claude-opus"}, got, "deduped, first-seen order")

	// A turn whose messages carry no model leaves Models nil (present-aware).
	noModel := &spineResult{
		conversationStartedAt: sp.conversationStartedAt,
		contentSha256:         "sha-nomodel",
		ideType:               sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		prompts: []*sessionintelligencepb.Prompt{
			userPrompt(),
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{MessageId: "x", InputTokens: 1}},
		},
	}
	nrows := buildMeasuredTurnRows("org-1", "rr-2", noModel)
	require.Len(t, nrows, 1)
	assert.Nil(t, nrows[0].Models, "no per-message model → Models stays nil")
}

// TestSupportsTurnGrouping pins the supported-source membership: turn rows are cut only for
// the in-scope providers; every other source is excluded, and Kiro's autonomous Crew surface
// is excluded even though it shares IDE_TYPE_KIRO with interactive Kiro (session-turn-telemetry
// design).
func TestSupportsTurnGrouping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		ide           sessionintelligencepb.IdeType
		sessionSource string
		want          bool
	}{
		{"claude code", sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE, "", true},
		{"cursor", sessionintelligencepb.IdeType_IDE_TYPE_CURSOR, "", true},
		{"codex", sessionintelligencepb.IdeType_IDE_TYPE_CODEX, "", true},
		{"interactive kiro", sessionintelligencepb.IdeType_IDE_TYPE_KIRO, "", true},
		{"kiro crew excluded", sessionintelligencepb.IdeType_IDE_TYPE_KIRO, sessionintelligence.SessionSourceKiroCrew, false},
		{"copilot", sessionintelligencepb.IdeType_IDE_TYPE_COPILOT, "", false},
		{"antigravity", sessionintelligencepb.IdeType_IDE_TYPE_ANTIGRAVITY, "", false},
		{"unspecified", sessionintelligencepb.IdeType_IDE_TYPE_UNSPECIFIED, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, supportsTurnGrouping(tt.ide, tt.sessionSource))
		})
	}
}

// TestWriteMeasuredTurns_UnsupportedSourceWritesNoTurnGrain pins the supported-source predicate
// at the DB-write boundary: an unsupported source carrying a real, groupable conversation writes
// no measured turn rows, so it never contaminates the turn grain. (buildMeasuredTurnRows itself is
// the shared content builder and now cuts rows for every source — R still judges a deferred
// source; the gate lives here, in writeMeasuredTurns.) It never retires a prior tail EXCEPT for
// Kiro Crew: Crew shares IDE_TYPE_KIRO with interactive Kiro (supported) and is told apart only by
// session_source, so a legacy upload that grouped a Crew session as interactive Kiro left live
// rows that must be retired once session_source resolves — that single case retires the whole
// prior tail (SoftDeleteMeasuredBeyond at -1) and writes no rows. The supported path writing the
// grain for the same shape is TestProcessFlat_WritesTurnPhaseGrain.
func TestWriteMeasuredTurns_UnsupportedSourceWritesNoTurnGrain(t *testing.T) {
	t.Parallel()
	prompts := []*sessionintelligencepb.Prompt{
		userPrompt(),
		{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Metadata: &sessionintelligencepb.PromptMetadata{
			MessageId: "m1", InputTokens: 100, OutputTokens: 20,
		}},
	}
	tests := []struct {
		name          string
		ide           sessionintelligencepb.IdeType
		sessionSource string
		// wantCrewRetire is Kiro Crew's lone exception: it retires the whole prior tail
		// (SoftDeleteMeasuredBeyond at -1) to clean up rows a legacy upload wrote before
		// session_source resolved. Every always-unsupported source (by ide_type) never had rows.
		wantCrewRetire bool
	}{
		{"copilot", sessionintelligencepb.IdeType_IDE_TYPE_COPILOT, "", false},
		{"antigravity", sessionintelligencepb.IdeType_IDE_TYPE_ANTIGRAVITY, "", false},
		{"unspecified", sessionintelligencepb.IdeType_IDE_TYPE_UNSPECIFIED, "", false},
		{"kiro crew", sessionintelligencepb.IdeType_IDE_TYPE_KIRO, sessionintelligence.SessionSourceKiroCrew, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			turnDAO := mocks.NewMockISessionTurnAnalysisDAO(ctrl)
			// No source writes measured rows here — an unexpected UpsertMeasured fails the mock.
			turnDAO.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Times(0)
			if tt.wantCrewRetire {
				// Kiro Crew retires the whole prior tail once and writes no rows.
				turnDAO.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), "org-1", "rr-1", int32(-1)).Return(int64(0), nil).Times(1)
			} else {
				// Every other unsupported source never had rows, so it never retires.
				turnDAO.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
			}

			sp := &spineResult{
				conversationStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				contentSha256:         "sha-1",
				ideType:               tt.ide,
				usage:                 &sessionintelligencepb.UsageMetadata{SessionSource: tt.sessionSource},
				prompts:               prompts,
			}
			p := &SessionAnalysisProcessor{sessionTurnDAO: turnDAO, cfg: measuredWriteTestConfig{}}
			record := &model.RawRecordModel{ID: "rr-1", OrganizationID: "org-1"}
			if err := p.writeMeasuredTurns(context.Background(), "org-1", record, sp, buildMeasuredTurnRows("org-1", record.ID, sp)); err != nil {
				t.Fatalf("writeMeasuredTurns: %v", err)
			}
		})
	}
}

// TestTurnGroupingSupportedIDETypeSQLList pins the exact IN-list literal the coverage
// denominators scope by. The coverage query builds its IN (...) from this helper, so a source
// added to turnGroupingSupportedIDETypes updates that query for free; this test pins the literal
// so such a change is explicit and reviewed.
func TestTurnGroupingSupportedIDETypeSQLList(t *testing.T) {
	t.Parallel()
	assert.Equal(t,
		"'IDE_TYPE_CLAUDE_CODE', 'IDE_TYPE_CURSOR', 'IDE_TYPE_CODEX', 'IDE_TYPE_KIRO'",
		turnGroupingSupportedIDETypeSQLList(),
	)
}
