// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

// Unit coverage for the derived-column helpers added with the empty-column population work:
// the phase range/turn/token derivations and their domain-clamping, plus the two blob-measured
// scalars (active_share, first_user_msg_ts) that ApplyMeasuredBlobColumns now populates.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

func TestPhaseTurnCountFromRange(t *testing.T) {
	tests := []struct {
		name       string
		start, end int32
		want       *int32
	}{
		{name: "single-turn span", start: 3, end: 3, want: int32Ptr(1)},
		{name: "multi-turn span", start: 0, end: 9, want: int32Ptr(10)},
		{name: "inverted range is nil", start: 5, end: 2, want: nil},
		{name: "extreme endpoints do not overflow int32", start: -2147483648, end: 2147483647, want: int32Ptr(2147483647)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := phaseTurnCountFromRange(tt.start, tt.end)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

func TestPhaseTokens(t *testing.T) {
	sp := &spineResult{promptTokenCosts: []int64{10, 20, 30, 40}} // domain [0,3]
	tests := []struct {
		name       string
		start, end int32
		want       *int64
	}{
		{name: "in-domain span sums the range", start: 1, end: 2, want: int64Ptr(50)},
		{name: "whole domain", start: 0, end: 3, want: int64Ptr(100)},
		{name: "range entirely past the domain is nil, not zero", start: 100, end: 200, want: nil},
		{name: "inverted range is nil", start: 3, end: 1, want: nil},
		{name: "range overrunning the end sums only the in-domain prefix", start: 2, end: 99, want: int64Ptr(70)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase := &PhaseJudgment{RangeStart: int32Ptr(tt.start), RangeEnd: int32Ptr(tt.end)}
			got := phaseTokens(sp, phase)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

func TestClampPhaseRangesToDomain(t *testing.T) {
	promptCount := int32(10) // valid indices [0,9]

	t.Run("clamps an overrunning end and re-derives turn_count", func(t *testing.T) {
		phases := []*PhaseJudgment{{PhaseOrdinal: 0, RangeStart: int32Ptr(0), RangeEnd: int32Ptr(10000), TurnCount: int32Ptr(10001)}}
		got := clampPhaseRangesToDomain(phases, promptCount)
		require.Len(t, got, 1)
		assert.Equal(t, int32(0), *got[0].RangeStart)
		assert.Equal(t, int32(9), *got[0].RangeEnd, "end clamped to promptCount-1")
		require.NotNil(t, got[0].TurnCount)
		assert.Equal(t, int32(10), *got[0].TurnCount, "turn_count re-derived from the clamped range")
	})

	t.Run("drops a phase whose range is entirely out of the domain", func(t *testing.T) {
		phases := []*PhaseJudgment{
			{PhaseOrdinal: 0, RangeStart: int32Ptr(0), RangeEnd: int32Ptr(4)},
			{PhaseOrdinal: 1, RangeStart: int32Ptr(50), RangeEnd: int32Ptr(60)},
		}
		got := clampPhaseRangesToDomain(phases, promptCount)
		require.Len(t, got, 1, "the fully-out-of-domain phase is dropped")
		assert.Equal(t, int32(0), got[0].PhaseOrdinal)
	})

	t.Run("leaves an in-domain phase and its turn_count untouched", func(t *testing.T) {
		phases := []*PhaseJudgment{{PhaseOrdinal: 0, RangeStart: int32Ptr(2), RangeEnd: int32Ptr(5), TurnCount: int32Ptr(4)}}
		got := clampPhaseRangesToDomain(phases, promptCount)
		require.Len(t, got, 1)
		assert.Equal(t, int32(2), *got[0].RangeStart)
		assert.Equal(t, int32(5), *got[0].RangeEnd)
		assert.Equal(t, int32(4), *got[0].TurnCount)
	})

	t.Run("a phase with no range is passed through", func(t *testing.T) {
		phases := []*PhaseJudgment{{PhaseOrdinal: 0}}
		got := clampPhaseRangesToDomain(phases, promptCount)
		require.Len(t, got, 1)
		assert.Nil(t, got[0].RangeStart)
	})

	t.Run("empty domain leaves phases unchanged", func(t *testing.T) {
		phases := []*PhaseJudgment{{PhaseOrdinal: 0, RangeStart: int32Ptr(0), RangeEnd: int32Ptr(3)}}
		got := clampPhaseRangesToDomain(phases, 0)
		require.Len(t, got, 1)
		assert.Equal(t, int32(3), *got[0].RangeEnd, "not clamped")
	})
}

func TestApplyMeasuredBlobColumns_ActiveShareAndFirstUserMsg(t *testing.T) {
	tests := []struct {
		name             string
		usage            *sessionintelligencepb.UsageMetadata
		wantActiveShare  *float64
		wantFirstUserTs  *time.Time
		firstUserTsIsSet bool
	}{
		{
			name:            "active fraction of the session",
			usage:           &sessionintelligencepb.UsageMetadata{EffectiveActiveDurationS: 20, SessionDurationSeconds: 100},
			wantActiveShare: float64Ptr(0.2),
		},
		{
			name:            "zero session duration leaves active_share nil (no divide-by-zero)",
			usage:           &sessionintelligencepb.UsageMetadata{EffectiveActiveDurationS: 20, SessionDurationSeconds: 0},
			wantActiveShare: nil,
		},
		{
			name:            "active exceeding session duration clamps to 1",
			usage:           &sessionintelligencepb.UsageMetadata{EffectiveActiveDurationS: 150, SessionDurationSeconds: 100},
			wantActiveShare: float64Ptr(1),
		},
		{
			name:             "first_user_msg_ts parsed from the first timestamp",
			usage:            &sessionintelligencepb.UsageMetadata{UserMessageTimestamps: []string{"2026-08-17T07:50:57Z", "2026-08-17T08:00:00Z"}},
			firstUserTsIsSet: true,
			wantFirstUserTs:  timePtr(time.Date(2026, 8, 17, 7, 50, 57, 0, time.UTC)),
		},
		{
			name:  "malformed first timestamp leaves first_user_msg_ts nil",
			usage: &sessionintelligencepb.UsageMetadata{UserMessageTimestamps: []string{"not-a-timestamp"}},
		},
		{
			name:  "no timestamps leaves first_user_msg_ts nil",
			usage: &sessionintelligencepb.UsageMetadata{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := &model.SessionAnalysis{}
			ApplyMeasuredBlobColumns(analysis, nil, tt.usage)

			if tt.wantActiveShare == nil {
				assert.Nil(t, analysis.ActiveShare)
			} else {
				require.NotNil(t, analysis.ActiveShare)
				assert.InDelta(t, *tt.wantActiveShare, *analysis.ActiveShare, 1e-9)
			}

			if tt.firstUserTsIsSet {
				require.NotNil(t, analysis.FirstUserMsgTs)
				assert.True(t, tt.wantFirstUserTs.Equal(*analysis.FirstUserMsgTs), "want %v got %v", tt.wantFirstUserTs, *analysis.FirstUserMsgTs)
			} else {
				assert.Nil(t, analysis.FirstUserMsgTs)
			}
		})
	}
}

// TestApplyMeasuredColumns_LastActiveAtFromFetchedAt pins last_active_at = the raw record's
// fetched_at (carried on sp.lastActiveAt) — the session last_conversation_at equivalent — and its
// zero-value guard (a never-fetched record stays NULL rather than storing a 0001-01-01 sentinel).
func TestApplyMeasuredColumns_LastActiveAtFromFetchedAt(t *testing.T) {
	fetched := time.Date(2026, 8, 19, 11, 40, 35, 0, time.UTC)
	tests := []struct {
		name     string
		fetched  time.Time
		wantSet  bool
		wantWhen time.Time
	}{
		{name: "fetched_at populates last_active_at", fetched: fetched, wantSet: true, wantWhen: fetched},
		{name: "zero fetched_at leaves last_active_at nil", fetched: time.Time{}, wantSet: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := &model.SessionAnalysis{}
			sp := &spineResult{lastActiveAt: tt.fetched, usage: &sessionintelligencepb.UsageMetadata{}, metrics: &sessionintelligencepb.ConversationMetrics{}}
			applyMeasuredColumns(analysis, sp)

			if tt.wantSet {
				require.NotNil(t, analysis.LastActiveAt)
				assert.True(t, tt.wantWhen.Equal(*analysis.LastActiveAt), "want %v got %v", tt.wantWhen, *analysis.LastActiveAt)
			} else {
				assert.Nil(t, analysis.LastActiveAt)
			}
		})
	}
}

// TestHitlGateFromApprovalMode pins the approval/permission-mode → HitlGate map across both IDE
// vocabularies: every Claude Code permissionMode, every Codex approval_policy (legacy and modern),
// and the empty/unknown fall-through to nil (column stays NULL rather than a wrong posture).
func TestHitlGateFromApprovalMode(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want *sessionintelligencepb.HitlGate
	}{
		// Claude Code permissionMode.
		{name: "claude default", mode: "default", want: sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT.Enum()},
		{name: "claude plan", mode: "plan", want: sessionintelligencepb.HitlGate_HITL_GATE_PLAN.Enum()},
		{name: "claude acceptEdits camelCase", mode: "acceptEdits", want: sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()},
		{name: "claude accept_edits snake", mode: "accept_edits", want: sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()},
		{name: "claude auto", mode: "auto", want: sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()},
		{name: "claude bypassPermissions camelCase", mode: "bypassPermissions", want: sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()},
		{name: "claude bypass_permissions snake", mode: "bypass_permissions", want: sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()},
		// Codex approval_policy — modern vocabulary.
		{name: "codex never", mode: "never", want: sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()},
		{name: "codex on-request", mode: "on-request", want: sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT.Enum()},
		{name: "codex on-failure", mode: "on-failure", want: sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()},
		{name: "codex untrusted", mode: "untrusted", want: sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT.Enum()},
		// Codex approval_policy — legacy vocabulary.
		{name: "codex suggest", mode: "suggest", want: sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT.Enum()},
		{name: "codex auto-edit", mode: "auto-edit", want: sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()},
		{name: "codex full-auto", mode: "full-auto", want: sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()},
		// Normalization: surrounding whitespace and mixed case still resolve.
		{name: "whitespace and case are normalized", mode: "  BypassPermissions  ", want: sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()},
		// Empty / unknown fall through to nil.
		{name: "empty is nil", mode: "", want: nil},
		{name: "whitespace-only is nil", mode: "   ", want: nil},
		{name: "unknown token is nil", mode: "yolo-mode", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hitlGateFromApprovalMode(tt.mode)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

// userPromptWithMode builds a ROLE_USER prompt carrying the given raw permission_mode ("" = none).
// (assistantPrompt lives in turnevents_test.go, shared across the package's tests.)
func userPromptWithMode(mode string) *sessionintelligencepb.Prompt {
	return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{PermissionMode: mode}}
}

// TestPerTurnHitlGates pins the spine's per-turn forward-fill: one entry per user prompt, keyed by
// prompt_index; a sparsely-stamped mode carries forward from the last stamped turn; a mid-session
// change lands on and after its turn; an all-empty session yields all-nil (column stays NULL).
func TestPerTurnHitlGates(t *testing.T) {
	gate := func(g sessionintelligencepb.HitlGate) *sessionintelligencepb.HitlGate { return g.Enum() }
	tests := []struct {
		name    string
		prompts []*sessionintelligencepb.Prompt
		want    []*sessionintelligencepb.HitlGate
	}{
		{
			name: "sparse stamp forward-fills to later turns",
			// user(plan), assistant, user(no mode), user(no mode) → all three user turns are plan.
			prompts: []*sessionintelligencepb.Prompt{
				userPromptWithMode("plan"), assistantPrompt(),
				userPromptWithMode(""),
				userPromptWithMode(""),
			},
			want: []*sessionintelligencepb.HitlGate{
				gate(sessionintelligencepb.HitlGate_HITL_GATE_PLAN),
				gate(sessionintelligencepb.HitlGate_HITL_GATE_PLAN),
				gate(sessionintelligencepb.HitlGate_HITL_GATE_PLAN),
			},
		},
		{
			name: "mid-session change lands on and after its turn",
			// turns before the first stamp are nil (unknown); the change to bypassPermissions carries on.
			prompts: []*sessionintelligencepb.Prompt{
				userPromptWithMode(""),
				userPromptWithMode("default"),
				userPromptWithMode(""),
				userPromptWithMode("bypassPermissions"),
				userPromptWithMode(""),
			},
			want: []*sessionintelligencepb.HitlGate{
				nil,
				gate(sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT),
				gate(sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT),
				gate(sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS),
				gate(sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS),
			},
		},
		{
			name: "no per-turn mode anywhere yields all nil",
			prompts: []*sessionintelligencepb.Prompt{
				userPromptWithMode(""), assistantPrompt(), userPromptWithMode(""),
			},
			want: []*sessionintelligencepb.HitlGate{nil, nil},
		},
		{
			name:    "no user prompts yields empty slice",
			prompts: []*sessionintelligencepb.Prompt{assistantPrompt()},
			want:    []*sessionintelligencepb.HitlGate{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := perTurnHitlGates(tt.prompts)
			require.Len(t, got, len(tt.want))
			for i := range tt.want {
				if tt.want[i] == nil {
					assert.Nil(t, got[i], "index %d", i)
					continue
				}
				require.NotNil(t, got[i], "index %d", i)
				assert.Equal(t, *tt.want[i], *got[i], "index %d", i)
			}
		})
	}
}

// TestPhaseHitlGate pins that a phase reads the gate at its RangeStart, and that a missing/out-of-
// bounds start stores NULL rather than a wrong gate.
func TestPhaseHitlGate(t *testing.T) {
	plan := sessionintelligencepb.HitlGate_HITL_GATE_PLAN.Enum()
	bypass := sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()
	sp := &spineResult{perTurnHitlGate: []*sessionintelligencepb.HitlGate{plan, plan, nil, bypass}} // indices [0,3]
	tests := []struct {
		name  string
		start *int32
		want  *sessionintelligencepb.HitlGate
	}{
		{name: "reads the gate at RangeStart", start: int32Ptr(0), want: plan},
		{name: "reads a later phase's start gate", start: int32Ptr(3), want: bypass},
		{name: "a nil per-turn entry at start is nil", start: int32Ptr(2), want: nil},
		{name: "nil RangeStart is nil", start: nil, want: nil},
		{name: "start past the table is nil, not a wrong gate", start: int32Ptr(99), want: nil},
		{name: "negative start is nil", start: int32Ptr(-1), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := phaseHitlGate(sp, &PhaseJudgment{RangeStart: tt.start})
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

// TestBuildPhaseRows_HitlGatePerPhase is the regression guard for: with the gate changing
// BETWEEN phases, each phase row must carry the gate active at its own RangeStart — not the
// session's last posture (the bug this replaces stamped every phase with one session-level mode).
func TestBuildPhaseRows_HitlGatePerPhase(t *testing.T) {
	// A session whose mode changes at turn 2: turns 0-1 default, turns 2-3 bypassPermissions.
	prompts := []*sessionintelligencepb.Prompt{
		userPromptWithMode("default"),
		userPromptWithMode(""),
		userPromptWithMode("bypassPermissions"),
		userPromptWithMode(""),
	}
	sp := &spineResult{perTurnHitlGate: perTurnHitlGates(prompts)}
	phases := []*PhaseJudgment{
		{PhaseOrdinal: 0, RangeStart: int32Ptr(0), RangeEnd: int32Ptr(1)},
		{PhaseOrdinal: 1, RangeStart: int32Ptr(2), RangeEnd: int32Ptr(3)},
	}
	rows := buildPhaseRows("org1", "sa1", sp, phases, nil)
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].HitlGate)
	assert.Equal(t, sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT, *rows[0].HitlGate, "phase 0 carries its own gate")
	require.NotNil(t, rows[1].HitlGate)
	assert.Equal(t, sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS, *rows[1].HitlGate,
		"phase 1 carries the gate active at ITS start, not the session's first or last posture")
}

// TestBuildPhaseRows_ToolCallCountPerPhase is the regression guard for: each phase row must
// carry the sum of the measured per-turn tool_call_count over its own prompt-index range, not a
// session-level total or NULL.
func TestBuildPhaseRows_ToolCallCountPerPhase(t *testing.T) {
	// Four turns: phase [0,1] sums 2+3=5, phase [2,3] sums 5+0=5.
	measuredTurns := []*model.SessionTurnAnalysis{
		{PromptIndex: 0, ToolCallCount: int32Ptr(2)},
		{PromptIndex: 1, ToolCallCount: int32Ptr(3)},
		{PromptIndex: 2, ToolCallCount: int32Ptr(5)},
		{PromptIndex: 3, ToolCallCount: int32Ptr(0)},
	}
	phases := []*PhaseJudgment{
		{PhaseOrdinal: 0, RangeStart: int32Ptr(0), RangeEnd: int32Ptr(1)},
		{PhaseOrdinal: 1, RangeStart: int32Ptr(2), RangeEnd: int32Ptr(3)},
	}
	rows := buildPhaseRows("org1", "sa1", &spineResult{}, phases, measuredTurns)
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].ToolCallCount)
	assert.Equal(t, int32(5), *rows[0].ToolCallCount, "phase 0 sums its own turns")
	require.NotNil(t, rows[1].ToolCallCount)
	assert.Equal(t, int32(5), *rows[1].ToolCallCount, "phase 1 sums its own turns, not the session total")
}

// TestPhaseToolCalls pins that a phase sums the measured per-turn tool_call_count over its inclusive
// range, that a malformed range stores NULL, and that a turn with a nil count contributes 0.
func TestPhaseToolCalls(t *testing.T) {
	measuredTurns := []*model.SessionTurnAnalysis{
		{PromptIndex: 0, ToolCallCount: int32Ptr(2)},
		{PromptIndex: 1, ToolCallCount: nil}, // no session_events timeline → contributes 0
		{PromptIndex: 2, ToolCallCount: int32Ptr(7)},
	}
	tests := []struct {
		name  string
		start *int32
		end   *int32
		want  *int32 // nil ⟹ expect NULL
	}{
		{name: "sums the in-range counts", start: int32Ptr(0), end: int32Ptr(2), want: int32Ptr(9)},
		{name: "a nil per-turn count contributes 0", start: int32Ptr(0), end: int32Ptr(1), want: int32Ptr(2)},
		{name: "out-of-range turns are excluded", start: int32Ptr(2), end: int32Ptr(2), want: int32Ptr(7)},
		{name: "nil RangeStart stores NULL", start: nil, end: int32Ptr(2), want: nil},
		{name: "nil RangeEnd stores NULL", start: int32Ptr(0), end: nil, want: nil},
		{name: "inverted range stores NULL", start: int32Ptr(2), end: int32Ptr(0), want: nil},
		{name: "negative RangeStart stores NULL", start: int32Ptr(-1), end: int32Ptr(2), want: nil},
		// A range entirely past the turn domain intersects no measured turn — NULL (unknown),
		// not a real "zero tool calls", mirroring phaseTokens' out-of-domain guard.
		{name: "range entirely past the domain stores NULL", start: int32Ptr(100), end: int32Ptr(200), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := phaseToolCalls(measuredTurns, &PhaseJudgment{RangeStart: tt.start, RangeEnd: tt.end})
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}

	// A valid range over an empty turn set intersects nothing → NULL, not a misleading 0,
	// matching the session-grain rollup's nil-when-no-measured-turns guard.
	t.Run("valid range over empty measuredTurns stores NULL", func(t *testing.T) {
		assert.Nil(t, phaseToolCalls(nil, &PhaseJudgment{RangeStart: int32Ptr(0), RangeEnd: int32Ptr(2)}))
	})
}

func timePtr(t time.Time) *time.Time { return &t }
