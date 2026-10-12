// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// convBody builds a one-human-turn conversation (a user prompt + its assistant reply) as a
// protojson body, optionally carrying the given session events. One group ⇒ one measured
// turn at prompt_index 0.
func convBody(t *testing.T, events []*sessionintelligencepb.SessionEvent) []byte {
	t.Helper()
	start := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	conv := &sessionintelligencepb.Conversation{
		IdeType: sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		Prompts: []*sessionintelligencepb.Prompt{
			{Role: sessionintelligencepb.Role_ROLE_USER, Content: "do it", Timestamp: timestamppb.New(start), Metadata: &sessionintelligencepb.PromptMetadata{InputTokens: 10}},
			{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: "ok", Timestamp: timestamppb.New(start.Add(time.Minute)), Metadata: &sessionintelligencepb.PromptMetadata{OutputTokens: 20}},
		},
		SessionEvents: events,
	}
	body, err := protojson.Marshal(conv)
	require.NoError(t, err)
	return body
}

// TestProcessFlat_ChildWritesMeasuredTurns pins #6: a delegated child (seated, in-window,
// turn telemetry on) writes its own measured turn rows before MarkSkipped, stamped with the
// parent locator and a NULL parent_prompt_index, with the delegated-agent identity.
func TestProcessFlat_ChildWritesMeasuredTurns(t *testing.T) {
	h := newHarness(t)
	orgID := uuid.NewString()
	person := uuid.NewString()
	record := sessionRawRecord(t, orgID, person, "sha1")
	record.Metadata = mustJSON(t, map[string]string{
		"person_id":         person,
		"relation_type":     "SUBAGENT",
		"parent_record_key": "parent-key",
		"tool_use_id":       "toolu_abc",
	})

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	// The child body (payload s3_key is "s3://body") is read once, for grouping.
	h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(convBody(t, nil), nil)

	var got *model.SessionTurnAnalysis
	h.turn.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, turn *model.SessionTurnAnalysis) error {
			got = turn
			return nil
		})
	h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(0)).Return(int64(0), nil)
	h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD).Return(true, nil)
	h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "parent-key").Return(nil, dao.ErrNotFound)
	// Never claims/judges a child.
	h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).Times(0)
	h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Times(0)

	processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	require.NotNil(t, got, "a child measured turn row is written")
	assert.Equal(t, int32(0), got.PromptIndex)
	require.NotNil(t, got.ParentConversationID)
	assert.Equal(t, "parent-key", *got.ParentConversationID)
	require.NotNil(t, got.ParentToolUseID)
	assert.Equal(t, "toolu_abc", *got.ParentToolUseID)
	assert.Nil(t, got.ParentPromptIndex, "parent_prompt_index is NULL until the parent backfills it")
	require.NotNil(t, got.IsSubagent)
	assert.True(t, *got.IsSubagent, "a delegated child's turn identity is the agent")
	require.NotNil(t, got.Author)
	assert.Equal(t, "SESSION_EVENT_AUTHOR_DELEGATED_AGENT", *got.Author)
	require.NotNil(t, got.SourceContentSha256, "the per-body digest is stamped")
}

// TestProcessFlat_ChildTurnsGatedOffWhenUnseated pins decision A: a child whose person is
// unseated writes NO measured turn rows (the read side would exclude them), even with turn
// telemetry on — it stays a pure CHILD skip.
func TestProcessFlat_ChildTurnsGatedOffWhenUnseated(t *testing.T) {
	h := newHarness(t)
	orgID := uuid.NewString()
	seated := uuid.NewString()
	record := sessionRawRecord(t, orgID, uuid.NewString(), "sha1") // an UNSEATED child person
	record.Metadata = mustJSON(t, map[string]string{
		"person_id":         uuid.NewString(),
		"relation_type":     "SUBAGENT",
		"parent_record_key": "parent-key",
		"tool_use_id":       "toolu_abc",
	})

	h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
	h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
	h.state.EXPECT().MarkSkipped(gomock.Any(), gomock.Any(), sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD).Return(true, nil)
	h.rawDAO.EXPECT().GetByRecordKey(gomock.Any(), orgID, sa.SessionConnectorID, "parent-key").Return(nil, dao.ErrNotFound)
	// The guardrail: an unseated child gets no S3 read and no turn write.
	h.s3.EXPECT().ReadObject(gomock.Any(), gomock.Any()).Times(0)
	h.turn.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Times(0)

	// A seat cap resolved active with a seat set that excludes the child's person.
	ctx := planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: 1, DataWindowDays: -1})
	ctx = planscoped.WithSeats(ctx, planscoped.SeatsContext{PersonIDs: []string{seated}})
	processed, err := h.proc.ProcessFlat(ctx, orgID, 50)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}

// TestProcessFlat_ParentBackfillsChildLinks pins #7: after a parent commit, the derived
// dispatch → prompt_index map is applied via BackfillChildParentPromptIndex keyed on the
// parent's record_key; a parent that dispatched nothing (empty map) never calls it.
func TestProcessFlat_ParentBackfillsChildLinks(t *testing.T) {
	tests := []struct {
		name         string
		events       []*sessionintelligencepb.SessionEvent
		wantBackfill bool
	}{
		{
			name: "dispatch present — backfill with the derived map",
			events: []*sessionintelligencepb.SessionEvent{
				{Sequence: 1, Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE, PromptOrdinal: 1},
				// A TOOL_CALL named Task (not SKILL_CALL) so the artifact ledger does not fire E.
				{Sequence: 2, Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL, OperationName: "Task", CallId: "call-x"},
				{Sequence: 3, Kind: sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE, PromptOrdinal: 2},
			},
			wantBackfill: true,
		},
		{
			name:         "no timeline — backfill is skipped",
			events:       nil,
			wantBackfill: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			orgID, person := uuid.NewString(), uuid.NewString()
			record := sessionRawRecord(t, orgID, person, "sha1")
			record.RecordKey = "parent-key"
			claimToken := "tok-p"
			ord0 := int32(0)

			h.expectScan(&dao.ActionableCandidate{RawRecordID: record.ID, Category: dao.CategoryNoState})
			h.rawDAO.EXPECT().GetByIDForDate(gomock.Any(), record.ID, gomock.Any()).Return(record, nil)
			h.state.EXPECT().ClaimForProcessing(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, s *model.SessionAnalysisState) (bool, error) {
					s.ClaimToken = &claimToken
					return true, nil
				})
			h.s3.EXPECT().ReadObject(gomock.Any(), "s3://body").Return(convBody(t, tt.events), nil)
			h.state.EXPECT().TransitionToSpineDone(gomock.Any(), record.ID, claimToken, "sha1", gomock.Any(), int32(2), gomock.Any()).Return(true, nil)
			// The session-grain measured row is written at spine_done, before the producers.
			h.analysis.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
			h.producer.EXPECT().AnalyzeRead(gomock.Any(), gomock.Any()).Return(&sa.ReadResult{
				Phases: []*sa.PhaseJudgment{{PhaseOrdinal: 0}},
				Turns:  []*sa.TurnJudgment{{PromptIndex: 0, PhaseOrdinal: &ord0}},
			}, nil)
			h.analysis.EXPECT().UpsertJudged(gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, a *model.SessionAnalysis, _ bool) error {
					a.ID = "session-1"
					return nil
				})
			// The parent's own measured turn (one group, prompt_index 0).
			h.turn.EXPECT().UpsertMeasured(gomock.Any(), gomock.Any()).Return(nil)
			h.turn.EXPECT().SoftDeleteMeasuredBeyond(gomock.Any(), orgID, record.ID, int32(0)).Return(int64(0), nil)
			h.phase.EXPECT().ReplacePhaseSet(gomock.Any(), orgID, "session-1", gomock.Any()).
				Return(map[int32]string{0: "phase-0"}, nil)
			h.turn.EXPECT().UpdateJudged(gomock.Any(), gomock.Any()).Return(int64(1), nil)
			h.state.EXPECT().TransitionToJudged(gomock.Any(), record.ID, claimToken, int32(0), gomock.Any()).Return(true, nil)
			// R returned no security issues, so the commit reconciles an empty flag set.
			h.secFlag.EXPECT().ReconcileOpenFlags(gomock.Any(), record.ID, gomock.Any()).Return(nil)

			if tt.wantBackfill {
				h.turn.EXPECT().BackfillChildParentPromptIndex(gomock.Any(), orgID, "parent-key", map[string]int32{"call-x": 0}).Return(int64(0), nil).Times(1)
			} else {
				h.turn.EXPECT().BackfillChildParentPromptIndex(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
			}

			processed, err := h.proc.ProcessFlat(readyCtx(), orgID, 50)
			require.NoError(t, err)
			assert.Equal(t, 1, processed)
		})
	}
}
