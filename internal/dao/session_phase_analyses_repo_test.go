// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for SessionPhaseAnalysisRepo. session_phase_analyses is
// data-window-only (no seat column — seat scoping composes through the parent
// session_analyses), so ListBySessionAnalysisID keys on the parent's id and carries
// no seat IN-clause. The tests exercise the RETURNING insert, the phase-ordinal
// ordering, and the batched purge.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// seedParentAnalysis inserts a session_analyses row and returns its id, to satisfy
// the session_phase_analyses_parent_fk FK on (organization_id, session_analysis_id).
// The write goes through the repo under context.Background() (planscoped.WriteDB
// falls back to tenantdb.DB with no Config, and the superuser conn bypasses RLS).
func seedParentAnalysis(t *testing.T, db *gorm.DB, orgID string) string {
	t.Helper()
	parent := newSessionAnalysis(orgID, uuid.NewString(), uuid.NewString())
	require.NoError(t, dao.NewSessionAnalysisRepo(db).Create(context.Background(), parent))
	return parent.ID
}

// newSessionPhase builds a phase row at the given ordinal under a parent analysis,
// carrying a proto-enum text column (phase_theme) and a JSONB proto-message column
// (went_well) for the round-trip assertion.
func newSessionPhase(orgID, sessionAnalysisID string, ordinal int32) *model.SessionPhaseAnalysis {
	theme := sessionintelligencepb.PhaseTheme_PHASE_THEME_PLANNING
	return &model.SessionPhaseAnalysis{
		OrganizationID:        orgID,
		SessionAnalysisID:     &sessionAnalysisID,
		ConversationStartedAt: sessionTestDate,
		PhaseOrdinal:          &ordinal,
		PhaseTheme:            &theme,
		WentWell:              &sessionintelligencepb.StringList{Values: []string{"clear objective"}},
	}
}

// TestSessionPhaseAnalysisRepo_ReplacePhaseSet_FKSafeReanalysis is item 2's guardrail
// : a shorter re-analysis upserts the surviving phases by
// ordinal PRESERVING their id (so a turn's composite FK stays valid), and soft-deletes
// the trailing phase the new set dropped — an UPDATE of deleted_at, so the turn→phase
// RESTRICT FK is never tripped even while a turn still references the retired phase.
func TestSessionPhaseAnalysisRepo_ReplacePhaseSet_FKSafeReanalysis(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	ctx := context.Background()
	phaseRepo := dao.NewSessionPhaseAnalysisRepo(tx)
	turnRepo := dao.NewSessionTurnAnalysisRepo(tx)

	// First analysis: three phases, ordinals 0/1/2.
	first := []*model.SessionPhaseAnalysis{
		newSessionPhase(orgID, sessionAnalysisID, 0),
		newSessionPhase(orgID, sessionAnalysisID, 1),
		newSessionPhase(orgID, sessionAnalysisID, 2),
	}
	firstIDs, err := phaseRepo.ReplacePhaseSet(ctx, orgID, sessionAnalysisID, first)
	require.NoError(t, err)
	require.Len(t, firstIDs, 3)

	// A turn references the trailing phase (ordinal 2) via the composite FK.
	rawRecordID := uuid.NewString()
	turn := newSessionTurn(orgID, rawRecordID, 0)
	phase2ID := firstIDs[2]
	turn.PhaseID = &phase2ID
	require.NoError(t, turnRepo.Create(ctx, turn))

	// Shorter re-analysis: only ordinals 0/1 survive. The trailing phase 2 — still
	// referenced by the turn — is soft-deleted, which must NOT trip the RESTRICT FK.
	second := []*model.SessionPhaseAnalysis{
		newSessionPhase(orgID, sessionAnalysisID, 0),
		newSessionPhase(orgID, sessionAnalysisID, 1),
	}
	secondIDs, err := phaseRepo.ReplacePhaseSet(ctx, orgID, sessionAnalysisID, second)
	require.NoError(t, err, "soft-deleting a referenced phase must not trip the turn→phase RESTRICT FK")
	require.Len(t, secondIDs, 2)

	// The surviving phases kept their ids (a hard delete-and-insert would mint new ones
	// and orphan the turn's phase_id).
	assert.Equal(t, firstIDs[0], secondIDs[0], "phase 0 id is preserved across reanalysis")
	assert.Equal(t, firstIDs[1], secondIDs[1], "phase 1 id is preserved across reanalysis")

	// Only two phases are live; phase 2 is soft-deleted.
	live, err := phaseRepo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	assert.Len(t, live, 2)

	// The turn row still exists — the physical phase 2 row survives (deleted_at set),
	// so the FK is intact.
	turns, err := turnRepo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, turns, 1)
}

// TestSessionPhaseAnalysisRepo_ReplacePhaseSet_ShrinkThenGrow is the revive-on-regrow
// guardrail for phases: a shorter re-analysis soft-deletes
// trailing phases, and a later upload that grows back to the same phase_ordinal must
// REVIVE the soft-deleted row (the ordinal key is a non-partial unique, so the conflict
// update carries deleted_at = NULL) rather than leave the regrown phase hidden.
func TestSessionPhaseAnalysisRepo_ReplacePhaseSet_ShrinkThenGrow(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	ctx := context.Background()
	repo := dao.NewSessionPhaseAnalysisRepo(tx)

	three := []*model.SessionPhaseAnalysis{
		newSessionPhase(orgID, sessionAnalysisID, 0),
		newSessionPhase(orgID, sessionAnalysisID, 1),
		newSessionPhase(orgID, sessionAnalysisID, 2),
	}
	_, err := repo.ReplacePhaseSet(ctx, orgID, sessionAnalysisID, three)
	require.NoError(t, err)

	// Shrink to one ordinal.
	_, err = repo.ReplacePhaseSet(ctx, orgID, sessionAnalysisID, []*model.SessionPhaseAnalysis{
		newSessionPhase(orgID, sessionAnalysisID, 0),
	})
	require.NoError(t, err)
	shrunk, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, shrunk, 1)

	// Grow back to three: ordinals 1 and 2 must revive.
	_, err = repo.ReplacePhaseSet(ctx, orgID, sessionAnalysisID, []*model.SessionPhaseAnalysis{
		newSessionPhase(orgID, sessionAnalysisID, 0),
		newSessionPhase(orgID, sessionAnalysisID, 1),
		newSessionPhase(orgID, sessionAnalysisID, 2),
	})
	require.NoError(t, err)
	regrown, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	assert.Len(t, regrown, 3, "the regrown phases must be live again (revive-on-regrow)")
}

func TestSessionPhaseAnalysisRepo_CreateAndListBySessionAnalysisID(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	sessionAnalysisID := seedParentAnalysis(t, tx, orgID)
	ctx := context.Background()
	repo := dao.NewSessionPhaseAnalysisRepo(tx)

	// Insert out of ordinal order to prove the ORDER BY decides the result.
	require.NoError(t, repo.Create(ctx, newSessionPhase(orgID, sessionAnalysisID, 2)))
	require.NoError(t, repo.Create(ctx, newSessionPhase(orgID, sessionAnalysisID, 0)))
	require.NoError(t, repo.Create(ctx, newSessionPhase(orgID, sessionAnalysisID, 1)))

	got, err := repo.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.NotNil(t, got[0].PhaseOrdinal)
	assert.Equal(t, int32(0), *got[0].PhaseOrdinal)
	assert.Equal(t, int32(1), *got[1].PhaseOrdinal)
	assert.Equal(t, int32(2), *got[2].PhaseOrdinal, "phases must be ordered by phase_ordinal ASC")
	// Round-trips on the first phase.
	require.NotNil(t, got[0].PhaseTheme)
	assert.Equal(t, sessionintelligencepb.PhaseTheme_PHASE_THEME_PLANNING, *got[0].PhaseTheme)
	require.NotNil(t, got[0].WentWell)
	assert.Equal(t, []string{"clear objective"}, got[0].WentWell.Values)

	empty, err := repo.ListBySessionAnalysisID(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestSessionPhaseAnalysisRepo_ListBySessionAnalysisID_CrossTenantExclusion asserts
// a distinct org's phases (under a distinct parent id) are not returned for another
// org's parent.
func TestSessionPhaseAnalysisRepo_ListBySessionAnalysisID_CrossTenantExclusion(t *testing.T) {
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
	repo := dao.NewSessionPhaseAnalysisRepo(tx)

	require.NoError(t, repo.Create(ctx, newSessionPhase(orgA, parentA, 0)))
	require.NoError(t, repo.Create(ctx, newSessionPhase(orgB, parentB, 0)))

	got, err := repo.ListBySessionAnalysisID(ctx, parentA)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, orgA, got[0].OrganizationID, "org B's phases resolve only by org B's parent id")
}
