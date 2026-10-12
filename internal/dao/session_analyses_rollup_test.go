// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// TestSessionAnalysisRepo_NewVocabRollupColumns pins the two R-owned new-vocabulary rollups
// (taxonomy_issues, waste_categories, session-analyses-serving-parity design): they round-trip on a
// judged upsert, survive a spine-only measured refresh (they are judged columns, not measured), and
// are NULLed by a judge failure.
func TestSessionAnalysisRepo_NewVocabRollupColumns(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	person := uuid.NewString()
	rawRecordID := uuid.NewString()
	ctx := loopTestCtx(person)
	repo := dao.NewSessionAnalysisRepo(tx)

	judged := newSessionAnalysis(orgID, person, rawRecordID)
	judged.WasteCategories = &sessionintelligencepb.TokenBreakdownList{Items: []*sessionintelligencepb.TokenBreakdown{
		{Category: "redundant-reread", Tokens: 300},
		{Category: "over-fetch", Tokens: 500},
	}}
	judged.TaxonomyIssues = &sessionintelligencepb.TaxonomyIssueList{Items: []*sessionintelligencepb.TaxonomyIssue{
		{CategorySlug: "api-key-leak", Severity: "high", Layer: "security"},
		{CategorySlug: "ambiguous-directive", Severity: "medium"},
	}}
	require.NoError(t, repo.UpsertJudged(ctx, judged, true))

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.NotNil(t, got.WasteCategories, "waste_categories round-trips on a judged upsert")
	require.Len(t, got.WasteCategories.GetItems(), 2)
	assert.Equal(t, "over-fetch", got.WasteCategories.GetItems()[1].GetCategory())
	assert.Equal(t, int64(500), got.WasteCategories.GetItems()[1].GetTokens())
	require.NotNil(t, got.TaxonomyIssues, "taxonomy_issues round-trips on a judged upsert")
	require.Len(t, got.TaxonomyIssues.GetItems(), 2)
	assert.Equal(t, "api-key-leak", got.TaxonomyIssues.GetItems()[0].GetCategorySlug())
	assert.Equal(t, "security", got.TaxonomyIssues.GetItems()[0].GetLayer())

	// A spine-only measured refresh (no verdict) must leave the judged rollups untouched — they are
	// in the judged set, not the measured set, so the "spine-only reprocess carries them unchanged"
	// contract holds.
	measured := &model.SessionAnalysis{
		OrganizationID:        orgID,
		PersonID:              person,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		VocabularyVersion:     1,
	}
	newPrompts := int32(9)
	measured.TotalPrompts = &newPrompts
	require.NoError(t, repo.UpsertMeasured(ctx, measured))

	got, err = repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.NotNil(t, got.WasteCategories, "waste_categories survives a spine-only measured refresh")
	assert.Len(t, got.WasteCategories.GetItems(), 2)
	require.NotNil(t, got.TaxonomyIssues, "taxonomy_issues survives a spine-only measured refresh")
	assert.Len(t, got.TaxonomyIssues.GetItems(), 2)

	// A judge failure NULLs the judged set, including the two rollups.
	changed, err := repo.MarkJudgeFailed(ctx, rawRecordID)
	require.NoError(t, err)
	require.True(t, changed)
	got, err = repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Nil(t, got.WasteCategories, "a failed judge NULLs waste_categories")
	assert.Nil(t, got.TaxonomyIssues, "a failed judge NULLs taxonomy_issues")
}
