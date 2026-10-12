// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestSplitHumanTurns_LeadingNonUserFoldsIntoFirstTurn guards the human-turn grouping that
// the chunked-read stitch re-bases prompt_index against. A non-user prompt before the first
// user turn must fold into that first turn's group, never form a phantom group 0 — a leading
// group would shift every group index by one, and since R and perTurnTokenCosts index by
// user-turn ordinal, the two frames would disagree by one for every window after the first
// (a gapped/misaligned prompt_index set on any session that opens with a system/assistant
// preamble and is large enough to chunk).
func TestSplitHumanTurns_LeadingNonUserFoldsIntoFirstTurn(t *testing.T) {
	user := func() *sessionintelligencepb.Prompt {
		return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_USER}
	}
	asst := func() *sessionintelligencepb.Prompt {
		return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_ASSISTANT}
	}

	tests := []struct {
		name       string
		prompts    []*sessionintelligencepb.Prompt
		wantGroups int
		// wantFirstStartsUser asserts the first group's FIRST prompt when there is a user turn:
		// false means a leading non-user prompt is (correctly) the first element of turn 0.
		wantFirstLen  int
		wantGroupLens []int
	}{
		{
			name:          "no leading preamble — one group per user turn",
			prompts:       []*sessionintelligencepb.Prompt{user(), asst(), user()},
			wantGroups:    2,
			wantGroupLens: []int{2, 1}, // [user,asst] , [user]
		},
		{
			name:          "leading non-user folds into the FIRST user turn (no phantom group 0)",
			prompts:       []*sessionintelligencepb.Prompt{asst(), user(), asst(), user()},
			wantGroups:    2,
			wantGroupLens: []int{3, 1}, // [asst,user,asst] , [user] — NOT [asst],[user,asst],[user]
		},
		{
			name:          "multiple leading non-user prompts all fold into turn 0",
			prompts:       []*sessionintelligencepb.Prompt{asst(), asst(), user(), user()},
			wantGroups:    2,
			wantGroupLens: []int{3, 1}, // [asst,asst,user] , [user]
		},
		{
			name:          "no user prompt at all — kept as one group, not dropped",
			prompts:       []*sessionintelligencepb.Prompt{asst(), asst()},
			wantGroups:    1,
			wantGroupLens: []int{2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := splitHumanTurns(tt.prompts)
			require.Len(t, groups, tt.wantGroups)
			gotLens := make([]int, len(groups))
			for i, g := range groups {
				gotLens[i] = len(g)
			}
			assert.Equal(t, tt.wantGroupLens, gotLens)
			// Every group after the first must OPEN with a user prompt — the invariant the
			// stitch's group-index → user-ordinal re-basing relies on.
			for i := 1; i < len(groups); i++ {
				assert.Equal(t, sessionintelligencepb.Role_ROLE_USER, groups[i][0].GetRole(),
					"group %d must open with the user turn", i)
			}
		})
	}
}

// TestRebaseToRawIndex_FilteredMachineTurnBeforeHuman is the H2 (single-chunk) regression:
// a machine (delegated/subagent) turn before a human turn is dropped from the render, so
// R's rendered TURN 0 is the human turn's RAW index 1, not 0. Before this fix, R's tagged
// prompt_index flowed straight through unmapped, so the human turn's judgment would have
// attached to raw index 0 — the machine turn's measured row — instead of its own.
func TestRebaseToRawIndex_FilteredMachineTurnBeforeHuman(t *testing.T) {
	sp := &spineResult{
		prompts: []*sessionintelligencepb.Prompt{
			{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{IsSubAgent: true}}, // raw index 0 — machine, filtered out of the render
			userPrompt(), // raw index 1 — the only turn rendered, as TURN 0
		},
		usage: &sessionintelligencepb.UsageMetadata{},
	}
	_, indexMap := buildSpineContent("org-1", "rr-1", sp)

	// R sees exactly one rendered turn and tags it at its rendered position, 0.
	read := &ReadResult{Turns: []*TurnJudgment{{PromptIndex: 0}}}
	rebaseToRawIndex(read, indexMap)

	require.Len(t, read.Turns, 1)
	assert.Equal(t, int32(1), read.Turns[0].PromptIndex,
		"the human turn's judgment must attach to its RAW prompt_index (1), not the rendered position (0)")
}

// TestStitchWindowTurns_FilteredGroupInsideMainRange is the H2 (chunked) regression: a
// machine turn filtered out of the middle of a window's main range must not shift the
// window's later turns onto the filtered turn's raw index. Before this fix, stitchWindowTurns
// re-based a rendered position by adding the window's raw mainStart, which — with a filtered
// group inside the range — collided the second rendered turn onto raw index 1 (the filtered
// machine turn) instead of its actual raw index 2.
func TestStitchWindowTurns_FilteredGroupInsideMainRange(t *testing.T) {
	sp := &spineResult{prompts: []*sessionintelligencepb.Prompt{
		userPrompt(), // raw index 0 — human, kept
		{Role: sessionintelligencepb.Role_ROLE_USER, Metadata: &sessionintelligencepb.PromptMetadata{IsSubAgent: true}}, // raw index 1 — machine, filtered
		userPrompt(), // raw index 2 — human, kept
	}}
	groups := splitHumanTurns(sp.prompts)
	require.Len(t, groups, 3)
	rows := buildMeasuredTurnRows("org", "rr", sp)
	window := readWindow{mainStart: 0, mainEnd: 3, lookEnd: 3}
	windowBlocks := buildTurnBlocks(groups, rows, nil, prefixHumanTurnFilter, window.mainStart, window.lookEnd)
	require.Len(t, windowBlocks, 2, "the machine turn at raw index 1 is filtered out of the render")

	indexMap := sequenceToRawIndex(windowBlocks)
	keptMain := keptMainCount(windowBlocks, int32(window.mainEnd))
	require.Equal(t, int32(2), keptMain, "both kept blocks fall inside the main range")

	// R sees two rendered turns (Sequence 0 and 1) and tags both.
	stitched := &ReadResult{}
	stitchWindowTurns(stitched, []*TurnJudgment{{PromptIndex: 0}, {PromptIndex: 1}}, 0, indexMap, keptMain, 0)

	require.Len(t, stitched.Turns, 2, "no turn is missed and none collide")
	gotRaw := []int32{stitched.Turns[0].PromptIndex, stitched.Turns[1].PromptIndex}
	assert.ElementsMatch(t, []int32{0, 2}, gotRaw,
		"the window's rendered positions map to the correct raw indices, skipping the filtered machine turn's index")
}

// TestRebaseDiscardToRawIndex_FilteredSession pins the D-side counterpart to rebaseToRawIndex:
// D tags its discarded ranges by rendered Sequence, so on a machine-turn-filtered session (where
// Sequence != raw Index) the ranges must be rebased to raw Index before the waste ledger reads
// them — else the ledger attributes the wrong turn's tokens. indexMap {0->0, 1->1, 2->3} models a
// filtered machine turn at raw index 2.
func TestRebaseDiscardToRawIndex_FilteredSession(t *testing.T) {
	indexMap := []int32{0, 1, 3} // rendered 2 -> raw 3 (raw 2 was a filtered machine turn)
	discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{Items: []*sessionintelligencepb.DiscardedRange{
		{RangeStart: 2, RangeEnd: 2, EvidencePromptIndex: 2}, // single rendered turn -> raw 3
		{RangeStart: 0, RangeEnd: 2, EvidencePromptIndex: 1}, // span straddling the filtered turn -> raw [0,3]
		{RangeStart: 5, RangeEnd: 5, EvidencePromptIndex: 5}, // out of the render's bounds -> dropped
	}}}

	rebaseDiscardToRawIndex(discard, indexMap)

	got := discard.DiscardedRanges.GetItems()
	require.Len(t, got, 2, "the out-of-bounds range is dropped, not clamped")
	assert.Equal(t, int32(3), got[0].GetRangeStart(), "single rendered turn 2 maps to raw 3")
	assert.Equal(t, int32(3), got[0].GetRangeEnd())
	assert.Equal(t, int32(3), got[0].GetEvidencePromptIndex(), "evidence index rebased too")
	assert.Equal(t, int32(0), got[1].GetRangeStart(), "span start rendered 0 -> raw 0")
	assert.Equal(t, int32(3), got[1].GetRangeEnd(), "span end rendered 2 -> raw 3 (includes the filtered raw turn 2)")
	assert.Equal(t, int32(1), got[1].GetEvidencePromptIndex())
}

// TestRebaseDiscardToRawIndex_UnresolvableEvidence pins the -1 sentinel: a range whose bounds
// resolve but whose evidence_prompt_index does not is kept with evidence set to -1 (a prompt_index
// no measured row has), rather than dropped or left pointing at the wrong turn.
func TestRebaseDiscardToRawIndex_UnresolvableEvidence(t *testing.T) {
	indexMap := []int32{0, 1}
	discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{Items: []*sessionintelligencepb.DiscardedRange{
		{RangeStart: 0, RangeEnd: 1, EvidencePromptIndex: 9}, // bounds ok, evidence out of bounds
	}}}

	rebaseDiscardToRawIndex(discard, indexMap)

	got := discard.DiscardedRanges.GetItems()
	require.Len(t, got, 1)
	assert.Equal(t, int32(-1), got[0].GetEvidencePromptIndex(), "unresolvable evidence index -> -1")
}
