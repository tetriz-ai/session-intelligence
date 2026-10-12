// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// discardFixture builds a small session whose D-shaped request exercises every fence block: a
// judged goal, one themed phase with an intent, a single human turn carrying a file path (so the
// send-transform can be checked), and a branch/revert ledger with two signal kinds.
func discardFixture() (*spineResult, *ReadResult) {
	sp := &spineResult{
		prompts: []*sessionintelligencepb.Prompt{
			{Role: sessionintelligencepb.Role_ROLE_USER, Content: "please edit src/auth/login.go", Metadata: &sessionintelligencepb.PromptMetadata{}},
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "done editing src/auth/login.go", Metadata: &sessionintelligencepb.PromptMetadata{
				MessageId: "m1", InputTokens: 10, OutputTokens: 5,
				TokenFidelity: sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED,
			}},
		},
		usage: &sessionintelligencepb.UsageMetadata{},
		ledgers: spineLedgers{
			revertSignals: 2,
			discardLedger: &discardLedgerDetail{
				revertVerbs:       []string{"git reset"},
				cancelledOrFailed: 1,
			},
		},
	}
	goal := "ship the login refactor"
	theme := sessionintelligencepb.PhaseTheme_PHASE_THEME_CREATION
	objective := "build the login flow"
	corrected := sessionintelligencepb.DownstreamImpact_DOWNSTREAM_IMPACT_CORRECTED
	read := &ReadResult{
		Verdict: SessionVerdict{UnderlyingGoal: &goal},
		Phases:  []*PhaseJudgment{{PhaseOrdinal: 0, PhaseTheme: &theme, PhaseObjective: &objective}},
		Turns:   []*TurnJudgment{{PromptIndex: 0, DownstreamImpact: &corrected}},
	}
	return sp, read
}

// TestBuildDiscardContent_AssemblesFenceBlocks pins that D receives a compact, D-shaped request
// (the ANALYZE_DISCARD_FENCE vocabulary) rather than R's raw transcript: the GOAL, the PHASES with
// theme + intent, the turn body decorated with the per-turn downstream_impact tag the fence reads,
// the measured BRANCH LEDGER, and CAPABILITIES — and that it is NOT the raw sp.llmContent (the
// passthrough gap this slice closes) and is anonymized the same way sp.llmContent is.
func TestBuildDiscardContent_AssemblesFenceBlocks(t *testing.T) {
	sp, read := discardFixture()
	// buildSpineContent runs first in the spine (before any producer), populating sp.turnBlocks that
	// buildDiscardContent now reuses rather than re-parsing the conversation.
	rawContent, _ := buildSpineContent("org-1", "rr-1", sp)
	sp.llmContent = anonymizePaths(rawContent)

	content := buildDiscardContent(sp, read)

	tests := []struct {
		name string
		want string
	}{
		{name: "goal marker", want: "=== GOAL ==="},
		{name: "goal text", want: "ship the login refactor"},
		{name: "phases marker", want: "=== PHASES ==="},
		{name: "phase theme", want: "phase_theme=creation"},
		{name: "phase intent", want: "intent=build the login flow"},
		{name: "turn body", want: "=== TURN 0 (author=SESSION_EVENT_AUTHOR_HUMAN) ==="},
		{name: "downstream_impact tag", want: "downstream_impact: corrected"},
		{name: "branch ledger marker", want: "=== BRANCH LEDGER ==="},
		{name: "revert operation evidence renders the matched git verb, not the raw op name", want: "revert_operations: git reset"},
		{name: "capabilities marker", want: "=== CAPABILITIES ==="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, content, tt.want)
		})
	}

	// The regression guard against the raw-passthrough gap: D must NOT be handed R's transcript.
	assert.NotEqual(t, sp.llmContent, content, "D content must differ from R's raw transcript (sp.llmContent)")

	// The same send-transform sp.llmContent gets: a raw file path in a turn never reaches the model.
	assert.NotContains(t, content, "login.go", "file paths must be anonymized before reaching the model")
	assert.NotContains(t, content, "src/auth", "file paths must be anonymized before reaching the model")
}

// TestBuildDiscardContent_UnjudgedTurnRendersWithoutTag guards the phantom/unjudged-turn path: a
// turn with no judged downstream_impact renders its body with no tag rather than crashing or
// emitting a spurious one.
func TestBuildDiscardContent_UnjudgedTurnRendersWithoutTag(t *testing.T) {
	sp, read := discardFixture()
	read.Turns = nil // no per-turn judgments at all
	// The spine cuts the turn blocks once before any producer runs; buildDiscardContent reuses them.
	buildSpineContent("org-1", "rr-1", sp)

	content := buildDiscardContent(sp, read)

	assert.Contains(t, content, "=== TURN 0 (author=SESSION_EVENT_AUTHOR_HUMAN) ===", "the body still renders")
	assert.NotContains(t, content, "downstream_impact:", "an unjudged turn carries no downstream_impact line")
}

// TestRenderBranchLedger_SurfacesIndividualSignals pins that the ledger renders each measured
// signal kind with its own evidence — the op names, the deleted-file count, the interruptions, the
// departed branches — not just an aggregate tally, so D can weigh each hint.
func TestRenderBranchLedger_SurfacesIndividualSignals(t *testing.T) {
	tests := []struct {
		name   string
		detail *discardLedgerDetail
		wants  []string
		absent []string
	}{
		{
			name: "every signal kind surfaces individually — verbs and the branch count, never identifiers",
			detail: &discardLedgerDetail{
				revertVerbs:       []string{"git reset", "git checkout"},
				cancelledOrFailed: 2,
				interruptions:     3,
				deletedFiles:      4,
				abandonedOutcome:  true,
				branchCount:       2,
			},
			wants: []string{
				"=== BRANCH LEDGER ===",
				"revert_operations: git reset, git checkout",
				"cancelled_or_failed_operations: 2",
				"user_interruptions: 3",
				"deleted_files: 4",
				"session_outcome: abandoned",
				"departed_not_returned_branches: 2",
			},
		},
		{
			name:   "single branch is not a departed-not-returned signal",
			detail: &discardLedgerDetail{branchCount: 1, interruptions: 1},
			wants:  []string{"user_interruptions: 1"},
			absent: []string{"departed_not_returned"},
		},
		{
			name:   "no signals renders an explicit no-signal line, never a bare header",
			detail: &discardLedgerDetail{},
			wants:  []string{"=== BRANCH LEDGER ===", "no measured revert signals"},
		},
		{
			name:   "nil ledger does not crash",
			detail: nil,
			wants:  []string{"=== BRANCH LEDGER ===", "no measured revert signals"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := renderBranchLedger(tt.detail)
			for _, want := range tt.wants {
				assert.Contains(t, block, want)
			}
			for _, absent := range tt.absent {
				assert.NotContains(t, block, absent)
			}
			// Every rendered fact is tagged authoritative (a measured signal, never a judged one).
			for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
				if line == "" || strings.HasPrefix(line, "===") || line == "no measured revert signals" {
					continue
				}
				assert.Contains(t, line, measuredTag, "each measured ledger line carries the authoritative tag")
			}
		})
	}
}

// TestRenderBranchLedger_RedactsIdentifiers is the send-boundary regression guard: raw git branch
// names and revert operation args are extensionless, so anonymizePaths would NOT hash them — the
// ledger must never retain them in the first place. It renders a full build (buildDiscardLedgerDetail
// → renderBranchLedger) and asserts only the bounded verb and the branch count reach the block, never
// the branch name or the operation's path arg.
func TestRenderBranchLedger_RedactsIdentifiers(t *testing.T) {
	tests := []struct {
		name   string
		conv   *sessionintelligencepb.Conversation
		wants  []string
		absent []string
	}{
		{
			name: "branch names and op args are never retained; only the verb and the count are",
			conv: &sessionintelligencepb.Conversation{
				SessionEvents: []*sessionintelligencepb.SessionEvent{
					{OperationName: "git checkout feature/customer-secret"},
				},
				SessionMeta: &sessionintelligencepb.SessionMeta{GitBranches: []string{"jsmith/private-branch", "main"}},
			},
			wants: []string{
				"revert_operations: git checkout",
				"departed_not_returned_branches: 2",
			},
			absent: []string{"jsmith/private-branch", "feature/customer-secret"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := renderBranchLedger(buildDiscardLedgerDetail(tt.conv))
			for _, want := range tt.wants {
				assert.Contains(t, block, want)
			}
			for _, absent := range tt.absent {
				assert.NotContains(t, block, absent, "raw identifiers must never reach the send boundary")
			}
		})
	}
}

// TestDiscardLedgerSignalCount_ReproducesRevertTally pins signalCount's exact tally over the
// retained signals, so a future edit that changes the count is caught and D's fire gate
// (revertSignals > 0) stays unchanged.
func TestDiscardLedgerSignalCount_ReproducesRevertTally(t *testing.T) {
	tests := []struct {
		name   string
		detail *discardLedgerDetail
		want   int
	}{
		{name: "nil is zero", detail: nil, want: 0},
		{name: "empty is zero", detail: &discardLedgerDetail{}, want: 0},
		{
			name: "cancelled + revert-op + interruptions",
			detail: &discardLedgerDetail{
				revertVerbs:       []string{"git revert"},
				cancelledOrFailed: 1,
				interruptions:     1,
			},
			want: 3,
		},
		{
			name: "meta aggregates: interruptions + abandoned + deleted + multi-branch",
			detail: &discardLedgerDetail{
				interruptions:    2,
				abandonedOutcome: true,
				deletedFiles:     3,
				branchCount:      2,
			},
			want: 5,
		},
		{
			name:   "single branch and zero deleted files do not count",
			detail: &discardLedgerDetail{branchCount: 1, deletedFiles: 0},
			want:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.detail.signalCount())
		})
	}
}
