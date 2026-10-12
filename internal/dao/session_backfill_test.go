// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for the cutover hard-floor guardrails: the CutoverDate is a hard
// write floor — the LIVE no-state leg admits only data_date >= cutover, and there is NO
// pre-cutover no-state leg, so a pre-cutover record is never LLM-processed (it is carried
// onto the new tables by the copy operational job instead).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// seedSessionRawAtDate inserts one session raw_record on a chosen data_date (the
// date axis the backfill descends and the cursor frontier reads).
func seedSessionRawAtDate(t *testing.T, tx *gorm.DB, orgID string, date time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"s3_key": "s3://k/" + uuid.NewString()})
	require.NoError(t, err)
	rec := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      scanConnectorID,
		OperationID:      "push_conversations",
		RecordKey:        uuid.NewString(),
		DataDate:         date,
		Payload:          payload,
		FetchedAt:        date,
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, tx.Create(rec).Error)
	return rec.ID
}

// cutoverScanParams is a resolved-active param set with a configured cutover floor; the
// version scalars are current so the version-behind leg never fires on these rows.
func cutoverScanParams(orgID string, cutover time.Time) dao.SessionAnalysisScanParams {
	return dao.SessionAnalysisScanParams{
		OrgID:               orgID,
		ConnectorID:         scanConnectorID,
		CutoverDate:         cutover,
		RetryCap:            3,
		VocabularyVersion:   1,
		SpineVersion:        1,
		ContractVersionRead: 1, ContractVersionEvidence: 1, ContractVersionDiscard: 1,
	}
}

// TestSessionAnalysisStateRepo_CutoverHardFloor is the hard-floor guardrail: with a cutover
// configured, the LIVE no-state leg admits only data_date >= cutover, and a pre-cutover
// no-state record is returned by NEITHER lane — it is never LLM-processed (it is carried onto
// the new tables by a backfill job). With no cutover the LIVE lane admits every in-window
// no-state row (the no-floor default), and the re-admission lane never returns a no-state row.
func TestSessionAnalysisStateRepo_CutoverHardFloor(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	cutover := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seedSessionRawAtDate(t, tx, orgID, time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)) // day-before-cutover, must never be picked
	seedSessionRawAtDate(t, tx, orgID, time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)) // preOld, must never be picked
	onBoundary := seedSessionRawAtDate(t, tx, orgID, cutover)                        // data_date == cutover → picked (floor is >=, inclusive)
	live := seedSessionRawAtDate(t, tx, orgID, time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC))

	params := cutoverScanParams(orgID, cutover)

	liveCands, err := repo.ScanLiveActionable(ctx, params, 50)
	require.NoError(t, err)
	got := make(map[string]bool, len(liveCands))
	for _, c := range liveCands {
		got[c.RawRecordID] = true
		assert.Equal(t, dao.CategoryNoState, c.Category)
	}
	assert.Len(t, liveCands, 2, "LIVE admits data_date >= cutover — the on-boundary and post-cutover rows only")
	assert.True(t, got[onBoundary], "a record ON the cutover date is admitted (the floor is >=, inclusive)")
	assert.True(t, got[live], "a post-cutover record is admitted")

	// The re-admission lane carries no no-state leg and no re-admission rows are seeded, so the
	// two pre-cutover records are NEVER returned by it (the hard floor — they are copy-migrated,
	// not LLM-processed). Assert emptiness directly so re-introducing a historical no-state leg
	// fails loudly (a range loop over an empty slice would pass vacuously).
	backfillCands, err := repo.ScanBackfillActionable(ctx, params, 50)
	require.NoError(t, err)
	assert.Empty(t, backfillCands, "the configured-cutover re-admission lane returns no rows for a pure no-state backlog")

	// A zero cutover is the no-floor default: LIVE admits every in-window no-state row.
	unconfigured := cutoverScanParams(orgID, time.Time{})
	liveAll, err := repo.ScanLiveActionable(ctx, unconfigured, 50)
	require.NoError(t, err)
	assert.Len(t, liveAll, 4, "with no cutover the LIVE lane admits all four no-state rows")
	backfillNone, err := repo.ScanBackfillActionable(ctx, unconfigured, 50)
	require.NoError(t, err)
	assert.Empty(t, backfillNone, "the re-admission lane matches no no-state rows regardless of cutover")
}
