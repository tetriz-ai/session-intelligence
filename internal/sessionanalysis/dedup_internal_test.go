// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// TestDedupCandidatesKeepLive is the cross-lane dedup guard: a JUDGED row can match a
// LIVE leg (content-changed) AND a BACKFILL leg (version-behind) in one tick, and the
// DISTINCT-ON dedup runs only within a lane — so a raw record present in both lanes
// must survive exactly once, as its LIVE entry (BACKFILL yields to LIVE). Processing
// it twice would Reclaim the just-committed generation and re-run it spine-only,
// wiping the freshly-written security flags.
func TestDedupCandidatesKeepLive(t *testing.T) {
	both := "rr-both"
	live := []*dao.ActionableCandidate{
		{RawRecordID: "rr-live-only", Category: dao.CategoryNoState},
		{RawRecordID: both, Category: dao.CategoryContentChanged},
	}
	backfill := []*dao.ActionableCandidate{
		{RawRecordID: both, Category: dao.CategoryVersionBehind},
		{RawRecordID: "rr-backfill-only", Category: dao.CategoryProducerEnabled},
	}

	got := dedupCandidatesKeepLive(live, backfill)

	require.Len(t, got, 3, "the both-lane row is not double-counted")
	byID := map[string]dao.ActionableCategory{}
	for _, candidate := range got {
		byID[candidate.RawRecordID] = candidate.Category
	}
	assert.Equal(t, dao.CategoryContentChanged, byID[both], "the both-lane row survives as its LIVE (content-changed) entry, not BACKFILL")
	assert.Equal(t, dao.CategoryNoState, byID["rr-live-only"])
	assert.Equal(t, dao.CategoryProducerEnabled, byID["rr-backfill-only"])
	// LIVE entries precede BACKFILL-only entries, preserving the lane ordering.
	assert.Equal(t, both, got[1].RawRecordID)
	assert.Equal(t, "rr-backfill-only", got[2].RawRecordID)
}
