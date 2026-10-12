// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for the session-analysis actionable scan and the CAS
// state-machine transitions. The two
// guardrails this file carries: the actionable-scan matrix (every category lands in
// its lane, and a JUDGED-at-current row is excluded from both), and the double-claim
// backstop (the live-generation partial unique means a second claim on one raw
// record inserts nothing).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

const scanConnectorID = "session"

// seedSessionRaw inserts one session raw_record with the given content sha and
// returns its id. content sha rides payload.content_sha256, the cheap SQL staleness
// signal the category-3 scan compares.
func seedSessionRaw(t *testing.T, tx *gorm.DB, orgID, contentSha string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"s3_key": "s3://k/" + uuid.NewString(), "content_sha256": contentSha})
	require.NoError(t, err)
	rec := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      scanConnectorID,
		OperationID:      "push_conversations",
		RecordKey:        uuid.NewString(),
		DataDate:         sessionTestDate,
		Payload:          payload,
		FetchedAt:        sessionTestDate,
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, tx.Create(rec).Error)
	return rec.ID
}

// seedSessionRawLegacy inserts a LEGACY session raw_record — its payload carries no
// content_sha256 (an older client), the case the category-3 legacy branch keys on — and pins
// its updated_at to the given instant (GORM stamps updated_at on Create, so the seed overrides
// it with a raw UPDATE). Returns the id.
func seedSessionRawLegacy(t *testing.T, tx *gorm.DB, orgID string, updatedAt time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"s3_key": "s3://k/" + uuid.NewString()})
	require.NoError(t, err)
	rec := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      scanConnectorID,
		OperationID:      "push_conversations",
		RecordKey:        uuid.NewString(),
		DataDate:         sessionTestDate,
		Payload:          payload,
		FetchedAt:        sessionTestDate,
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, tx.Create(rec).Error)
	require.NoError(t, tx.Exec("UPDATE raw_records SET updated_at = ? WHERE id = ?", updatedAt.UTC(), rec.ID).Error)
	return rec.ID
}

// insertState writes a state row directly (bypassing the repo's default-stamping) so
// a test can fix every lifecycle column the scan predicates read.
func insertState(t *testing.T, tx *gorm.DB, state *model.SessionAnalysisState) {
	t.Helper()
	if state.ID == "" {
		state.ID = uuid.NewString()
	}
	require.NoError(t, tx.Create(state).Error)
}

func TestSessionAnalysisStateRepo_ActionableScanMatrix(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	seatPerson := uuid.NewString()
	oldAttempt := sessionTestDate.Add(-24 * time.Hour)

	// LIVE-lane rows.
	rawNoState := seedSessionRaw(t, tx, orgID, "sha-x") // cat 1: no state row at all

	rawLease := seedSessionRaw(t, tx, orgID, "sha-x")
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawLease, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, VocabularyVersion: 2,
		LastAttemptedAt: &oldAttempt,
	})

	rawFail := seedSessionRaw(t, tx, orgID, "sha-x")
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawFail, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, VocabularyVersion: 2,
		AttemptCount: 1,
	})

	rawContent := seedSessionRaw(t, tx, orgID, "sha-new")
	oldSha := "sha-old"
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawContent, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &oldSha, // differs from payload sha-new → content changed
	})

	rawSeat := seedSessionRaw(t, tx, orgID, "sha-x")
	noSeat := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawSeat, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &noSeat, PersonID: &seatPerson,
	})

	// BACKFILL-lane rows.
	rawWindow := seedSessionRaw(t, tx, orgID, "sha-x")
	outOfWindow := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawWindow, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &outOfWindow,
	})

	rawRepo := seedSessionRaw(t, tx, orgID, "sha-x")
	repoUntracked := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_METADATA
	trackedRepo := "org/repo"
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRepo, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &repoUntracked, RepoName: &trackedRepo,
	})

	// A JUDGED row always carries its spine-stamped source sha (matching the body),
	// so it is not seen as content-changed — only version-behind / producer-enabled.
	stampedSha := "sha-x"
	rawVersion := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawVersion, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 1, // behind current (2)
		SourceContentSha256: &stampedSha,
	})

	// E (bit 2) was flag-skipped and is now enabled → re-admit as PRODUCER_ENABLED.
	rawProducer := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawProducer, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		FlagSkippedProducers: 2, SourceContentSha256: &stampedSha, // bit 2 (E), now enabled
	})

	// D (bit 4) is flag-skipped but stays DARK (not in EnabledProducerBits) → must NOT churn
	// re-admission. This is the category-9 empty-seek guard: a permanently-dark producer's bit
	// is masked out so the row is never picked with no work to do.
	rawProducerDark := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawProducerDark, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		FlagSkippedProducers: 4, SourceContentSha256: &stampedSha, // bit 4 (D), stays dark
	})

	// A JUDGED parent whose delegated child arrived after its last fold (children_seq
	// advanced past folded_children_seq) and whose last success predates the refold
	// throttle → re-admitted as CHILD_REFOLD.
	oldSuccess := sessionTestDate.Add(-24 * time.Hour)
	rawRefold := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRefold, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &stampedSha, ChildrenSeq: 1, FoldedChildrenSeq: 0, LastSucceededAt: &oldSuccess,
	})

	// The same drift but a RECENT success → within the refold throttle, so not re-admitted.
	recentSuccess := sessionTestDate.Add(time.Hour)
	rawRefoldThrottled := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRefoldThrottled, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &stampedSha, ChildrenSeq: 1, FoldedChildrenSeq: 0, LastSucceededAt: &recentSuccess,
	})

	// The excluded row: JUDGED at the current version, matching content sha, no
	// flag-skipped producer — actionable in neither lane.
	rawExcluded := seedSessionRaw(t, tx, orgID, "sha-same")
	sameSha := "sha-same"
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawExcluded, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sameSha,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             scanConnectorID,
		LiveWatermark:           time.Time{}, // admit every no-state row (backfill posture)
		RetryCap:                3,
		ThrottleCutoff:          sessionTestDate.Add(time.Hour), // last_succeeded_at (NULL here) < cutoff
		LeaseCutoff:             sessionTestDate,                // oldAttempt (−24h) < cutoff → lease expired
		Seats:                   []string{seatPerson},
		SeatCapEnabled:          true,
		Cutoff:                  sessionTestDate.Add(-48 * time.Hour), // conversation_started_at ≥ cutoff
		TrackedRepos:            []string{trackedRepo},
		RepoFilterEnabled:       true,
		VocabularyVersion:       2,
		VocabularyEffectiveFrom: sessionTestDate,
		ChildRefoldCutoff:       sessionTestDate, // oldSuccess (−24h) < cutoff; recentSuccess (+1h) is not
		EnabledProducerBits:     3,               // R (bit 1) + E (bit 2) enabled; D (bit 4) dark
	}

	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)
	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)

	liveCats := candidateCategories(live)
	backfillCats := candidateCategories(backfill)

	assert.Equal(t, dao.CategoryNoState, liveCats[rawNoState])
	assert.Equal(t, dao.CategoryLeaseExpired, liveCats[rawLease])
	assert.Equal(t, dao.CategoryRetryableFailure, liveCats[rawFail])
	assert.Equal(t, dao.CategoryContentChanged, liveCats[rawContent])
	assert.Equal(t, dao.CategorySeatGranted, liveCats[rawSeat])
	assert.Len(t, live, 5, "LIVE lane returns exactly the five LIVE-category rows")

	assert.Equal(t, dao.CategoryWindowWidened, backfillCats[rawWindow])
	assert.Equal(t, dao.CategoryRepoConnected, backfillCats[rawRepo])
	assert.Equal(t, dao.CategoryVersionBehind, backfillCats[rawVersion])
	assert.Equal(t, dao.CategoryProducerEnabled, backfillCats[rawProducer])
	assert.Equal(t, dao.CategoryChildRefold, backfillCats[rawRefold])
	assert.Len(t, backfill, 5, "BACKFILL lane returns exactly the five BACKFILL-category rows")

	assert.NotContains(t, backfillCats, rawRefoldThrottled, "a child-drifted row within the refold throttle is not re-admitted")
	assert.NotContains(t, backfillCats, rawProducerDark, "a permanently-dark flag-skipped producer does not churn PRODUCER_ENABLED re-admission")
	assert.NotContains(t, liveCats, rawExcluded, "a JUDGED-at-current row is excluded from LIVE")
	assert.NotContains(t, backfillCats, rawExcluded, "a JUDGED-at-current row is excluded from BACKFILL")
}

func candidateCategories(cands []*dao.ActionableCandidate) map[string]dao.ActionableCategory {
	out := make(map[string]dao.ActionableCategory, len(cands))
	for _, c := range cands {
		out[c.RawRecordID] = c.Category
	}
	return out
}

// TestSessionAnalysisStateRepo_CopiedParentFrozen pins the backfill freeze on the
// child-refold path (session-analyses-serving-parity design). A copied row (origin =
// SESSION_COPY) carries Session's finished output verbatim, so a delegated child arriving against
// it must neither advance its fence (BumpChildrenSeq) nor re-admit it for a refold — else the engine
// would re-judge frozen history. A live row (NULL origin) with the identical child drift is the
// control: it proves the origin guard excludes copies specifically, not every JUDGED row.
func TestSessionAnalysisStateRepo_CopiedParentFrozen(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	stampedSha := "sha-x"
	oldSuccess := sessionTestDate.Add(-24 * time.Hour)
	copyOrigin := sessionintelligencepb.SessionAnalysisOrigin_SESSION_ANALYSIS_ORIGIN_COPY

	// A copied parent, terminally JUDGED, whose child drifted past the fold fence with a stale
	// success — every predicate the child-refold leg keys on is satisfied EXCEPT the origin guard.
	rawCopied := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawCopied, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &stampedSha, ChildrenSeq: 1, FoldedChildrenSeq: 0, LastSucceededAt: &oldSuccess,
		Origin: &copyOrigin,
	})

	// The identical drift on a live row (NULL origin) — the control that must still re-admit.
	rawLive := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawLive, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &stampedSha, ChildrenSeq: 1, FoldedChildrenSeq: 0, LastSucceededAt: &oldSuccess,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             scanConnectorID,
		LiveWatermark:           time.Time{},
		RetryCap:                3,
		ThrottleCutoff:          sessionTestDate.Add(time.Hour),
		LeaseCutoff:             sessionTestDate,
		Cutoff:                  sessionTestDate.Add(-48 * time.Hour),
		VocabularyVersion:       2,
		VocabularyEffectiveFrom: sessionTestDate,
		ChildRefoldCutoff:       sessionTestDate, // oldSuccess (−24h) < cutoff
		EnabledProducerBits:     3,
	}

	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)
	cats := candidateCategories(backfill)

	assert.Equal(t, dao.CategoryChildRefold, cats[rawLive], "a live child-drifted parent is re-admitted as CHILD_REFOLD")
	assert.NotContains(t, cats, rawCopied, "a copied (SESSION_COPY) parent is frozen — never a child-refold candidate")

	// BumpChildrenSeq must never advance a copied parent's fence; the live parent's does advance.
	bumpedCopy, err := repo.BumpChildrenSeq(ctx, rawCopied)
	require.NoError(t, err)
	assert.False(t, bumpedCopy, "BumpChildrenSeq does not advance a copied parent's fence")

	bumpedLive, err := repo.BumpChildrenSeq(ctx, rawLive)
	require.NoError(t, err)
	assert.True(t, bumpedLive, "BumpChildrenSeq advances a live parent's fence")
}

// seedSessionRawOnDate is seedSessionRaw with an explicit data_date, so the ordering
// test can space rows across dates (the testutil default partition accepts any date).
func seedSessionRawOnDate(t *testing.T, tx *gorm.DB, orgID, contentSha string, dataDate time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"s3_key": "s3://k/" + uuid.NewString(), "content_sha256": contentSha})
	require.NoError(t, err)
	rec := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      scanConnectorID,
		OperationID:      "push_conversations",
		RecordKey:        uuid.NewString(),
		DataDate:         dataDate,
		Payload:          payload,
		FetchedAt:        dataDate,
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, tx.Create(rec).Error)
	return rec.ID
}

// TestSessionAnalysisStateRepo_PendingDataReadmit pins the per-producer reprocessing
// re-admission leg: a JUDGED or SPINE_DONE row
// whose producer went SKIPPED_PENDING_DATA (pending_data_producers <> 0) is re-selected in
// the BACKFILL lane as CategoryPendingData, while an otherwise-identical row with no pending
// bit is not — the bit, not the call_state-only status, drives re-admission.
func TestSessionAnalysisStateRepo_PendingDataReadmit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	sha := "sha-x"
	recent := sessionTestDate // > LeaseCutoff below, so a SPINE_DONE row is not lease-expired

	// JUDGED, current version, matching sha, E half pending (bit 2).
	rawPendingJudged := seedSessionRaw(t, tx, orgID, sha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawPendingJudged, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sha, PendingDataProducers: 2,
	})
	// SPINE_DONE with a recent attempt (not lease-expired), also E-pending.
	rawPendingSpine := seedSessionRaw(t, tx, orgID, sha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawPendingSpine, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SPINE_DONE, VocabularyVersion: 2,
		SourceContentSha256: &sha, PendingDataProducers: 2, LastAttemptedAt: &recent,
	})
	// JUDGED, current version, matching sha, no pending bit — must NOT re-admit.
	rawNotPending := seedSessionRaw(t, tx, orgID, sha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawNotPending, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sha,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             scanConnectorID,
		RetryCap:                3,
		ThrottleCutoff:          sessionTestDate.Add(time.Hour),
		LeaseCutoff:             sessionTestDate.Add(-time.Hour), // recent (=sessionTestDate) is NOT before this
		VocabularyVersion:       2,
		VocabularyEffectiveFrom: sessionTestDate.Add(-240 * time.Hour),
		EnabledProducerBits:     3,
	}
	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)
	backfillCats := candidateCategories(backfill)
	assert.Equal(t, dao.CategoryPendingData, backfillCats[rawPendingJudged])
	assert.Equal(t, dao.CategoryPendingData, backfillCats[rawPendingSpine])
	assert.NotContains(t, backfillCats, rawNotPending, "a JUDGED row with no pending bit is not re-admitted")
}

// TestSessionAnalysisStateRepo_ProducerFailedReadmit pins the failed-producer re-admission leg
// : a JUDGED/SPINE_DONE row whose producer went CALL_STATUS_FAILED
// (failed_producers <> 0) re-admits as CategoryProducerFailed UNDER the retry cap, but not once
// attempt_count reaches the cap (terminal — R stays served, the producer stays FAILED), and never
// when no failed bit is set.
func TestSessionAnalysisStateRepo_ProducerFailedReadmit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	sha := "sha-f"

	// JUDGED, D failed (bit 4), attempt_count 1 < cap → re-admits.
	rawUnderCap := seedSessionRaw(t, tx, orgID, sha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawUnderCap, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sha, FailedProducers: 4, AttemptCount: 1,
	})
	// JUDGED, E failed (bit 2), attempt_count 3 == cap → terminal, NOT re-admitted.
	rawAtCap := seedSessionRaw(t, tx, orgID, sha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawAtCap, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sha, FailedProducers: 2, AttemptCount: 3,
	})
	// JUDGED, no failed bit → NOT re-admitted.
	rawNoFail := seedSessionRaw(t, tx, orgID, sha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawNoFail, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sha,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             scanConnectorID,
		RetryCap:                3,
		ThrottleCutoff:          sessionTestDate.Add(time.Hour),
		LeaseCutoff:             sessionTestDate.Add(-time.Hour),
		VocabularyVersion:       2,
		VocabularyEffectiveFrom: sessionTestDate.Add(-240 * time.Hour),
		EnabledProducerBits:     7,
	}
	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)
	cats := candidateCategories(backfill)
	assert.Equal(t, dao.CategoryProducerFailed, cats[rawUnderCap], "a failed producer under the cap re-admits")
	assert.NotContains(t, cats, rawAtCap, "at the retry cap the failed producer is terminal — not re-admitted")
	assert.NotContains(t, cats, rawNoFail, "no failed bit → not re-admitted")
}

// TestSessionAnalysisStateRepo_NoStateFirstOrdering pins the NO_STATE-first cross-leg sort
// : NO_STATE ranks above every re-admission regardless of
// data_date, while every re-admission — pending-data included — is a class-1 peer; within a
// class the pre-slice data_date DESC order is preserved.
// TestSessionAnalysisStateRepo_BackfillReadmissionOrdering pins the re-admission-lane ordering
// under the cutover hard floor: the lane carries NO historical no-state leg, so a pre-cutover
// no-state record is NEVER returned (the hard floor — it is copy-migrated, not LLM-processed),
// and the re-admissions it does return (version-behind, pending-data) are class-1 peers ordered
// data_date DESC. The NO_STATE-first class-0 priority (the unchanged classRank CASE) is a LIVE-lane
// property now — the LIVE no-state leg is exercised by TestSessionAnalysisStateRepo_CutoverHardFloor.
func TestSessionAnalysisStateRepo_BackfillReadmissionOrdering(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	sha := "sha-x"
	cutover := sessionTestDate.AddDate(0, 0, 1) // every seeded row is pre-cutover

	// NO_STATE, OLDEST date — pre-cutover, so the hard floor means it is NEVER returned.
	rawNoState := seedSessionRawOnDate(t, tx, orgID, sha, sessionTestDate.AddDate(0, 0, -5))
	// Two VERSION_BEHIND rows (class 1) with distinct dates — the newer must precede the older.
	rawVBOlder := seedSessionRawOnDate(t, tx, orgID, sha, sessionTestDate.AddDate(0, 0, -3))
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawVBOlder, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 1,
		SourceContentSha256: &sha,
	})
	rawVBNewer := seedSessionRawOnDate(t, tx, orgID, sha, sessionTestDate.AddDate(0, 0, -1))
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawVBNewer, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 1,
		SourceContentSha256: &sha,
	})
	// PENDING_DATA, NEWEST date — a class-1 peer of the re-admissions, so it leads them by date.
	rawPending := seedSessionRawOnDate(t, tx, orgID, sha, sessionTestDate)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawPending, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &sha, PendingDataProducers: 2,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             scanConnectorID,
		CutoverDate:             cutover,
		RetryCap:                3,
		ThrottleCutoff:          sessionTestDate.Add(time.Hour),
		LeaseCutoff:             sessionTestDate.AddDate(0, 0, -10),
		VocabularyVersion:       2,
		VocabularyEffectiveFrom: sessionTestDate.AddDate(0, 0, -30),
		EnabledProducerBits:     3,
	}
	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)

	order := make([]string, 0, len(backfill))
	for _, c := range backfill {
		order = append(order, c.RawRecordID)
	}
	assert.NotContains(t, order, rawNoState,
		"the pre-cutover no-state record is NEVER returned — the cutover hard floor (it is copy-migrated)")
	assert.Equal(t, []string{rawPending, rawVBNewer, rawVBOlder}, order,
		"the re-admissions — pending-data included — are class-1 peers ordered data_date DESC")
}

// TestSessionAnalysisStateRepo_MasterOffNoChurn is the master-gate-off no-churn guard:
// enabledProducerBits gates on the master
// SessionAnalysisEnabled gate, so a master-off org's mask is 0. A JUDGED row every producer
// flag-skipped on (R+E+D, mask 7) then intersects EnabledProducerBits=0 to zero — the same
// category-9 empty-seek guard the D-dark case above pins — so the row must not surface as a
// candidate on either lane, sparing the spine/S3 re-read while the org stays dark.
func TestSessionAnalysisStateRepo_MasterOffNoChurn(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	stampedSha := "sha-x"
	rawMasterOff := seedSessionRaw(t, tx, orgID, stampedSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawMasterOff, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		FlagSkippedProducers: 7, SourceContentSha256: &stampedSha, // R+E+D all flag-skipped (master gate off)
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             scanConnectorID,
		LiveWatermark:           time.Time{},
		RetryCap:                3,
		ThrottleCutoff:          sessionTestDate.Add(time.Hour),
		LeaseCutoff:             sessionTestDate,
		Cutoff:                  sessionTestDate.Add(-48 * time.Hour),
		VocabularyVersion:       2,
		VocabularyEffectiveFrom: sessionTestDate,
		ChildRefoldCutoff:       sessionTestDate,
		EnabledProducerBits:     0, // what enabledProducerBits returns for the org while the master gate is off
	}

	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)
	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)

	assert.Empty(t, live, "a master-off org's flag-skipped row must not re-admit on LIVE")
	assert.Empty(t, backfill, "a master-off org's flag-skipped row must not re-admit on BACKFILL")
}

// TestSessionAnalysisStateRepo_LineageCorrectedReadmit guards the lineage-correction leg
// (Codex P1): a SKIPPED(CHILD) row is re-admitted once its re-upload corrects relation_type
// away from a child type (else it stays skipped forever after Session retires), while a row
// that is still a child never churns re-admission.
func TestSessionAnalysisStateRepo_LineageCorrectedReadmit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	child := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD

	// Still a delegated child (relation_type SUBAGENT) → must NOT be re-admitted.
	rawStillChild := seedSessionRaw(t, tx, orgID, "sha-a")
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"relation_type":"SUBAGENT"}`, rawStillChild).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawStillChild, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &child,
	})

	// Corrected to a top-level session (relation_type PRIMARY) → re-admitted as LINEAGE_CORRECTED.
	rawCorrected := seedSessionRaw(t, tx, orgID, "sha-b")
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"relation_type":"PRIMARY"}`, rawCorrected).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawCorrected, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &child,
	})

	params := dao.SessionAnalysisScanParams{OrgID: orgID, ConnectorID: scanConnectorID, VocabularyVersion: 2}
	backfill, err := repo.ScanBackfillActionable(ctx, params, 100)
	require.NoError(t, err)

	cats := candidateCategories(backfill)
	assert.Equal(t, dao.CategoryLineageCorrected, cats[rawCorrected], "a child corrected to PRIMARY is re-admitted")
	assert.NotContains(t, cats, rawStillChild, "a row that is still a child never churns re-admission")
}

// TestSessionAnalysisStateRepo_ChildRepriceReadmit guards the child-reprice leg (Codex P1): a
// SKIPPED(CHILD) row that is re-uploaded with a CHANGED body but is STILL a child must be
// re-admitted so its cost is re-priced and re-folded into the parent — but ONLY once the compat
// write is live (the leg is gated on CompatWriteEnabled, dormant during the parallel run where
// Session still re-prices changed children). Content-drift keys on the payload content_sha256 vs
// the stored anchor, with the raw_records.updated_at fallback for legacy children carrying no sha.
func TestSessionAnalysisStateRepo_ChildRepriceReadmit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	child := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD

	// Still a child (SUBAGENT), body changed: payload sha (sha-new) differs from the stored anchor
	// (sha-old) → re-admitted as CHILD_REPRICE.
	oldSha := "sha-old"
	rawChanged := seedSessionRaw(t, tx, orgID, "sha-new")
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"relation_type":"SUBAGENT"}`, rawChanged).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawChanged, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &child, SourceContentSha256: &oldSha,
	})

	// Still a child, body unchanged: payload sha matches the anchor → must NOT churn.
	sameSha := "sha-same"
	rawUnchanged := seedSessionRaw(t, tx, orgID, sameSha)
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"relation_type":"PLUGIN"}`, rawUnchanged).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawUnchanged, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &child, SourceContentSha256: &sameSha,
	})

	// Legacy child (no payload sha), re-upload bumped updated_at past the stored anchor → re-admitted
	// via the updated_at fallback.
	anchor := sessionTestDate.Add(-time.Hour)
	rawLegacy := seedSessionRawLegacy(t, tx, orgID, sessionTestDate.Add(time.Hour))
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"relation_type":"HOOK"}`, rawLegacy).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawLegacy, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &child, SourceUpdatedAt: &anchor,
	})

	// Gate OFF: the leg is dormant, nothing is re-admitted.
	dormant, err := repo.ScanBackfillActionable(ctx,
		dao.SessionAnalysisScanParams{OrgID: orgID, ConnectorID: scanConnectorID, VocabularyVersion: 2, CompatWriteEnabled: false}, 100)
	require.NoError(t, err)
	assert.NotContains(t, candidateCategories(dormant), rawChanged, "the child-reprice leg is dormant while the compat write is off")

	// Gate ON: changed + legacy-drifted children re-admit; the unchanged one does not.
	live, err := repo.ScanBackfillActionable(ctx,
		dao.SessionAnalysisScanParams{OrgID: orgID, ConnectorID: scanConnectorID, VocabularyVersion: 2, CompatWriteEnabled: true}, 100)
	require.NoError(t, err)
	cats := candidateCategories(live)
	assert.Equal(t, dao.CategoryChildReprice, cats[rawChanged], "a still-child with a changed body is re-admitted for repricing")
	assert.Equal(t, dao.CategoryChildReprice, cats[rawLegacy], "a legacy still-child whose updated_at moved is re-admitted")
	assert.NotContains(t, cats, rawUnchanged, "a still-child whose body is unchanged never churns")
}

// TestSessionAnalysisStateRepo_OwnershipChangedReadmit guards the ownership-drift leg (Codex P1):
// a JUDGED row whose metadata person_id was corrected by an unchanged-payload re-upload is
// re-admitted for re-attribution, while a row whose owner is unchanged is not (no churn).
func TestSessionAnalysisStateRepo_OwnershipChangedReadmit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	// Drifted: rr.metadata.person_id is corrected to newPerson while state still holds oldPerson,
	// and the re-upload bumped rr.updated_at past st.updated_at. source_content_sha256 matches the
	// payload so the content-changed leg does NOT fire (unchanged body) — only the ownership leg.
	oldPerson := uuid.NewString()
	newPerson := uuid.NewString()
	sha := "sha-drift"
	rawDrift := seedSessionRaw(t, tx, orgID, sha)
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb, updated_at = now() + interval '1 hour' WHERE id = ?`,
		`{"person_id":"`+newPerson+`"}`, rawDrift).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawDrift, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		PersonID: &oldPerson, SourceContentSha256: &sha,
	})

	// No drift: state person matches the metadata person → must NOT be re-admitted.
	samePerson := uuid.NewString()
	shaB := "sha-same"
	rawSame := seedSessionRaw(t, tx, orgID, shaB)
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb, updated_at = now() + interval '1 hour' WHERE id = ?`,
		`{"person_id":"`+samePerson+`"}`, rawSame).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawSame, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		PersonID: &samePerson, SourceContentSha256: &shaB,
	})

	params := dao.SessionAnalysisScanParams{OrgID: orgID, ConnectorID: scanConnectorID}
	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)

	cats := candidateCategories(live)
	assert.Equal(t, dao.CategoryOwnershipChanged, cats[rawDrift], "a corrected metadata person_id re-admits the judged row")
	assert.NotContains(t, cats, rawSame, "a row whose owner is unchanged is not re-admitted")
}

// TestSessionAnalysisStateRepo_TransitionToJudgedRefreshesPersonID asserts the state row's
// person_id is re-stamped from the commit on every judge (so the ownership-drift leg clears).
func TestSessionAnalysisStateRepo_TransitionToJudgedRefreshesPersonID(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	rawRecordID := uuid.NewString()
	oldPerson := uuid.NewString()
	newPerson := uuid.NewString()

	state := &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
		VocabularyVersion: 2, PersonID: &oldPerson,
	}
	claimed, err := repo.ClaimForProcessing(ctx, state)
	require.NoError(t, err)
	require.True(t, claimed)
	token := *state.ClaimToken

	spineOK, err := repo.TransitionToSpineDone(ctx, rawRecordID, token, "sha", sessionTestDate, 1, sessionTestDate)
	require.NoError(t, err)
	require.True(t, spineOK)

	commit := dao.JudgedCommit{VocabularyVersion: 2, PersonID: &newPerson, Disposition: sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED}
	judgedOK, err := repo.TransitionToJudged(ctx, rawRecordID, token, 0, commit)
	require.NoError(t, err)
	require.True(t, judgedOK)

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.NotNil(t, got.PersonID)
	assert.Equal(t, newPerson, *got.PersonID, "the judge re-stamps the current metadata person_id onto state")
}

// TestSessionAnalysisStateRepo_TransitionToJudgedAttemptCount pins the failed-producer attempt
// semantics: a fully-successful judge (FailedProducers==0) RESETS
// attempt_count to 0 and clears failed_producers, while a judge carrying a FAILED producer COUNTS
// toward the retry cap (increments) and stamps the failed_producers bit — unless the failure was a
// provider outage (IncrementAttempt=false), which leaves the count unchanged.
func TestSessionAnalysisStateRepo_TransitionToJudgedAttemptCount(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	boom, outage := "boom", "503"
	tests := []struct {
		name              string
		startAttempt      int32
		commit            dao.JudgedCommit
		wantAttempt       int32
		wantFailedBit     int32
		wantErrorNonEmpty bool
	}{
		{
			name:          "full success resets attempt_count and clears failed bit",
			startAttempt:  2,
			commit:        dao.JudgedCommit{FailedProducers: 0},
			wantAttempt:   0,
			wantFailedBit: 0,
		},
		{
			name:              "counted producer failure increments and stamps the bit",
			startAttempt:      1,
			commit:            dao.JudgedCommit{FailedProducers: 2, IncrementAttempt: true, LastError: &boom},
			wantAttempt:       2,
			wantFailedBit:     2,
			wantErrorNonEmpty: true,
		},
		{
			name:              "outage producer failure leaves attempt_count unchanged",
			startAttempt:      1,
			commit:            dao.JudgedCommit{FailedProducers: 4, IncrementAttempt: false, LastError: &outage},
			wantAttempt:       1,
			wantFailedBit:     4,
			wantErrorNonEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestTx(t)
			orgID := seedOrg(t, tx)
			ctx := t.Context()
			repo := dao.NewSessionAnalysisStateRepo(tx)
			rawRecordID := uuid.NewString()
			token := uuid.NewString()

			// A SPINE_DONE row with a known claim token and the target attempt_count, ready for the judge.
			insertState(t, tx, &model.SessionAnalysisState{
				OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
				Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SPINE_DONE, VocabularyVersion: 2,
				AttemptCount: tt.startAttempt, ClaimToken: &token,
			})

			tt.commit.VocabularyVersion = 2
			judgedOK, err := repo.TransitionToJudged(ctx, rawRecordID, token, 0, tt.commit)
			require.NoError(t, err)
			require.True(t, judgedOK)

			got, err := repo.GetByRawRecordID(ctx, rawRecordID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantAttempt, got.AttemptCount, "attempt_count")
			assert.Equal(t, tt.wantFailedBit, got.FailedProducers, "failed_producers bit")
			if tt.wantErrorNonEmpty {
				require.NotNil(t, got.LastError)
				assert.NotEmpty(t, *got.LastError)
			} else {
				assert.Nil(t, got.LastError, "a full-success judge clears last_error")
			}
		})
	}
}

// TestSessionAnalysisStateRepo_TransitionToFailedReportsAttemptCount asserts the transition
// returns the row's attempt_count after the update — incremented on a normal failure, unchanged
// under an outage (incrementAttempt=false). markFailed gates the terminal JUDGE_FAILED serving
// stamp on this value (>= RetryCap), so it must reflect the persisted count.
func TestSessionAnalysisStateRepo_TransitionToFailedReportsAttemptCount(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tests := []struct {
		name         string
		startAttempt int32
		increment    bool
		wantAttempt  int32
	}{
		{name: "a normal failure increments and reports the new count", startAttempt: 2, increment: true, wantAttempt: 3},
		{name: "an outage failure does not increment and reports the unchanged count", startAttempt: 2, increment: false, wantAttempt: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestTx(t)
			orgID := seedOrg(t, tx)
			ctx := t.Context()
			repo := dao.NewSessionAnalysisStateRepo(tx)
			rawRecordID := seedSessionRaw(t, tx, orgID, "sha-x")
			token := uuid.NewString()
			person := uuid.NewString()
			insertState(t, tx, &model.SessionAnalysisState{
				OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
				Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING, VocabularyVersion: 2,
				PersonID: &person, ClaimToken: &token, AttemptCount: tt.startAttempt,
			})

			ok, gotAttempt, err := repo.TransitionToFailed(ctx, rawRecordID, token, "boom", tt.increment, "sha-x", sessionTestDate)
			require.NoError(t, err)
			require.True(t, ok, "the claim token matches, so the transition updates the row")
			assert.Equal(t, tt.wantAttempt, gotAttempt, "the reported attempt_count matches the post-update value")

			got, err := repo.GetByRawRecordID(ctx, rawRecordID)
			require.NoError(t, err)
			assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, got.Status)
			assert.Equal(t, tt.wantAttempt, got.AttemptCount, "the reported count equals the persisted count")
		})
	}
}

// TestSessionAnalysisStateRepo_SeatGrantedReadmitOnMetadataPersonCorrection guards the cat-5
// re-admit keying on the CURRENT raw-record metadata person (Codex P1): a NO_SEAT skip whose
// re-upload corrected metadata.person_id from an unseated to a seated identity must re-admit even
// though the state row's person_id is still the stale (unseated) one; a row whose corrected person
// is still unseated stays skipped.
func TestSessionAnalysisStateRepo_SeatGrantedReadmitOnMetadataPersonCorrection(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	noSeat := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT

	seated := uuid.NewString()
	unseated := uuid.NewString()

	// Corrected: state.person_id is the stale unseated id; rr.metadata.person_id is now the seated
	// id → cat-5 must re-admit.
	rawCorrected := seedSessionRaw(t, tx, orgID, "sha-a")
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"person_id":"`+seated+`"}`, rawCorrected).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawCorrected, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &noSeat, PersonID: &unseated,
	})

	// Still unseated after correction → must NOT re-admit.
	stillUnseated := uuid.NewString()
	rawStill := seedSessionRaw(t, tx, orgID, "sha-b")
	require.NoError(t, tx.Exec(`UPDATE raw_records SET metadata = ?::jsonb WHERE id = ?`,
		`{"person_id":"`+stillUnseated+`"}`, rawStill).Error)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawStill, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &noSeat, PersonID: &unseated,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID: orgID, ConnectorID: scanConnectorID, Seats: []string{seated}, SeatCapEnabled: true,
	}
	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)

	cats := candidateCategories(live)
	assert.Equal(t, dao.CategorySeatGranted, cats[rawCorrected], "a metadata person_id corrected to a seated identity re-admits")
	assert.NotContains(t, cats, rawStill, "a row whose corrected person is still unseated stays skipped")
}

// TestSessionAnalysisStateRepo_ActionableScan_SeatCapInactiveFailsOpen asserts the
// tri-state gate's resolved-inactive branch: with SeatCapEnabled=false a
// NO_SEAT-skipped row re-admits even though its person is not in the (empty) seat
// set — fail-open, serve every row.
func TestSessionAnalysisStateRepo_ActionableScan_SeatCapInactiveFailsOpen(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	rawSeat := seedSessionRaw(t, tx, orgID, "sha-x")
	unseated := uuid.NewString()
	noSeat := sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawSeat, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, VocabularyVersion: 2,
		SkipReason: &noSeat, PersonID: &unseated,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID: orgID, ConnectorID: scanConnectorID, RetryCap: 3,
		Seats: nil, SeatCapEnabled: false, // resolved-inactive → fail open
	}
	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, rawSeat, live[0].RawRecordID)
	assert.Equal(t, dao.CategorySeatGranted, live[0].Category)
}

// TestSessionAnalysisStateRepo_ActionableScan_LegacyUpdatedAtStaleness covers the
// category-3 legacy branch: a session row with no payload
// content_sha256 uses raw_records.updated_at against the spine-stamped source_updated_at as
// its change signal. A moved updated_at re-admits; an unchanged one does not; a hash-bearing
// row ignores updated_at entirely; a legacy row with no stored baseline re-admits once to
// establish it; and the compat processing_status write (which never bumps updated_at) does not
// falsely re-admit a stable legacy row.
func TestSessionAnalysisStateRepo_ActionableScan_LegacyUpdatedAtStaleness(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	baseline := sessionTestDate.Add(-72 * time.Hour)
	moved := sessionTestDate.Add(-1 * time.Hour) // a later re-upload bumped updated_at

	judgedLegacy := func(rawRecordID string, sourceUpdatedAt *time.Time) {
		insertState(t, tx, &model.SessionAnalysisState{
			OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
			Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
			SourceUpdatedAt: sourceUpdatedAt, // no SourceContentSha256: this is a legacy generation
		})
	}

	// A legacy body re-uploaded since the last judge: updated_at (moved) != stored baseline.
	rawMoved := seedSessionRawLegacy(t, tx, orgID, moved)
	judgedLegacy(rawMoved, &baseline)

	// A legacy body unchanged since the last judge: updated_at == stored baseline.
	rawStable := seedSessionRawLegacy(t, tx, orgID, baseline)
	judgedLegacy(rawStable, &baseline)

	// A hash-bearing row whose updated_at moved but whose content_sha256 still matches the
	// stored digest — the hash path must ignore updated_at and NOT re-admit it.
	rawHash := seedSessionRaw(t, tx, orgID, "sha-stable")
	require.NoError(t, tx.Exec("UPDATE raw_records SET updated_at = ? WHERE id = ?", moved.UTC(), rawHash).Error)
	stableSha := "sha-stable"
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawHash, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, VocabularyVersion: 2,
		SourceContentSha256: &stableSha, SourceUpdatedAt: &baseline,
	})

	// A legacy row whose baseline was never stamped (SourceUpdatedAt NULL — a row that
	// predates the column): re-admitted ONCE, since updated_at IS DISTINCT FROM NULL.
	rawNilBaseline := seedSessionRawLegacy(t, tx, orgID, baseline)
	judgedLegacy(rawNilBaseline, nil)

	params := dao.SessionAnalysisScanParams{
		OrgID:          orgID,
		ConnectorID:    scanConnectorID,
		LiveWatermark:  time.Time{},
		RetryCap:       3,
		ThrottleCutoff: sessionTestDate.Add(time.Hour), // last_succeeded_at (NULL) < cutoff → throttle passes
	}
	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)
	cats := candidateCategories(live)

	assert.Equal(t, dao.CategoryContentChanged, cats[rawMoved], "a legacy row whose updated_at moved is re-admitted")
	assert.NotContains(t, cats, rawStable, "a legacy row with an unchanged updated_at is not re-admitted")
	assert.NotContains(t, cats, rawHash, "a hash-bearing row whose sha matches is unaffected by a moved updated_at")
	assert.Equal(t, dao.CategoryContentChanged, cats[rawNilBaseline], "a legacy row with no stored baseline re-admits once")

	// The compat processing_status write must not falsely re-admit the stable legacy row: it
	// stamps processing_status alone and never bumps updated_at, so the row stays out of scope.
	require.NoError(t, dao.NewRawRecordRepo(tx).StampProcessingStatusOnly(ctx, rawStable, orgID, sessionTestDate, model.ProcessingStatusProcessed))
	liveAfter, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)
	assert.NotContains(t, candidateCategories(liveAfter), rawStable, "a compat-stamped legacy row is NOT falsely re-admitted (compat never bumps updated_at)")
}

// TestSessionAnalysisStateRepo_ActionableScan_FailedContentChangeReadmit guards the FAILED-at-cap
// stranding fix: a corrected re-upload of a body that failed to the retry cap must reprocess (the
// design resets attempt_count on a content-change re-admit — that reset only matters for a FAILED
// row), while a FAILED row whose spine never stamped a baseline must NOT churn.
func TestSessionAnalysisStateRepo_ActionableScan_FailedContentChangeReadmit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)

	// A FAILED-at-cap, hash-bearing row whose body was corrected (payload sha now differs from the
	// spine-stamped baseline). It must re-admit as CONTENT_CHANGED despite being at the cap.
	oldSha := "sha-old"
	rawFailedChanged := seedSessionRaw(t, tx, orgID, "sha-new")
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawFailedChanged, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, VocabularyVersion: 2,
		SourceContentSha256: &oldSha, AttemptCount: 3, // at the cap
	})

	// A FAILED row whose spine never ran, so no baseline was stamped (SourceContentSha256 NULL). The
	// content-change signal cannot be distinguished from "no baseline", so it must NOT re-admit — else
	// cat-3's attempt_count reset would restart the failing spine every tick (churn).
	rawFailedNoBaseline := seedSessionRaw(t, tx, orgID, "sha-any")
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawFailedNoBaseline, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, VocabularyVersion: 2,
		AttemptCount: 3, // at the cap, no SourceContentSha256
	})

	// A FAILED-at-cap row whose body is UNCHANGED (payload sha matches the baseline) must stay
	// terminal — neither retryable (at cap) nor content-changed.
	sameSha := "sha-same"
	rawFailedUnchanged := seedSessionRaw(t, tx, orgID, sameSha)
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawFailedUnchanged, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, VocabularyVersion: 2,
		SourceContentSha256: &sameSha, AttemptCount: 3,
	})

	params := dao.SessionAnalysisScanParams{
		OrgID:          orgID,
		ConnectorID:    scanConnectorID,
		LiveWatermark:  time.Time{},
		RetryCap:       3,
		ThrottleCutoff: sessionTestDate.Add(time.Hour),
	}
	live, err := repo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)
	cats := candidateCategories(live)

	assert.Equal(t, dao.CategoryContentChanged, cats[rawFailedChanged], "a FAILED-at-cap row with a corrected body re-admits")
	assert.NotContains(t, cats, rawFailedNoBaseline, "a FAILED row with no stamped baseline does not churn re-admission")
	assert.NotContains(t, cats, rawFailedUnchanged, "a FAILED-at-cap row with an unchanged body stays terminal")
}

// TestSessionAnalysisStateRepo_DoubleClaim is the double-claim backstop: two ticks pick the same never-analysed raw record; the ON CONFLICT DO
// NOTHING claim plus the live-generation partial unique mean exactly one inserts.
// The second claim reports claimed=false and no row is duplicated.
func TestSessionAnalysisStateRepo_DoubleClaim(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	rawRecordID := uuid.NewString()

	newClaim := func() *model.SessionAnalysisState {
		return &model.SessionAnalysisState{
			OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
			VocabularyVersion: 2,
		}
	}

	first, err := repo.ClaimForProcessing(ctx, newClaim())
	require.NoError(t, err)
	assert.True(t, first, "the first tick claims the row")

	second, err := repo.ClaimForProcessing(ctx, newClaim())
	require.NoError(t, err)
	assert.False(t, second, "the second tick finds the row already claimed and inserts nothing")

	var count int64
	require.NoError(t, tx.Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ?", rawRecordID).Count(&count).Error)
	assert.Equal(t, int64(1), count, "exactly one live state row exists")
}

// TestSessionAnalysisStateRepo_ReclaimRace is the reclaim-race guardrail
// : two cron deliveries scanning the same failed row both
// read the same claim_token, then both call Reclaim with it. The token CAS means only
// the first wins (it rotates the token); the second's expected token no longer matches
// and it re-claims nothing — so a failed/changed/expired row is never S3/LLM-processed
// twice in one tick.
func TestSessionAnalysisStateRepo_ReclaimRace(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	rawRecordID := uuid.NewString()

	// A FAILED row carrying a claim token — the token both racing ticks read at scan.
	scannedToken := uuid.NewString()
	insertState(t, tx, &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
		Status: sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, VocabularyVersion: 2,
		ClaimToken: &scannedToken, AttemptCount: 1,
	})

	first, err := repo.Reclaim(ctx, rawRecordID, &scannedToken, false)
	require.NoError(t, err)
	assert.True(t, first, "the first tick wins the re-claim and rotates the token")

	second, err := repo.Reclaim(ctx, rawRecordID, &scannedToken, false)
	require.NoError(t, err)
	assert.False(t, second, "the second tick, holding the stale scanned token, re-claims nothing")
}

// TestSessionAnalysisStateRepo_ClaimSpineJudgedCASChain walks the happy-path CAS
// chain (claim → spine_done → judged) and asserts the durable staged-commit contract
// : the judged transition is CAS-gated on the claim_token (lease) ALONE, so a child
// sighted mid-flight (children_seq advanced) no longer blocks R from reaching JUDGED; the unfolded
// delta (children_seq > folded_children_seq) is left for the child-refold leg to re-admit.
func TestSessionAnalysisStateRepo_ClaimSpineJudgedCASChain(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	rawRecordID := uuid.NewString()

	state := &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
		VocabularyVersion: 2,
	}
	claimed, err := repo.ClaimForProcessing(ctx, state)
	require.NoError(t, err)
	require.True(t, claimed)
	token := *state.ClaimToken

	// The spine parses a conversation instant that differs from the data_date-seeded claim
	// value (near-midnight/legacy case); spine_done must reconcile the state column to it so
	// the reprocess window filter reads the authoritative timestamp.
	parsedConversationAt := sessionTestDate.Add(26 * time.Hour)
	spineOK, err := repo.TransitionToSpineDone(ctx, rawRecordID, token, "sha-body", sessionTestDate, 1, parsedConversationAt)
	require.NoError(t, err)
	assert.True(t, spineOK, "spine_done CAS matches the live claim token")

	afterSpine, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.WithinDuration(t, parsedConversationAt, afterSpine.ConversationStartedAt, time.Second,
		"spine_done reconciles conversation_started_at to the parsed instant, not the data_date seed")

	// A child sighting bumps children_seq to 1 while R is in flight. Under the durable staged
	// commit this no longer fences the judged transition — R must still land JUDGED.
	bumped, err := repo.BumpChildrenSeq(ctx, rawRecordID)
	require.NoError(t, err)
	require.True(t, bumped)

	commit := dao.JudgedCommit{VocabularyVersion: 2, Disposition: sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED}
	// This pass folded no child (folded_children_seq=0). The commit lands under the lease alone
	// despite children_seq having advanced to 1 — the child fence is gone.
	judgedOK, err := repo.TransitionToJudged(ctx, rawRecordID, token, 0 /* folded */, commit)
	require.NoError(t, err)
	assert.True(t, judgedOK, "R reaches JUDGED under the lease alone; a mid-flight child no longer blocks it")

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, got.Status)
	require.NotNil(t, got.LastSucceededAt)
	// children_seq (1) > folded_children_seq (0): the unfolded delta is left for the child-refold
	// leg to re-admit the now-JUDGED parent and fold it, carrying R by rehydration.
	assert.Greater(t, got.ChildrenSeq, got.FoldedChildrenSeq,
		"an unfolded child remains, to be picked up by the child-refold leg")
}

// TestSessionAnalysisStateRepo_StaleTokenCannotCommit is the double-commit backstop
// : a re-claim rotates claim_token, so a lease-expired-but-alive worker
// holding the old token cannot land its spine/judged transition after the re-claim.
func TestSessionAnalysisStateRepo_StaleTokenCannotCommit(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := t.Context()
	repo := dao.NewSessionAnalysisStateRepo(tx)
	rawRecordID := uuid.NewString()

	state := &model.SessionAnalysisState{
		OrganizationID: orgID, RawRecordID: rawRecordID, ConversationStartedAt: sessionTestDate,
		VocabularyVersion: 2,
	}
	require.NoError(t, mustClaim(ctx, repo, state))
	staleToken := *state.ClaimToken

	// Another tick re-claims (the original's lease expired), rotating the token. It
	// CAS-matches the current (stale) token the re-claimer read at scan.
	reclaimed, err := repo.Reclaim(ctx, rawRecordID, &staleToken, false)
	require.NoError(t, err)
	require.True(t, reclaimed)

	ok, err := repo.TransitionToSpineDone(ctx, rawRecordID, staleToken, "sha-body", sessionTestDate, 1, sessionTestDate)
	require.NoError(t, err)
	assert.False(t, ok, "the stale-token worker's commit is rejected after a re-claim rotated the token")
}

func mustClaim(ctx context.Context, repo *dao.SessionAnalysisStateRepo, state *model.SessionAnalysisState) error {
	claimed, err := repo.ClaimForProcessing(ctx, state)
	if err != nil {
		return err
	}
	if !claimed {
		return assert.AnError
	}
	return nil
}

// The LIVE no-state leg admits a delegated child like a parent, so applySkip can price it
// (StampChildCost) and fold it into its parent.
// childRawRecordMetadata builds the metadata JSON a delegated child carries; costUsdMicros/priced
// mirrors production (StampChildCost writes a JSON number, never present until a processor prices
// the child).
func childRawRecordMetadata(t *testing.T, relationType, personID, parentRecordKey string, costUsdMicros int64, priced bool) json.RawMessage {
	t.Helper()
	fields := map[string]any{"relation_type": relationType}
	if personID != "" {
		fields["person_id"] = personID
	}
	if parentRecordKey != "" {
		fields["parent_record_key"] = parentRecordKey
	}
	if priced {
		fields["cost_usd_micros"] = costUsdMicros
	}
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return b
}

// seedRawRecordWithMetadata inserts a raw_record carrying explicit metadata (a delegated
// child's relation_type/person_id/parent_record_key/cost_usd_micros — seedSessionRaw's
// fixed metadata cannot express this). An empty recordKey gets a random one; the payload
// carries contentSha as content_sha256.
func seedRawRecordWithMetadata(t *testing.T, db *gorm.DB, orgID, recordKey, contentSha string, metadata json.RawMessage) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"s3_key": "s3://k/" + uuid.NewString(), "content_sha256": contentSha})
	require.NoError(t, err)
	if recordKey == "" {
		recordKey = uuid.NewString()
	}
	rec := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      scanConnectorID,
		OperationID:      "push_conversations",
		RecordKey:        recordKey,
		DataDate:         sessionTestDate,
		Payload:          payload,
		Metadata:         metadata,
		FetchedAt:        sessionTestDate,
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, db.Create(rec).Error)
	return rec.ID
}

// TestSessionAnalysisStateRepo_NoStateLegAdmitsChildrenLikeParents is the child-admission
// guardrail: the LIVE cat-1 no-state leg admits a first-sighted
// delegated child exactly like a parent, unfiltered by relation_type — a child's ONLY admission
// path to applySkip's pricing/fence-bump branch (StampChildCost), so it must never be filtered
// out here (see the comment on ScanLiveActionable's cat-1 leg; excluding children broke two
// pre-existing pipeline e2e tests when tried). Both a priced and an unpriced child surface as
// no-state candidates; the per-record path then prices and folds each. (The pre-cutover priced
// backlog once accelerated by a bulk sweep is now carried by a backfill job, so there is no
// scan-level child filter left to compose against.)
func TestSessionAnalysisStateRepo_NoStateLegAdmitsChildrenLikeParents(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	orgID := seedOrg(t, testDB)
	t.Cleanup(func() {
		cleanupSessionRows(orgID)
		_ = testDB.Session(&gorm.Session{}).Exec("DELETE FROM raw_records WHERE organization_id = ?::uuid", orgID).Error
	})
	ctx := context.Background()
	stateRepo := dao.NewSessionAnalysisStateRepo(testDB)

	rawParent := seedSessionRaw(t, testDB, orgID, "sha-parent")
	rawPricedChild := seedRawRecordWithMetadata(t, testDB, orgID, "", "sha-priced-child",
		childRawRecordMetadata(t, "SUBAGENT", uuid.NewString(), "", 500000, true))
	rawUnpricedChild := seedRawRecordWithMetadata(t, testDB, orgID, "", "sha-unpriced-child",
		childRawRecordMetadata(t, "PLUGIN", uuid.NewString(), "", 0, false))

	params := dao.SessionAnalysisScanParams{OrgID: orgID, ConnectorID: scanConnectorID}
	live, err := stateRepo.ScanLiveActionable(ctx, params, 100)
	require.NoError(t, err)

	cats := candidateCategories(live)
	assert.Equal(t, dao.CategoryNoState, cats[rawParent], "the parent drains as a normal no-state candidate")
	assert.Equal(t, dao.CategoryNoState, cats[rawPricedChild], "a priced child is admitted like a parent — its only path to a state row")
	assert.Equal(t, dao.CategoryNoState, cats[rawUnpricedChild], "an unpriced child is admitted — its only path to being priced")
}
