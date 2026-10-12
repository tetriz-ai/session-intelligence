// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

func discardRange(start, end int32) *sessionintelligencepb.DiscardedRange {
	return &sessionintelligencepb.DiscardedRange{RangeStart: start, RangeEnd: end}
}

// TestSumDiscardedTokens covers the waste-ledger sum: the measured per-turn token cost
// summed over the union of D's inclusive VALID ranges, overlaps deduped, a range whose bound
// falls outside the turn table REJECTED (dropped, not clamped), and each valid range's own
// token_sum stamped in passing. It also asserts the count of ranges returned as valid.
func TestSumDiscardedTokens(t *testing.T) {
	perTurn := []int64{10, 20, 30, 40, 50}

	tests := []struct {
		name       string
		ranges     []*sessionintelligencepb.DiscardedRange
		wantTotal  int64
		wantPerSum []int64 // token_sum stamped on each VALID range, in order; len = valid count
	}{
		{
			name:       "single inclusive range",
			ranges:     []*sessionintelligencepb.DiscardedRange{discardRange(1, 2)},
			wantTotal:  50, // 20 + 30
			wantPerSum: []int64{50},
		},
		{
			name:       "overlapping ranges deduped in the total",
			ranges:     []*sessionintelligencepb.DiscardedRange{discardRange(1, 3), discardRange(2, 4)},
			wantTotal:  140,              // turns 1..4: 20+30+40+50, turn 2/3 counted once
			wantPerSum: []int64{90, 120}, // each range still reports its own inclusive sum
		},
		{
			name:       "out-of-range range rejected (not clamped)",
			ranges:     []*sessionintelligencepb.DiscardedRange{discardRange(3, 99)},
			wantTotal:  0,   // end 99 > n-1 → the whole range is dropped, contributing nothing
			wantPerSum: nil, // no valid range, so no token_sum stamped
		},
		{
			name:       "negative start range rejected (not clamped to zero)",
			ranges:     []*sessionintelligencepb.DiscardedRange{discardRange(-1, 2)},
			wantTotal:  0, // start -1 < 0 → dropped rather than clamped to turn 0
			wantPerSum: nil,
		},
		{
			name:       "inverted range rejected (start > end, not accepted as a zero-contribution range)",
			ranges:     []*sessionintelligencepb.DiscardedRange{discardRange(3, 1)},
			wantTotal:  0, // start 3 > end 1 → the whole range is dropped, contributing nothing
			wantPerSum: nil,
		},
		{
			name:       "one valid range kept, one out-of-range range dropped",
			ranges:     []*sessionintelligencepb.DiscardedRange{discardRange(0, 1), discardRange(2, 99)},
			wantTotal:  30,          // only the valid range (turns 0+1) contributes
			wantPerSum: []int64{30}, // the invalid range is dropped before token_sum is stamped
		},
		{
			name:       "no ranges",
			ranges:     nil,
			wantTotal:  0,
			wantPerSum: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validRanges, got := sumDiscardedTokens(tt.ranges, perTurn)
			assert.Equal(t, tt.wantTotal, got)
			assert.Len(t, validRanges, len(tt.wantPerSum))
			for i, want := range tt.wantPerSum {
				assert.Equal(t, want, validRanges[i].GetTokenSum(), "range %d token_sum", i)
			}
		})
	}
}

// TestApplyWasteLedger covers the commit-time ledger: wasted_tokens is set from the ranges
// and capped at total_tokens. R's token_efficiency is NOT rewritten — D never downgrades R
// .
func TestApplyWasteLedger(t *testing.T) {
	efficient := sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_EFFICIENT
	unknown := sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_UNKNOWN

	t.Run("sets wasted_tokens and leaves token_efficiency untouched", func(t *testing.T) {
		total := int64(1000)
		analysis := &model.SessionAnalysis{TotalTokens: &total, TokenEfficiency: &efficient}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}}
		discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{
			Items: []*sessionintelligencepb.DiscardedRange{discardRange(1, 2)}, // 200 + 300
		}}
		applyWasteLedger(analysis, sp, discard)
		require.NotNil(t, analysis.WastedTokens)
		assert.Equal(t, int64(500), *analysis.WastedTokens)
		require.NotNil(t, analysis.DiscardedRanges)
		require.NotNil(t, analysis.TokenEfficiency)
		// R is immutable — efficient stays efficient, never downgraded to wasteful.
		assert.Equal(t, sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_EFFICIENT, *analysis.TokenEfficiency)
	})

	t.Run("caps wasted_tokens at total_tokens", func(t *testing.T) {
		total := int64(250)
		analysis := &model.SessionAnalysis{TotalTokens: &total, TokenEfficiency: &unknown}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}}
		discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{
			Items: []*sessionintelligencepb.DiscardedRange{discardRange(0, 2)}, // 600 > 250
		}}
		applyWasteLedger(analysis, sp, discard)
		require.NotNil(t, analysis.WastedTokens)
		assert.Equal(t, int64(250), *analysis.WastedTokens)
		// unknown is not upgraded/downgraded — only efficient downgrades.
		assert.Equal(t, sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_UNKNOWN, *analysis.TokenEfficiency)
	})

	t.Run("no discard leaves the ledger untouched", func(t *testing.T) {
		analysis := &model.SessionAnalysis{TokenEfficiency: &efficient}
		sp := &spineResult{promptTokenCosts: []int64{100}}
		applyWasteLedger(analysis, sp, nil)
		assert.Nil(t, analysis.WastedTokens)
		assert.Nil(t, analysis.DiscardedRanges)
		assert.Equal(t, sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_EFFICIENT, *analysis.TokenEfficiency)
	})

	t.Run("an out-of-range range is dropped from the stored set, not clamped into it", func(t *testing.T) {
		total := int64(1000)
		analysis := &model.SessionAnalysis{TotalTokens: &total, TokenEfficiency: &efficient}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}} // n=3, valid indices 0..2
		discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{
			Items: []*sessionintelligencepb.DiscardedRange{discardRange(0, 1), discardRange(2, 99)}, // second range's end is out of the turn table
		}}
		applyWasteLedger(analysis, sp, discard)
		require.NotNil(t, analysis.WastedTokens)
		assert.Equal(t, int64(300), *analysis.WastedTokens, "only the valid range's turns (100+200) are counted")
		require.NotNil(t, analysis.DiscardedRanges)
		require.Len(t, analysis.DiscardedRanges.GetItems(), 1, "the out-of-range range is dropped, not stored as a clamped range")
		assert.Equal(t, int32(0), analysis.DiscardedRanges.GetItems()[0].GetRangeStart())
		assert.Equal(t, int32(1), analysis.DiscardedRanges.GetItems()[0].GetRangeEnd())
	})

	t.Run("an inverted range is rejected — neither stored nor counted", func(t *testing.T) {
		analysis := &model.SessionAnalysis{TokenEfficiency: &efficient}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}}
		discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{
			Items: []*sessionintelligencepb.DiscardedRange{discardRange(2, 0)}, // start > end
		}}
		applyWasteLedger(analysis, sp, discard)
		assert.Nil(t, analysis.WastedTokens, "an inverted range must not contribute to the waste total")
		assert.Nil(t, analysis.DiscardedRanges, "an inverted range must not be stored")
	})

	t.Run("every range rejected leaves the ledger unset", func(t *testing.T) {
		analysis := &model.SessionAnalysis{TokenEfficiency: &efficient}
		sp := &spineResult{promptTokenCosts: []int64{100, 200, 300}}
		discard := &DiscardResult{DiscardedRanges: &sessionintelligencepb.DiscardedRangeList{
			Items: []*sessionintelligencepb.DiscardedRange{discardRange(5, 99)}, // wholly out of the turn table
		}}
		applyWasteLedger(analysis, sp, discard)
		assert.Nil(t, analysis.WastedTokens, "a spine-only reprocess carries the prior generation's ledger unchanged when D's whole range set is rejected")
		assert.Nil(t, analysis.DiscardedRanges)
	})
}
