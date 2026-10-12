// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for SessionRoundTripFaultRepo. session_round_trip_faults is E's
// per-pair fault table (data-window-only: no person_id — seat scoping composes through
// the parent session_analyses join, like session_turn_analyses/session_phase_analyses).
// The tests exercise the replace-set write (a fresh generation soft-deletes the prior
// one rather than appending to it), the prompt-index ordering of ListBySessionAnalysisID,
// and cross-tenant exclusion. This table has no DeleteByOrgID — ISessionRoundTripFaultDAO
// exposes only ReplaceSet and ListBySessionAnalysisID.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// newRoundTripFault builds one fault row at the given prompt index (P's index — the
// earlier prompt of the pair the fault lands on).
func newRoundTripFault(orgID, sessionAnalysisID, rawRecordID string, promptIndex int32, fault sessionintelligencepb.RoundTripFault) *model.SessionRoundTripFault {
	f := fault
	return &model.SessionRoundTripFault{
		OrganizationID:        orgID,
		SessionAnalysisID:     sessionAnalysisID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		PromptIndex:           promptIndex,
		Fault:                 &f,
	}
}

// TestSessionRoundTripFaultRepo_ReplaceSet_ReplacesPriorGeneration is the replace-set
// guardrail (by design, part of the ownership fix): a second
// ReplaceSet call REPLACES the live fault set — the prior generation's rows are
// soft-deleted, never left live alongside the new set (an E re-run must not accumulate
// duplicate faults for a session that keeps getting re-judged).
func TestSessionRoundTripFaultRepo_ReplaceSet_ReplacesPriorGeneration(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionRoundTripFaultRepo(tx)

	setA := []*model.SessionRoundTripFault{
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 0, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT),
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 1, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_PROMPT),
	}
	require.NoError(t, repo.ReplaceSet(ctx, orgID, sessionAnalysisID, setA))

	live, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, live, 2)

	// A second generation carries only prompt_index 5 — a re-judge that resolved
	// fewer round trips this time.
	setB := []*model.SessionRoundTripFault{
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 5, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_MIXED),
	}
	require.NoError(t, repo.ReplaceSet(ctx, orgID, sessionAnalysisID, setB))

	live, err = repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, live, 1, "a re-run REPLACES the fault set rather than appending to it")
	assert.Equal(t, int32(5), live[0].PromptIndex)
	require.NotNil(t, live[0].Fault)
	assert.Equal(t, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_MIXED, *live[0].Fault)

	// Set A's two rows are soft-deleted, not hard-deleted and not left live.
	var deletedCount int
	require.NoError(t, tx.Raw(
		"SELECT COUNT(*) FROM session_round_trip_faults WHERE session_analysis_id = ?::uuid AND deleted_at IS NOT NULL",
		sessionAnalysisID,
	).Row().Scan(&deletedCount))
	assert.Equal(t, 2, deletedCount, "the replaced generation's rows are soft-deleted, not dropped")
}

// TestSessionRoundTripFaultRepo_ReplaceSet_EmptySetClearsFaults asserts that an E rerun
// which found no round-trip faults clears the prior generation (an empty set is a real
// result, not a no-op) rather than leaving the stale set serving.
func TestSessionRoundTripFaultRepo_ReplaceSet_EmptySetClearsFaults(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionRoundTripFaultRepo(tx)

	require.NoError(t, repo.ReplaceSet(ctx, orgID, sessionAnalysisID, []*model.SessionRoundTripFault{
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 0, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT),
	}))
	live, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, live, 1)

	require.NoError(t, repo.ReplaceSet(ctx, orgID, sessionAnalysisID, nil))
	live, err = repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	assert.Empty(t, live, "an empty set clears the session's faults")
}

// TestSessionRoundTripFaultRepo_ListBySessionAnalysisID_OrderedByPromptIndex inserts out
// of prompt-index order and asserts ORDER BY, not insertion order, decides the result.
func TestSessionRoundTripFaultRepo_ListBySessionAnalysisID_OrderedByPromptIndex(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionRoundTripFaultRepo(tx)

	require.NoError(t, repo.ReplaceSet(ctx, orgID, sessionAnalysisID, []*model.SessionRoundTripFault{
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 2, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_TASTE),
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 0, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT),
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 1, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_PROMPT),
	}))

	live, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, live, 3)
	assert.Equal(t, int32(0), live[0].PromptIndex)
	assert.Equal(t, int32(1), live[1].PromptIndex)
	assert.Equal(t, int32(2), live[2].PromptIndex, "faults must be ordered by prompt_index ASC")

	empty, err := repo.ListBySessionAnalysisID(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestSessionRoundTripFaultRepo_NoRoundTripVerdictIsRecorded asserts a pair E resolved
// as no_round_trip still lands a row carrying that verdict — it is a real judgment
// ("this pair was not a round-trip"), not an abstention the writer skips.
func TestSessionRoundTripFaultRepo_NoRoundTripVerdictIsRecorded(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionRoundTripFaultRepo(tx)

	require.NoError(t, repo.ReplaceSet(ctx, orgID, sessionAnalysisID, []*model.SessionRoundTripFault{
		newRoundTripFault(orgID, sessionAnalysisID, rawRecordID, 0, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_NO_ROUND_TRIP),
	}))

	live, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.NotNil(t, live[0].Fault)
	assert.Equal(t, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_NO_ROUND_TRIP, *live[0].Fault)
}

// TestSessionRoundTripFaultRepo_ListBySessionAnalysisID_CrossTenantExclusion asserts a
// distinct org's faults (under a distinct parent id) are not returned for another org's
// session_analysis_id.
func TestSessionRoundTripFaultRepo_ListBySessionAnalysisID_CrossTenantExclusion(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgA := seedOrg(t, tx)
	orgB := seedOrg(t, tx)
	parentA := seedParentAnalysis(t, tx, orgA)
	parentB := seedParentAnalysis(t, tx, orgB)
	ctx := context.Background()
	repo := dao.NewSessionRoundTripFaultRepo(tx)

	require.NoError(t, repo.ReplaceSet(ctx, orgA, parentA, []*model.SessionRoundTripFault{
		newRoundTripFault(orgA, parentA, uuid.NewString(), 0, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT),
	}))
	require.NoError(t, repo.ReplaceSet(ctx, orgB, parentB, []*model.SessionRoundTripFault{
		newRoundTripFault(orgB, parentB, uuid.NewString(), 0, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT),
	}))

	got, err := repo.ListBySessionAnalysisID(ctx, parentA)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, orgA, got[0].OrganizationID, "org B's faults resolve only by org B's parent id")
}
