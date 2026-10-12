// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for SessionAnalysisStateRepo. session_analysis_state carries
// tenant_isolation only (not plan-scoped), so it reads and writes through
// tenantdb.DB — the actionable-work scan must see out-of-window rows the content
// tables hide. The flagship case guards GetByRawRecordID's live-generation contract
// (status <> SUPERSEDED): a superseded generation and its live replacement coexist
// for one raw_record_id and only the live one is returned.

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// newSessionAnalysisState builds a state row with the given status and vocabulary
// version, carrying a JSONB proto-message column (call_state) for the round-trip
// assertion.
func newSessionAnalysisState(orgID, rawRecordID string, status sessionintelligencepb.SessionAnalysisStatus, vocab int32) *model.SessionAnalysisState {
	return &model.SessionAnalysisState{
		OrganizationID:        orgID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		Status:                status,
		VocabularyVersion:     vocab,
		CallState: &sessionintelligencepb.CallStateMap{
			Calls: map[string]*sessionintelligencepb.CallState{
				"READ": {InputHash: "hash-read", ContractVersion: 3},
			},
		},
	}
}

func TestSessionAnalysisStateRepo_CreateAndGetByRawRecordID(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	state := newSessionAnalysisState(orgID, rawRecordID, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, 1)
	require.NoError(t, repo.Create(ctx, state))
	assert.NotEmpty(t, state.ID, "Create must stamp a generated id")

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Equal(t, state.ID, got.ID)
	assert.Equal(t, orgID, got.OrganizationID)
	// Proto-enum text column round-trips as the enum name.
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, got.Status)
	// JSONB proto-message column round-trips.
	require.NotNil(t, got.CallState)
	require.Contains(t, got.CallState.Calls, "READ")
	assert.Equal(t, "hash-read", got.CallState.Calls["READ"].InputHash)
	assert.Equal(t, int32(3), got.CallState.Calls["READ"].ContractVersion)

	missing, err := repo.GetByRawRecordID(ctx, uuid.NewString())
	require.ErrorIs(t, err, dao.ErrNotFound)
	assert.Nil(t, missing)
}

// TestSessionAnalysisStateRepo_GetByRawRecordID_ExcludesSuperseded is the flagship
// guard for the just-made fix: a vocabulary upgrade lets a SUPERSEDED generation and
// its live replacement coexist for one raw_record_id (the generation key carries
// vocabulary_version). GetByRawRecordID must return the live row deterministically,
// never the superseded one by primary-key order.
func TestSessionAnalysisStateRepo_GetByRawRecordID_ExcludesSuperseded(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	// The superseded generation (vocab 1) — excluded from the read and from the
	// session_analysis_state_one_live partial-unique index.
	require.NoError(t, repo.Create(ctx,
		newSessionAnalysisState(orgID, rawRecordID, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SUPERSEDED, 1)))
	// The live generation (vocab 2, a different, non-superseded status).
	live := newSessionAnalysisState(orgID, rawRecordID, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, 2)
	require.NoError(t, repo.Create(ctx, live))

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Equal(t, live.ID, got.ID, "the live generation must be returned, never the superseded one")
	assert.Equal(t, int32(2), got.VocabularyVersion)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, got.Status)
}

// TestSessionAnalysisStateRepo_Create_DuplicateLiveErrors asserts Create is
// insert-only: a second non-superseded row for the same (organization_id,
// raw_record_id) violates session_analysis_state_one_live rather than updating.
func TestSessionAnalysisStateRepo_Create_DuplicateLiveErrors(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	require.NoError(t, repo.Create(ctx,
		newSessionAnalysisState(orgID, rawRecordID, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, 1)))
	err := repo.Create(ctx,
		newSessionAnalysisState(orgID, rawRecordID, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SPINE_DONE, 2))
	require.Error(t, err, "a second live state row for the same raw record must be rejected")
}

// TestSessionAnalysisStateRepo_GetByRawRecordID_CrossTenantExclusion asserts a
// distinct org's state (under a distinct raw_record_id) is not returned for another
// org's key.
func TestSessionAnalysisStateRepo_GetByRawRecordID_CrossTenantExclusion(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgA := seedOrg(t, tx)
	orgB := seedOrg(t, tx)
	rawA := uuid.NewString()
	rawB := uuid.NewString()
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	require.NoError(t, repo.Create(ctx,
		newSessionAnalysisState(orgA, rawA, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, 1)))
	require.NoError(t, repo.Create(ctx,
		newSessionAnalysisState(orgB, rawB, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, 1)))

	got, err := repo.GetByRawRecordID(ctx, rawA)
	require.NoError(t, err)
	assert.Equal(t, orgA, got.OrganizationID, "org B's state resolves only by org B's key")
}
