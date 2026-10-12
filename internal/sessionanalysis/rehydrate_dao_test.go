// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/testutil"
)

func ptrTo[T any](v T) *T { return &v }

// rehydrateExcludedRFields are the TurnJudgment / SessionVerdict struct fields the R generator
// leaves nil ON PURPOSE — they are E-owned, so a pure-R ReadResult must not carry them and the
// rehydrate reader must not read them. The completeness check below skips exactly these; every
// other field must be filled, so a newly-added R field lands unfilled and fails the check.
var rehydrateExcludedRFields = map[string]struct{}{
	"RoundTripFault": {}, "RoundTripEvidencePromptIndex": {}, // E-owned (session_round_trip_faults)
	"ArtifactAdjudications": {}, // E-owned (session_analyses.artifact_adjudications)
}

// TestRehydrateReadResult_RoundTripThroughDAO is the reprocessing slice's gate
// : rehydrate(read(commit(R))) == R through the REAL DAO,
// so JSONB key reordering, proto-enum text round-trip, null-vs-empty, and timestamp precision at
// the persistence layer are exercised, not bypassed by an in-memory identity check. The R
// generator fills every R-owned field non-trivially (guarded by assertAllRFieldsSet), and the
// column-set-parity test proves every model column is classified — together they stop a newly
// added R field from silently escaping the carry mapper.
func TestRehydrateReadResult_RoundTripThroughDAO(t *testing.T) {
	db, cleanup, ok := testutil.MustOpen()
	if !ok {
		t.Skip("no test DB")
	}
	defer cleanup()
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()

	orgID := uuid.NewString()
	personID := uuid.NewString()
	rawRecordID := uuid.NewString()
	sha := "sha-roundtrip"
	ctx := seatCtx(personID)

	const numTurns, numPhases = 3, 2
	read := generateR(numTurns, numPhases)
	assertAllRFieldsSet(t, read)

	sp := &spineResult{
		conversationStartedAt: time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		contentSha256:         sha,
		vocabularyVersion:     2,
		disposition:           sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED,
		turnCount:             numTurns,
	}
	record := &model.RawRecordModel{ID: rawRecordID, OrganizationID: orgID, DataDate: sp.conversationStartedAt}

	analysisRepo := dao.NewSessionAnalysisRepo(tx)
	turnRepo := dao.NewSessionTurnAnalysisRepo(tx)
	phaseRepo := dao.NewSessionPhaseAnalysisRepo(tx)

	// Commit R exactly as the commit path does: judged content row, then phases (returning the
	// ordinal->id map), then a measured turn row per turn (so the judged overlay has a row to
	// UPDATE) followed by the judged overlay carrying the resolved phase_id.
	analysis := buildSessionAnalysisRow(orgID, record, personID, sp, read, nil, true, buildMeasuredTurnRows(orgID, record.ID, sp))
	require.NoError(t, analysisRepo.UpsertJudged(ctx, analysis, read.EvidenceRan))
	require.NotEmpty(t, analysis.ID)

	ordinalToPhaseID, err := phaseRepo.ReplacePhaseSet(ctx, orgID, analysis.ID, buildPhaseRows(orgID, analysis.ID, sp, read.Phases, buildMeasuredTurnRows(orgID, record.ID, sp)))
	require.NoError(t, err)

	for _, turn := range read.Turns {
		measured := &model.SessionTurnAnalysis{
			OrganizationID:        orgID,
			RawRecordID:           rawRecordID,
			SessionAnalysisID:     &analysis.ID,
			ConversationStartedAt: sp.conversationStartedAt,
			PromptIndex:           turn.PromptIndex,
			// The measured NOT-NULL columns the grouper owns (author / token_fidelity /
			// source_content_sha256) — set minimally so the judged overlay has a row to update.
			Author:              ptrTo("user"),
			IsSubagent:          ptrTo(false),
			TokenFidelity:       ptrTo(sessionintelligencepb.TokenFidelity(1)),
			SourceContentSha256: &sha,
		}
		require.NoError(t, turnRepo.UpsertMeasured(ctx, measured))
		row := buildTurnRow(orgID, rawRecordID, analysis.ID, sp, turn)
		if turn.PhaseOrdinal != nil {
			if phaseID, found := ordinalToPhaseID[*turn.PhaseOrdinal]; found {
				row.PhaseID = &phaseID
			}
		}
		affected, uerr := turnRepo.UpdateJudged(ctx, row)
		require.NoError(t, uerr)
		require.Equal(t, int64(1), affected)
	}

	// Read back through the readers the mapper is specified against, then rehydrate.
	analysisBack, err := analysisRepo.GetByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	turnsBack, err := turnRepo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	phasesBack, err := phaseRepo.ListBySessionAnalysisID(ctx, analysis.ID)
	require.NoError(t, err)

	got := rehydrateReadResult(analysisBack, turnsBack, phasesBack)
	require.NotNil(t, got)

	// R-owned grain must be identical; protocmp handles the proto messages nested in the structs.
	if diff := cmp.Diff(read.Verdict, got.Verdict, protocmp.Transform()); diff != "" {
		t.Errorf("verdict round-trip mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(read.Turns, got.Turns, protocmp.Transform()); diff != "" {
		t.Errorf("turns round-trip mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(read.Phases, got.Phases, protocmp.Transform()); diff != "" {
		t.Errorf("phases round-trip mismatch (-want +got):\n%s", diff)
	}
}

// seatCtx mirrors the dao repo tests' plan-scoped context so the seat-scoped session_analyses
// reads/writes resolve the session's person.
func seatCtx(personIDs ...string) context.Context {
	ctx := planscoped.WithConfigForTest(context.Background(), planscoped.Config{
		Plan:           commonv1.Plan_PLAN_TRIAL,
		State:          commonv1.PlanState_PLAN_STATE_ACTIVE,
		SeatsLimit:     100,
		DataWindowDays: 3650,
		OrgCreatedAt:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	return planscoped.WithSeats(ctx, planscoped.SeatsContext{PersonIDs: personIDs})
}

// generateR builds a ReadResult whose every R-owned field is set non-trivially — indices start
// at 1 so no value-typed index field is a zero (which the completeness check reads as unset).
// E-owned fields (rehydrateExcludedRFields) are deliberately left nil.
func generateR(numTurns, numPhases int) *ReadResult {
	read := &ReadResult{
		Verdict: SessionVerdict{
			SessionSteering:       ptrTo(sessionintelligencepb.SessionSteering(1)),
			GoalAchieved:          ptrTo(sessionintelligencepb.GoalAchieved(1)),
			Smoothness:            ptrTo(sessionintelligencepb.Smoothness(1)),
			FrictionCause:         ptrTo(sessionintelligencepb.FrictionCause(1)),
			Speed:                 ptrTo(sessionintelligencepb.Speed(1)),
			TokenEfficiency:       ptrTo(sessionintelligencepb.TokenEfficiency(1)),
			GoalIsCodingWork:      ptrTo(true),
			UnderlyingGoal:        ptrTo("ship the feature"),
			WorkType:              ptrTo(sessionintelligencepb.PhaseTheme(1)),
			ScoreOverall:          ptrTo(int32(7)),
			ScoreClarity:          ptrTo(int32(6)),
			ScoreSpecificity:      ptrTo(int32(5)),
			ScoreContextProvision: ptrTo(int32(4)),
			ScoreActionability:    ptrTo(int32(8)),
			ScoreCompleteness:     ptrTo(int32(9)),
			ScoreEfficiency:       ptrTo(int32(3)),
			InvokedSkills: &sessionintelligencepb.InvokedSkillJudgmentList{Items: []*sessionintelligencepb.InvokedSkillJudgment{
				{InvocationRef: "s1", ArtifactName: "skill-x", PromptIndex: 1},
			}},
			HarnessFindings: &sessionintelligencepb.HarnessFindingList{Items: []*sessionintelligencepb.HarnessFinding{
				{PreferenceSignal: "prefers-x", InvokedSkill: "skill-x", BothValid: true},
			}},
		},
	}
	for i := 1; i <= numTurns; i++ {
		phaseOrdinal := int32(((i - 1) % numPhases) + 1)
		read.Turns = append(read.Turns, &TurnJudgment{
			PromptIndex:          int32(i),
			PhaseOrdinal:         ptrTo(phaseOrdinal),
			PromptType:           ptrTo(sessionintelligencepb.PromptType(1)),
			UserSteering:         ptrTo(sessionintelligencepb.TurnUserSteering(1)),
			PromptCompleteness:   ptrTo(sessionintelligencepb.PromptCompleteness(1)),
			PromptObjectiveMet:   ptrTo(sessionintelligencepb.PromptObjectiveMet(1)),
			AgentHelpfulness:     ptrTo(sessionintelligencepb.AgentHelpfulness(1)),
			AgentInitiative:      ptrTo(sessionintelligencepb.AgentInitiative(1)),
			ToolUse:              ptrTo(sessionintelligencepb.ToolUse(1)),
			WasRetry:             ptrTo(sessionintelligencepb.WasRetry(1)),
			FailureClass:         ptrTo(sessionintelligencepb.FailureClass(1)),
			TokenLocus:           ptrTo(sessionintelligencepb.TokenLocus(1)),
			WasteCause:           ptrTo(sessionintelligencepb.WasteCause(1)),
			DecisionType:         ptrTo(sessionintelligencepb.DecisionType(1)),
			DownstreamImpact:     ptrTo(sessionintelligencepb.DownstreamImpact(1)),
			FeedbackTarget:       ptrTo(sessionintelligencepb.FeedbackTarget(1)),
			FeedbackDisposition:  ptrTo(sessionintelligencepb.FeedbackDisposition(1)),
			ReferencedArtifact:   ptrTo("example-org/example-repo#7"),
			ReferencedFindingIDs: &sessionintelligencepb.StringList{Values: []string{"f1", "f2"}},
			EvidenceSeq:          ptrTo(int32(i)),
			PromptIssues: &sessionintelligencepb.PromptIssueList{Items: []*sessionintelligencepb.PromptIssue{
				{Slug: sessionintelligencepb.PromptIssueSlug_PROMPT_ISSUE_SLUG_API_KEY_LEAK, Severity: sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH, Layer: "security", Note: "leak"},
			}},
			ChunkIndex: ptrTo(int32(0)),
		})
	}
	for i := 1; i <= numPhases; i++ {
		read.Phases = append(read.Phases, &PhaseJudgment{
			PhaseOrdinal:       int32(i),
			RangeStart:         ptrTo(int32(i)),
			RangeEnd:           ptrTo(int32(i + 1)),
			BoundaryOverridden: ptrTo(true),
			OverrideReason:     ptrTo("merged trivial phases"),
			PhaseTheme:         ptrTo(sessionintelligencepb.PhaseTheme(1)),
			SdlcStageSet:       &sessionintelligencepb.SdlcStageSet{Stages: []sessionintelligencepb.SdlcStage{sessionintelligencepb.SdlcStage(1)}},
			TaskComplexity:     ptrTo(sessionintelligencepb.TaskComplexity(1)),
			UserSteering:       ptrTo(sessionintelligencepb.PhaseUserSteering(1)),
			PhaseObjective:     ptrTo("implement X"),
			PhaseObjectiveMet:  ptrTo(sessionintelligencepb.PhaseObjectiveMet(1)),
			AgentHelpfulness:   ptrTo(sessionintelligencepb.AgentHelpfulness(1)),
			Smoothness:         ptrTo(sessionintelligencepb.Smoothness(1)),
			FrictionCause:      ptrTo(sessionintelligencepb.FrictionCause(1)),
			Speed:              ptrTo(sessionintelligencepb.Speed(1)),
			WentWell:           &sessionintelligencepb.StringList{Values: []string{"clear ask"}},
			WentWrong:          &sessionintelligencepb.StringList{Values: []string{"missed edge"}},
			Coaching: &sessionintelligencepb.PhaseCoachingList{Items: []*sessionintelligencepb.PhaseCoaching{
				{RootCause: "unclear scope", Suggestion: "state the goal"},
			}},
			TurnCount: ptrTo(int32(i + 1)),
		})
	}
	return read
}

// assertAllRFieldsSet reflects over the generated R structs and asserts every field is set
// (the reflection-filled-generator half of the R-field-parity guard) — so a newly-added R field the
// generator forgot fails here. Value-typed index fields (PromptIndex, PhaseOrdinal) start at 1
// so they are never a zero, and the documented E-owned fields are skipped.
func assertAllRFieldsSet(t *testing.T, read *ReadResult) {
	t.Helper()
	assertStructAllSet(t, "SessionVerdict", reflect.ValueOf(read.Verdict))
	for i, turn := range read.Turns {
		assertStructAllSet(t, "TurnJudgment", reflect.ValueOf(*turn))
		require.NotZerof(t, turn.PromptIndex, "turn[%d].PromptIndex must be non-zero", i)
	}
	for i, phase := range read.Phases {
		assertStructAllSet(t, "PhaseJudgment", reflect.ValueOf(*phase))
		require.NotZerof(t, phase.PhaseOrdinal, "phase[%d].PhaseOrdinal must be non-zero", i)
	}
}

func assertStructAllSet(t *testing.T, structName string, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if _, excluded := rehydrateExcludedRFields[field.Name]; excluded {
			continue
		}
		assert.Falsef(t, v.Field(i).IsZero(), "%s.%s must be filled non-trivially by the R generator (or classified E-owned)", structName, field.Name)
	}
}
