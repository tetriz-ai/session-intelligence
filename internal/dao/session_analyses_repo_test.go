// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for SessionAnalysisRepo. They exercise the GORM SQL a
// caller-side mock cannot reach: the RETURNING insert through planscoped.WriteDB,
// the live-generation GetByRawRecordID read (superseded_at IS NULL) through the
// seat-scoped planscoped.DB, and the batched frozen-org purge.
//
// session_analyses is seat-scoped on person_id, so at least one read runs under a
// real seat context (loopTestCtx = planscoped.WithConfigForTest + WithSeats), not
// context.Background() (dao-repo-tests rule 4). True cross-org RLS isolation is
// covered by session_analysis_rls_test.go (rls_test_user); the default test
// connection is superuser and bypasses RLS, so these repo-level cases assert the
// method's own predicate scoping (raw_record_id key + the seat IN-clause).

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// sessionTestDate is the conversation anchor for the session-engine repo tests,
// kept inside the wide loopTestCtx data window so a date predicate is never the
// reason a row is missing.
var sessionTestDate = time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)

// newSessionAnalysis builds a live (non-superseded) session-analysis row carrying a
// proto-enum text column (ide_type / work_type) and a JSONB proto-message column
// (token_breakdown) so a read-back can prove both round-trip through Postgres.
func newSessionAnalysis(orgID, personID, rawRecordID string) *model.SessionAnalysis {
	ide := commonv1.IdeType_IDE_TYPE_CLAUDE_CODE
	work := sessionintelligencepb.PhaseTheme_PHASE_THEME_CREATION
	return &model.SessionAnalysis{
		OrganizationID:        orgID,
		PersonID:              personID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		VocabularyVersion:     1,
		IdeType:               &ide,
		WorkType:              &work,
		TokenBreakdown: &sessionintelligencepb.AnalysisTokenBreakdown{
			InputTokens:  1200,
			OutputTokens: 340,
			TotalTokens:  1540,
		},
	}
}

func TestSessionAnalysisRepo_CreateAndGetByRawRecordID(t *testing.T) {
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

	analysis := newSessionAnalysis(orgID, person, rawRecordID)
	require.NoError(t, repo.Create(ctx, analysis))
	assert.NotEmpty(t, analysis.ID, "Create must stamp a generated id")

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Equal(t, analysis.ID, got.ID)
	assert.Equal(t, orgID, got.OrganizationID)
	assert.Equal(t, person, got.PersonID)
	assert.Equal(t, int32(1), got.VocabularyVersion)
	// Proto-enum text column round-trips as the enum name.
	require.NotNil(t, got.IdeType)
	assert.Equal(t, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, *got.IdeType)
	require.NotNil(t, got.WorkType)
	assert.Equal(t, sessionintelligencepb.PhaseTheme_PHASE_THEME_CREATION, *got.WorkType)
	// JSONB proto-message column round-trips field-for-field.
	require.NotNil(t, got.TokenBreakdown)
	assert.Equal(t, int64(1200), got.TokenBreakdown.InputTokens)
	assert.Equal(t, int64(340), got.TokenBreakdown.OutputTokens)
	assert.Equal(t, int64(1540), got.TokenBreakdown.TotalTokens)

	missing, err := repo.GetByRawRecordID(ctx, uuid.NewString())
	require.ErrorIs(t, err, dao.ErrNotFound)
	assert.Nil(t, missing)
}

// TestSessionAnalysisRepo_CopyUpsert pins CopyUpsert's ON CONFLICT contract — the
// "live generation always wins" invariant the backfill depends on. The DO UPDATE fires
// only WHERE origin = SESSION_COPY, so a copy re-copies its own frozen row but can NEVER clobber
// a live (origin NULL) content row. Case 3 is the guard: reverting the origin WHERE clause lets a
// copy update the live row (RowsAffected 1), which this test forbids.

// TestSessionAnalysisRepo_ReadinessColumnsAreIndependent pins the three per-half readiness
// columns' independence: E's artifact half can sit
// PENDING while its round-trip half is already PRODUCED, and D's own readiness is a third,
// independent axis — a single evidence_disposition scalar could not represent this mixed
// state, so the row must carry (and round-trip) all three separately.
func TestSessionAnalysisRepo_ReadinessColumnsAreIndependent(t *testing.T) {
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

	analysis := newSessionAnalysis(orgID, person, rawRecordID)
	artifactPending := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_SKIPPED_PENDING_DATA
	roundtripProduced := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED
	discardProduced := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED
	analysis.EvidenceArtifactDisposition = &artifactPending
	analysis.EvidenceRoundtripDisposition = &roundtripProduced
	analysis.DiscardDisposition = &discardProduced
	require.NoError(t, repo.Create(ctx, analysis))

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)

	require.NotNil(t, got.EvidenceArtifactDisposition)
	assert.Equal(t, sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_SKIPPED_PENDING_DATA, *got.EvidenceArtifactDisposition,
		"E's artifact half is still pending")

	require.NotNil(t, got.EvidenceRoundtripDisposition)
	assert.Equal(t, sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED, *got.EvidenceRoundtripDisposition,
		"E's round-trip half committed independently of the pending artifact half")

	require.NotNil(t, got.DiscardDisposition)
	assert.Equal(t, sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED, *got.DiscardDisposition,
		"D's readiness is a third, independent axis")
}

// TestSessionAnalysisRepo_UpsertJudged_PreservesEvidenceOnReJudgeWithoutEvidence is the
// last-E-owned-home guardrail: skill_assessments and
// artifact_adjudications are filled by applyEvidence ONLY when E fires, so an R-only
// re-judge (evidenceRan=false, analysis carries nil in both columns — buildSessionAnalysis
// never sets them when E did not run) must NOT NULL out a prior generation's valid E
// output — UpsertJudged carries the prior E-owned columns forward across its UpdateAll. An
// E-ran re-judge (evidenceRan=true) with fresh values is the control: its own output must
// win, not the carried-forward prior.
func TestSessionAnalysisRepo_UpsertJudged_PreservesEvidenceOnReJudgeWithoutEvidence(t *testing.T) {
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

	// First judged commit: E fired and persisted its two owned columns.
	firstGen := newSessionAnalysis(orgID, person, rawRecordID)
	firstGen.SkillAssessments = &sessionintelligencepb.InvokedSkillJudgmentList{Items: []*sessionintelligencepb.InvokedSkillJudgment{
		{InvocationRef: "inv-1", ArtifactName: "code-review", SkillVerdict: sessionintelligencepb.SkillVerdict_SKILL_VERDICT_ADEQUATE},
	}}
	firstGen.ArtifactAdjudications = &sessionintelligencepb.ArtifactAdjudicationList{Items: []*sessionintelligencepb.ArtifactAdjudication{
		{ArtifactRef: "pr-1", OverallAttribution: sessionintelligencepb.OverallAttribution_OVERALL_ATTRIBUTION_ARTIFACT_SKILL},
	}}
	require.NoError(t, repo.UpsertJudged(ctx, firstGen, true))

	// An R-only re-judge: E did NOT run this pass, so the built analysis carries nil in
	// both E-owned columns — exactly what buildSessionAnalysis produces when read.EvidenceRan
	// is false.
	rOnly := &model.SessionAnalysis{
		OrganizationID: orgID, PersonID: person, RawRecordID: rawRecordID,
		ConversationStartedAt: sessionTestDate, VocabularyVersion: 1,
	}
	goal := sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY
	rOnly.GoalAchieved = &goal
	require.NoError(t, repo.UpsertJudged(ctx, rOnly, false))

	afterROnly, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Equal(t, sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY, *afterROnly.GoalAchieved, "R's fresh verdict is written")
	// Without the fix this read-back would be NULL — the mutant this test kills.
	require.NotNil(t, afterROnly.SkillAssessments, "E's prior skill_assessments must survive an R-only re-judge")
	require.Len(t, afterROnly.SkillAssessments.GetItems(), 1)
	assert.Equal(t, "inv-1", afterROnly.SkillAssessments.GetItems()[0].GetInvocationRef())
	require.NotNil(t, afterROnly.ArtifactAdjudications, "E's prior artifact_adjudications must survive an R-only re-judge")
	require.Len(t, afterROnly.ArtifactAdjudications.GetItems(), 1)
	assert.Equal(t, "pr-1", afterROnly.ArtifactAdjudications.GetItems()[0].GetArtifactRef())

	// A subsequent E-ran re-judge with fresh values overwrites the carried-forward prior —
	// E's own re-evaluation is authoritative once it actually runs.
	eRan := &model.SessionAnalysis{
		OrganizationID: orgID, PersonID: person, RawRecordID: rawRecordID,
		ConversationStartedAt: sessionTestDate, VocabularyVersion: 1,
	}
	eRan.SkillAssessments = &sessionintelligencepb.InvokedSkillJudgmentList{Items: []*sessionintelligencepb.InvokedSkillJudgment{
		{InvocationRef: "inv-2", ArtifactName: "test-writer", SkillVerdict: sessionintelligencepb.SkillVerdict_SKILL_VERDICT_INADEQUATE},
	}}
	eRan.ArtifactAdjudications = &sessionintelligencepb.ArtifactAdjudicationList{Items: []*sessionintelligencepb.ArtifactAdjudication{
		{ArtifactRef: "pr-2", OverallAttribution: sessionintelligencepb.OverallAttribution_OVERALL_ATTRIBUTION_REVIEW_SKILL},
	}}
	require.NoError(t, repo.UpsertJudged(ctx, eRan, true))

	afterERan, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, afterERan.SkillAssessments.GetItems(), 1)
	assert.Equal(t, "inv-2", afterERan.SkillAssessments.GetItems()[0].GetInvocationRef(), "E's fresh re-evaluation wins over the carried-forward prior")
	require.Len(t, afterERan.ArtifactAdjudications.GetItems(), 1)
	assert.Equal(t, "pr-2", afterERan.ArtifactAdjudications.GetItems()[0].GetArtifactRef())
}

// TestSessionAnalysisRepo_UpsertJudged_PreservesForeignWriterColumnOnReJudge pins the list-drift
// guard: a session_analyses column owned by NO current writer (segmentation_suspect) must survive a
// re-judge of the live generation, because UpsertJudged refreshes only the pipeline-owned column
// set (sessionAnalysisJudgedUpsertColumns), never a blind UpdateAll. Reverting UpsertJudged to
// UpdateAll:true nulls segmentation_suspect here — the mutant this kills.
func TestSessionAnalysisRepo_UpsertJudged_PreservesForeignWriterColumnOnReJudge(t *testing.T) {
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

	// The judge's first commit — like the real judge, it never writes segmentation_suspect (a column
	// owned by no current writer).
	require.NoError(t, repo.UpsertJudged(ctx, newSessionAnalysis(orgID, person, rawRecordID), true))

	// A different writer stamps segmentation_suspect onto the live row.
	require.NoError(t, tx.Model(&model.SessionAnalysis{}).
		Where("raw_record_id = ?", rawRecordID).
		Update("segmentation_suspect", true).Error)

	// A re-judge of the same live generation — buildSessionAnalysis carries nil in segmentation_suspect.
	reJudge := &model.SessionAnalysis{
		OrganizationID: orgID, PersonID: person, RawRecordID: rawRecordID,
		ConversationStartedAt: sessionTestDate, VocabularyVersion: 1,
	}
	require.NoError(t, repo.UpsertJudged(ctx, reJudge, true))

	after, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.NotNil(t, after.SegmentationSuspect, "a different writer's segmentation_suspect must survive a re-judge")
	assert.True(t, *after.SegmentationSuspect)
}

// TestSessionAnalysisRepo_UpsertMeasured_PreservesJudged is the spine-only-preserves-
// judged guardrail: a measured-only refresh (the no-LLM
// spine-version-bump path) must update the measured columns WITHOUT clobbering the
// prior generation's judged verdict — the fix for UpsertJudged's UpdateAll NULLing the
// verdict on a read==nil commit.
func TestSessionAnalysisRepo_UpsertMeasured_PreservesJudged(t *testing.T) {
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

	// The first full judged commit: a real verdict plus a measured value.
	judged := newSessionAnalysis(orgID, person, rawRecordID)
	goal := sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY
	judged.GoalAchieved = &goal
	firstPrompts := int32(5)
	judged.TotalPrompts = &firstPrompts
	require.NoError(t, repo.UpsertJudged(ctx, judged, true))

	// A spine-only measured correction: a new measured value, NO verdict.
	measured := &model.SessionAnalysis{
		OrganizationID:        orgID,
		PersonID:              person,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		VocabularyVersion:     1,
	}
	newPrompts := int32(10)
	measured.TotalPrompts = &newPrompts
	// The folded child usage is a measured column too, so a spine-only child re-fold must
	// refresh it.
	measured.SubagentUsage = &sessionintelligencepb.SubagentUsage{ChildCount: 1, TotalTokens: 1000, TotalEstimatedCostUsd: 5.0}
	require.NoError(t, repo.UpsertMeasured(ctx, measured))

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	// The measured column refreshed.
	require.NotNil(t, got.TotalPrompts)
	assert.Equal(t, int32(10), *got.TotalPrompts, "the measured column is refreshed")
	// The folded child usage refreshed on the measured path.
	require.NotNil(t, got.SubagentUsage, "subagent_usage is a measured column and refreshes on a spine-only re-fold")
	assert.Equal(t, int32(1), got.SubagentUsage.GetChildCount())
	assert.Equal(t, int64(1000), got.SubagentUsage.GetTotalTokens())
	// The judged verdict is intact — NOT NULLed by the measured-only refresh.
	require.NotNil(t, got.GoalAchieved, "the judged verdict must survive a measured-only refresh")
	assert.Equal(t, sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY, *got.GoalAchieved)
	require.NotNil(t, got.WorkType, "the prior judged work_type must survive too")
	assert.Equal(t, sessionintelligencepb.PhaseTheme_PHASE_THEME_CREATION, *got.WorkType)
}

// TestSessionAnalysisRepo_UpsertMeasuredReaffirmingJudged_RestoresClearedJudgedAt is the Codex-P1
// legacy-re-emit guard: a cat-3 legacy reopen clears judged_at (ClearServedVerdict), but the
// identical body takes the spine-only path — so the commit must RE-AFFIRM judged_at (via
// UpsertMeasuredReaffirmingJudged) without touching the carried verdict, else the still-valid
// session is dropped from serving (judged_at NULL) forever.
func TestSessionAnalysisRepo_UpsertMeasuredReaffirmingJudged_RestoresClearedJudgedAt(t *testing.T) {
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

	// A full judged commit: verdict + judged_at.
	judged := newSessionAnalysis(orgID, person, rawRecordID)
	goal := sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY
	judged.GoalAchieved = &goal
	judgedAt := time.Now().UTC()
	judged.JudgedAt = &judgedAt
	require.NoError(t, repo.UpsertJudged(ctx, judged, true))

	// A cat-3 legacy reopen clears the served verdict.
	cleared, err := repo.ClearServedVerdict(ctx, rawRecordID)
	require.NoError(t, err)
	require.True(t, cleared)
	afterClear, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Nil(t, afterClear.JudgedAt, "ClearServedVerdict drops it from serving")

	// The spine-only re-affirm on the unchanged body: refresh a measured value AND restore judged_at.
	reaffirmed := &model.SessionAnalysis{
		OrganizationID: orgID, PersonID: person, RawRecordID: rawRecordID,
		ConversationStartedAt: sessionTestDate, VocabularyVersion: 1,
	}
	newPrompts := int32(9)
	reaffirmed.TotalPrompts = &newPrompts
	reaffirmedAt := time.Now().UTC()
	reaffirmed.JudgedAt = &reaffirmedAt
	require.NoError(t, repo.UpsertMeasuredReaffirmingJudged(ctx, reaffirmed))

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.NotNil(t, got.JudgedAt, "judged_at is restored so the session serves again")
	require.NotNil(t, got.TotalPrompts)
	assert.Equal(t, int32(9), *got.TotalPrompts, "the measured column refreshed")
	require.NotNil(t, got.GoalAchieved, "the carried judged verdict is untouched")
	assert.Equal(t, sessionintelligencepb.GoalAchieved_GOAL_ACHIEVED_FULLY, *got.GoalAchieved)
}

// TestSessionAnalysisRepo_DerivedColumnOwnership pins the persistence and multi-writer ownership
// of the columns the empty-column-population work added — proving they actually round-trip through
// Postgres (not merely compile) and that each lands in its correct ownership bucket across a
// spine-only reaffirm. The MarkJudgeFailed half of the split (weighted_score/phase_count NULLed
// while the judge-written measured aggregates survive) is pinned in the MarkJudgeFailed content-
// reopen case.
func TestSessionAnalysisRepo_DerivedColumnOwnership(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	capFull := sessionintelligencepb.TokenSplitCapability_TOKEN_SPLIT_CAPABILITY_FULL

	t.Run("the derived measured + judged columns persist through UpsertJudged", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx(t)
		orgID := seedOrg(t, tx)
		person := uuid.NewString()
		rawRecordID := uuid.NewString()
		ctx := loopTestCtx(person)
		repo := dao.NewSessionAnalysisRepo(tx)

		analysis := newSessionAnalysis(orgID, person, rawRecordID)
		// Spine-derived measured scalars.
		sessionID, conversationID := "sess-abc", "conv-xyz"
		activeShare := 0.42
		firstUserMsg := sessionTestDate.Add(90 * time.Second)
		lastActive := sessionTestDate.Add(2 * time.Hour)
		analysis.SessionID = &sessionID
		analysis.ConversationID = &conversationID
		analysis.ActiveShare = &activeShare
		analysis.FirstUserMsgTs = &firstUserMsg
		analysis.LastActiveAt = &lastActive
		// R-verdict-derived judged rollups.
		phaseCount, weighted := int32(3), 61.5
		analysis.PhaseCount = &phaseCount
		analysis.WeightedScore = &weighted
		// Judge-written measured aggregates.
		human, delegated, toolCalls := int32(6), int32(2), int32(11)
		analysis.HumanTurnCount = &human
		analysis.DelegatedTurnCount = &delegated
		analysis.ToolCallCount = &toolCalls
		analysis.CapTokenSplit = &capFull
		require.NoError(t, repo.UpsertJudged(ctx, analysis, true))

		got, err := repo.GetByRawRecordID(ctx, rawRecordID)
		require.NoError(t, err)
		require.NotNil(t, got.SessionID)
		assert.Equal(t, sessionID, *got.SessionID)
		require.NotNil(t, got.ConversationID)
		assert.Equal(t, conversationID, *got.ConversationID)
		require.NotNil(t, got.ActiveShare)
		assert.InDelta(t, activeShare, *got.ActiveShare, 1e-9)
		require.NotNil(t, got.FirstUserMsgTs)
		assert.WithinDuration(t, firstUserMsg, *got.FirstUserMsgTs, time.Second)
		require.NotNil(t, got.LastActiveAt)
		assert.WithinDuration(t, lastActive, *got.LastActiveAt, time.Second)
		require.NotNil(t, got.PhaseCount)
		assert.Equal(t, phaseCount, *got.PhaseCount)
		require.NotNil(t, got.WeightedScore)
		assert.InDelta(t, weighted, *got.WeightedScore, 1e-9)
		require.NotNil(t, got.HumanTurnCount)
		assert.Equal(t, human, *got.HumanTurnCount)
		require.NotNil(t, got.DelegatedTurnCount)
		assert.Equal(t, delegated, *got.DelegatedTurnCount)
		require.NotNil(t, got.ToolCallCount)
		assert.Equal(t, toolCalls, *got.ToolCallCount)
		require.NotNil(t, got.CapTokenSplit)
		assert.Equal(t, capFull, *got.CapTokenSplit)
	})

	t.Run("a spine-only reaffirm preserves the judge-owned columns and refreshes the measured ones", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx(t)
		orgID := seedOrg(t, tx)
		person := uuid.NewString()
		rawRecordID := uuid.NewString()
		ctx := loopTestCtx(person)
		repo := dao.NewSessionAnalysisRepo(tx)

		// A full judged commit carrying both the R-verdict rollups and the judge-written measured
		// aggregates, plus a measured column to prove the reaffirm refreshes the measured set.
		judged := newSessionAnalysis(orgID, person, rawRecordID)
		judgedAt := sessionTestDate.Add(time.Hour)
		phaseCount, weighted := int32(4), 88.0
		human, delegated, toolCalls := int32(7), int32(3), int32(20)
		firstPrompts := int32(5)
		judged.JudgedAt = &judgedAt
		judged.PhaseCount = &phaseCount
		judged.WeightedScore = &weighted
		judged.HumanTurnCount = &human
		judged.DelegatedTurnCount = &delegated
		judged.ToolCallCount = &toolCalls
		judged.CapTokenSplit = &capFull
		judged.TotalPrompts = &firstPrompts
		require.NoError(t, repo.UpsertJudged(ctx, judged, true))

		// A spine-only reaffirm carries the measured set only (no judge columns); it must restore
		// judged_at (the reaffirm's purpose) and refresh a measured column, but never re-write a
		// judged or judge-written column.
		reaffirm := &model.SessionAnalysis{
			OrganizationID: orgID, PersonID: person, RawRecordID: rawRecordID,
			ConversationStartedAt: sessionTestDate, VocabularyVersion: 1,
		}
		reaffirmedAt := sessionTestDate.Add(2 * time.Hour)
		newPrompts := int32(9)
		reaffirm.JudgedAt = &reaffirmedAt
		reaffirm.TotalPrompts = &newPrompts
		require.NoError(t, repo.UpsertMeasuredReaffirmingJudged(ctx, reaffirm))

		got, err := repo.GetByRawRecordID(ctx, rawRecordID)
		require.NoError(t, err)
		require.NotNil(t, got.JudgedAt, "reaffirm restores judged_at")
		require.NotNil(t, got.TotalPrompts)
		assert.Equal(t, int32(9), *got.TotalPrompts, "a measured column is refreshed by the reaffirm")
		// R-verdict rollups are judged columns — excluded from the reaffirm set, so preserved.
		require.NotNil(t, got.PhaseCount, "phase_count (judged) survives a spine-only reaffirm")
		assert.Equal(t, phaseCount, *got.PhaseCount)
		require.NotNil(t, got.WeightedScore, "weighted_score (judged) survives a spine-only reaffirm")
		assert.InDelta(t, weighted, *got.WeightedScore, 1e-9)
		// The judge-written measured aggregates are absent from the reaffirm set too — preserved.
		require.NotNil(t, got.HumanTurnCount, "human_turn_count survives a spine-only reaffirm")
		assert.Equal(t, human, *got.HumanTurnCount)
		require.NotNil(t, got.DelegatedTurnCount)
		assert.Equal(t, delegated, *got.DelegatedTurnCount)
		require.NotNil(t, got.ToolCallCount)
		assert.Equal(t, toolCalls, *got.ToolCallCount)
		require.NotNil(t, got.CapTokenSplit)
		assert.Equal(t, capFull, *got.CapTokenSplit)
	})
}

// TestSessionAnalysisRepo_GetByRawRecordID_ExcludesSuperseded guards the
// GetByRawRecordID contract (superseded_at IS NULL): a superseded generation and
// its live replacement coexist under the durable key, and only the live one is
// returned.
func TestSessionAnalysisRepo_GetByRawRecordID_ExcludesSuperseded(t *testing.T) {
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

	// A stale generation: superseded_at set (excluded from GetByRawRecordID and from
	// the session_analyses_one_live partial-unique index).
	superseded := newSessionAnalysis(orgID, person, rawRecordID)
	superseded.VocabularyVersion = 1
	supersededAt := sessionTestDate.Add(time.Hour)
	superseded.SupersededAt = &supersededAt
	require.NoError(t, repo.Create(ctx, superseded))

	// The live generation for the same raw record.
	live := newSessionAnalysis(orgID, person, rawRecordID)
	live.VocabularyVersion = 2
	require.NoError(t, repo.Create(ctx, live))

	got, err := repo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Equal(t, live.ID, got.ID, "GetByRawRecordID must return the live generation")
	assert.Equal(t, int32(2), got.VocabularyVersion)
	assert.Nil(t, got.SupersededAt)
}

// TestSessionAnalysisRepo_Create_DuplicateLiveGenerationErrors asserts Create is
// insert-only: a second live row for the same (organization_id, raw_record_id)
// violates the session_analyses_one_live partial-unique index rather than silently
// updating.
func TestSessionAnalysisRepo_Create_DuplicateLiveGenerationErrors(t *testing.T) {
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

	require.NoError(t, repo.Create(ctx, newSessionAnalysis(orgID, person, rawRecordID)))
	err := repo.Create(ctx, newSessionAnalysis(orgID, person, rawRecordID))
	require.Error(t, err, "a second live generation for the same raw record must be rejected")
}
