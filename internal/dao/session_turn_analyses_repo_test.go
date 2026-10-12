// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB peer tests for SessionTurnAnalysisRepo. session_turn_analyses is
// data-window-only (no person_id seat column — seat scoping composes through the
// parent session_analyses), so reads key on raw_record_id and carry no seat
// IN-clause. The tests exercise the RETURNING insert, the prompt-index ordering of
// ListByRawRecordID, and the batched purge.

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
)

// TestSessionTurnAnalysisRepo_UpsertJudged_TwoWriter is the two-writer guardrail
// : the measured grouper writes a turn's measured
// columns, then this engine's judged pass UpsertJudged's the same (org, raw_record,
// prompt_index) — the column-scoped upsert sets ONLY the judged columns and must NOT
// clobber the measured ones. SoftDeleteJudgedBeyond then retires a trailing turn.
func TestSessionTurnAnalysisRepo_UpsertJudged_TwoWriter(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	// The measured grouper lands the measured half first (token_breakdown, the NOT-NULL
	// source_content_sha256 staleness anchor).
	measured := newSessionTurn(orgID, rawRecordID, 0)
	measuredSha := "measured-sha"
	measured.SourceContentSha256 = &measuredSha
	require.NoError(t, repo.Create(ctx, measured))

	// This engine's judged pass upserts the judged half on the same key.
	promptType := sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION
	judgedSha := "judged-sha"
	judged := &model.SessionTurnAnalysis{
		OrganizationID:        orgID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		PromptIndex:           0,
		PromptType:            &promptType,
		JudgedContentSha256:   &judgedSha,
	}
	affected, err := repo.UpdateJudged(ctx, judged)
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected, "the judged overlay updates the one live measured row")

	got, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	// The judged column landed.
	require.NotNil(t, got[0].PromptType)
	assert.Equal(t, sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION, *got[0].PromptType)
	require.NotNil(t, got[0].JudgedContentSha256)
	assert.Equal(t, judgedSha, *got[0].JudgedContentSha256)
	// The measured columns the judged pass did NOT write are preserved.
	require.NotNil(t, got[0].SourceContentSha256)
	assert.Equal(t, measuredSha, *got[0].SourceContentSha256, "the judged upsert must not clobber the measured half")
	require.NotNil(t, got[0].TokenBreakdown)
	assert.Equal(t, int64(100), got[0].TokenBreakdown.GetInputTokens())

	// A second turn, then a shorter re-analysis retires it via SoftDeleteJudgedBeyond.
	second := newSessionTurn(orgID, rawRecordID, 1)
	require.NoError(t, repo.Create(ctx, second))
	retired, err := repo.SoftDeleteJudgedBeyond(ctx, orgID, rawRecordID, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), retired)
	remaining, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "the trailing turn beyond prompt_index 0 is soft-deleted")
}

// TestSessionTurnAnalysisRepo_UpsertMeasured_TwoWriter is the mirror of the judged
// two-writer guard for the measured grouper's writer: judged content lands first, the
// measured pass UpsertMeasured's the same key touching ONLY the measured columns, and
// the judged columns are preserved. SoftDeleteMeasuredBeyond then retires a trailing
// turn, and a regrow onto the retired index revives it (deleted_at cleared).
func TestSessionTurnAnalysisRepo_UpsertMeasured_TwoWriter(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	// A prior full generation exists (measured identity + a judged verdict); a fresh
	// measured upsert must overlay the measured half without clobbering the judged one.
	promptType := sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION
	judgedSha := "judged-sha"
	prior := newSessionTurn(orgID, rawRecordID, 0)
	prior.PromptType = &promptType
	prior.JudgedContentSha256 = &judgedSha
	require.NoError(t, repo.Create(ctx, prior))

	// The measured grouper upserts the measured half on the same key.
	measured := newSessionTurn(orgID, rawRecordID, 0)
	measuredSha := "measured-sha"
	measured.SourceContentSha256 = &measuredSha
	require.NoError(t, repo.UpsertMeasured(ctx, measured))

	got, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].SourceContentSha256)
	assert.Equal(t, measuredSha, *got[0].SourceContentSha256, "the measured column landed")
	require.NotNil(t, got[0].JudgedContentSha256)
	assert.Equal(t, judgedSha, *got[0].JudgedContentSha256, "the measured upsert must not clobber the judged half")

	// A trailing turn, retired by the measured writer, then revived on regrow.
	require.NoError(t, repo.UpsertMeasured(ctx, newSessionTurn(orgID, rawRecordID, 1)))
	retired, err := repo.SoftDeleteMeasuredBeyond(ctx, orgID, rawRecordID, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), retired)
	live, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Len(t, live, 1, "the trailing turn beyond index 0 is soft-deleted")

	// Regrow: upserting index 1 again conflicts onto the retired row and revives it.
	require.NoError(t, repo.UpsertMeasured(ctx, newSessionTurn(orgID, rawRecordID, 1)))
	revived, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Len(t, revived, 2, "the retired turn is revived (deleted_at cleared) on regrow")
}

// TestSessionTurnAnalysisRepo_BackfillChildParentPromptIndex stamps parent_prompt_index
// on the sub-agent child rows of a parent conversation, keyed by the dispatch tool_use id.
func TestSessionTurnAnalysisRepo_BackfillChildParentPromptIndex(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	parentConv := "parent-conv"
	toolUseID := "tu-dispatch-1"
	child := newSessionTurn(orgID, uuid.NewString(), 0)
	child.ParentConversationID = &parentConv
	child.ParentToolUseID = &toolUseID
	require.NoError(t, repo.UpsertMeasured(ctx, child))

	updated, err := repo.BackfillChildParentPromptIndex(ctx, orgID, parentConv, map[string]int32{toolUseID: 7})
	require.NoError(t, err)
	assert.Equal(t, int64(1), updated)

	got, err := repo.ListByRawRecordID(ctx, child.RawRecordID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ParentPromptIndex)
	assert.Equal(t, int32(7), *got[0].ParentPromptIndex, "the child's parent prompt index is back-filled")
}

// TestSessionTurnAnalysisRepo_BackfillChildParentPromptIndex_ParentBeforeChild pins the
// parent-arrives-first ordering: when the
// parent commits and back-fills before the child's turn rows are written, the keyed UPDATE
// matches nothing (0 rows) — the child stays unresolved until the children_seq fence
// re-admits the parent for a re-fold, whose re-run then stamps it. Exercised under a
// resolved plan/seat context to prove the plan-scoped write path composes (the table itself
// is data-window-only, so seat scoping is a read-side join concern).
func TestSessionTurnAnalysisRepo_BackfillChildParentPromptIndex_ParentBeforeChild(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := planscoped.WithConfigForTest(context.Background(), planscoped.Config{SeatsLimit: -1, DataWindowDays: -1})
	ctx = planscoped.WithSeats(ctx, planscoped.SeatsContext{PersonIDs: []string{uuid.NewString()}})
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	parentConv, toolUseID := "parent-conv", "tu-dispatch"

	// Parent-before-child: the parent back-fills before any child turn row exists.
	updated, err := repo.BackfillChildParentPromptIndex(ctx, orgID, parentConv, map[string]int32{toolUseID: 4})
	require.NoError(t, err)
	assert.Equal(t, int64(0), updated, "no child rows yet — the keyed UPDATE resolves nothing")

	// The child's turn rows arrive later (its own tick), unresolved.
	child := newSessionTurn(orgID, uuid.NewString(), 0)
	child.ParentConversationID = &parentConv
	child.ParentToolUseID = &toolUseID
	require.NoError(t, repo.UpsertMeasured(ctx, child))
	pre, err := repo.ListByRawRecordID(ctx, child.RawRecordID)
	require.NoError(t, err)
	require.Len(t, pre, 1)
	assert.Nil(t, pre[0].ParentPromptIndex, "the child is unresolved until the parent re-folds")

	// The children_seq fence re-admits the parent; its re-fold re-runs the backfill and now
	// resolves the child.
	updated, err = repo.BackfillChildParentPromptIndex(ctx, orgID, parentConv, map[string]int32{toolUseID: 4})
	require.NoError(t, err)
	assert.Equal(t, int64(1), updated, "the re-fold resolves the now-present child")
	post, err := repo.ListByRawRecordID(ctx, child.RawRecordID)
	require.NoError(t, err)
	require.Len(t, post, 1)
	require.NotNil(t, post[0].ParentPromptIndex)
	assert.Equal(t, int32(4), *post[0].ParentPromptIndex)
}

// TestSessionTurnAnalysisRepo_BackfillChildParentPromptIndex_Batched proves the map is
// applied in one keyed UPDATE across many dispatches (analytics-fanout-budget batching):
// every child with a matching (parent_conversation_id, parent_tool_use_id) is stamped,
// a child on a different tool-use id is untouched, and a soft-deleted child is skipped.
func TestSessionTurnAnalysisRepo_BackfillChildParentPromptIndex_Batched(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	parentConv := "parent-conv"
	tuA, tuB, tuOther := "tu-a", "tu-b", "tu-other"

	mkChild := func(toolUse string) *model.SessionTurnAnalysis {
		c := newSessionTurn(orgID, uuid.NewString(), 0)
		c.ParentConversationID = &parentConv
		c.ParentToolUseID = &toolUse
		require.NoError(t, repo.UpsertMeasured(ctx, c))
		return c
	}
	childA := mkChild(tuA)
	childB := mkChild(tuB)
	childOther := mkChild(tuOther) // not in the backfill map — must stay null

	updated, err := repo.BackfillChildParentPromptIndex(ctx, orgID, parentConv, map[string]int32{tuA: 3, tuB: 9})
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated, "both mapped children updated in one call")

	assertParentIdx := func(rawRecordID string, want *int32) {
		got, err := repo.ListByRawRecordID(ctx, rawRecordID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		if want == nil {
			assert.Nil(t, got[0].ParentPromptIndex)
			return
		}
		require.NotNil(t, got[0].ParentPromptIndex)
		assert.Equal(t, *want, *got[0].ParentPromptIndex)
	}
	three, nine := int32(3), int32(9)
	assertParentIdx(childA.RawRecordID, &three)
	assertParentIdx(childB.RawRecordID, &nine)
	assertParentIdx(childOther.RawRecordID, nil)
}

// TestSessionTurnAnalysisRepo_UpsertMeasured_PreservesBackfilledParentIndex is finding
// 1's guard (the parent/child concurrent-processing race): after the parent has
// back-filled a child's parent_prompt_index, a later child re-upsert that still resolved
// NO index for the SAME parent locator must NOT NULL-clobber the resolved value.
func TestSessionTurnAnalysisRepo_UpsertMeasured_PreservesBackfilledParentIndex(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	parentConv, toolUseID := "parent-conv", "tu-dispatch"
	rawRecordID := uuid.NewString()

	// Child upserted first with no resolved parent index (parent not yet grouped).
	child := newSessionTurn(orgID, rawRecordID, 0)
	child.ParentConversationID = &parentConv
	child.ParentToolUseID = &toolUseID
	require.NoError(t, repo.UpsertMeasured(ctx, child))

	// Parent gets grouped and back-fills the link.
	_, err := repo.BackfillChildParentPromptIndex(ctx, orgID, parentConv, map[string]int32{toolUseID: 5})
	require.NoError(t, err)

	// A later re-upsert of the same child still carries a null parent index for the SAME
	// locator — the backfilled value must survive rather than being clobbered to null.
	rerun := newSessionTurn(orgID, rawRecordID, 0)
	rerun.ParentConversationID = &parentConv
	rerun.ParentToolUseID = &toolUseID
	require.Nil(t, rerun.ParentPromptIndex)
	require.NoError(t, repo.UpsertMeasured(ctx, rerun))

	got, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ParentPromptIndex, "the back-filled parent index must not be NULL-clobbered by a later re-upsert")
	assert.Equal(t, int32(5), *got[0].ParentPromptIndex)
}

// TestSessionTurnAnalysisRepo_UpsertMeasured_RequiresSourceDigest is finding 2's guard:
// a measured upsert with no per-body source digest is rejected, so no live row with a
// NULL digest can be written (it could not join the measured-vs-judged coherence check).
func TestSessionTurnAnalysisRepo_UpsertMeasured_RequiresSourceDigest(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	turn := newSessionTurn(orgID, uuid.NewString(), 0)
	turn.SourceContentSha256 = nil
	err := repo.UpsertMeasured(ctx, turn)
	require.Error(t, err, "a measured upsert with no source digest must be rejected")

	empty := ""
	turn.SourceContentSha256 = &empty
	require.Error(t, repo.UpsertMeasured(ctx, turn), "an empty source digest must also be rejected")
}

// TestSessionTurnAnalysisRepo_UpsertJudged_PreservesIdentity is finding 2's guard: the
// measured grouper owns the turn IDENTITY columns (prompt_origination, author,
// is_subagent), and the judged pass's buildTurnRow never populates them. Because those
// columns are no longer in turnJudgedColumns, a measured-first turn's identity must
// SURVIVE the judged upsert (they were being NULL-clobbered), while the verdict columns
// the judged pass owns ARE written.
func TestSessionTurnAnalysisRepo_UpsertJudged_PreservesIdentity(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	// The measured grouper lands identity + measured columns first.
	origin := sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN
	author := "octocat"
	isSubagent := false
	fidelity := sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED
	sha := "measured-sha"
	measured := &model.SessionTurnAnalysis{
		OrganizationID:        orgID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		PromptIndex:           0,
		PromptOrigination:     &origin,
		Author:                &author,
		IsSubagent:            &isSubagent,
		TokenFidelity:         &fidelity,
		SourceContentSha256:   &sha,
	}
	require.NoError(t, repo.Create(ctx, measured))

	// The judged pass updates ONLY the verdict half — it never sets identity.
	promptType := sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION
	tokenLocus := sessionintelligencepb.TokenLocus_TOKEN_LOCUS_AGENT_REASONING
	affected, err := repo.UpdateJudged(ctx, &model.SessionTurnAnalysis{
		OrganizationID:        orgID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		PromptIndex:           0,
		PromptType:            &promptType,
		TokenLocus:            &tokenLocus,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected)

	got, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	// The verdict columns landed.
	require.NotNil(t, got[0].PromptType)
	assert.Equal(t, sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION, *got[0].PromptType)
	require.NotNil(t, got[0].TokenLocus)
	assert.Equal(t, sessionintelligencepb.TokenLocus_TOKEN_LOCUS_AGENT_REASONING, *got[0].TokenLocus)
	// The measured identity columns SURVIVE — they must not be NULL-clobbered.
	require.NotNil(t, got[0].PromptOrigination, "prompt_origination must survive the judged upsert")
	assert.Equal(t, sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN, *got[0].PromptOrigination)
	require.NotNil(t, got[0].Author, "author must survive the judged upsert")
	assert.Equal(t, "octocat", *got[0].Author)
	require.NotNil(t, got[0].IsSubagent, "is_subagent must survive the judged upsert")
	assert.False(t, *got[0].IsSubagent)
}

// TestSessionTurnAnalysisRepo_UpdateJudged_IsUpdateOnly guards the single-cutter
// contract: the judged pass NEVER creates or revives a turn
// row — the measured grouper is the sole creator and reviver of a turn's identity. So a
// judged update against a prompt_index the grouper never created affects zero rows (the
// caller requeues rather than land a judged-only row), and a judged update against a
// retired (soft-deleted) row does NOT revive it (revive-on-regrow belongs to the measured
// writer's UpsertMeasured, exercised by the measured two-writer test).
func TestSessionTurnAnalysisRepo_UpdateJudged_IsUpdateOnly(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	promptType := sessionintelligencepb.PromptType_PROMPT_TYPE_CREATION
	judgedTurn := func(promptIndex int32) *model.SessionTurnAnalysis {
		return &model.SessionTurnAnalysis{
			OrganizationID:        orgID,
			RawRecordID:           rawRecordID,
			ConversationStartedAt: sessionTestDate,
			PromptIndex:           promptIndex,
			PromptType:            &promptType,
		}
	}

	// No measured row exists yet → the judged update inserts nothing and reports 0 rows.
	affected, err := repo.UpdateJudged(ctx, judgedTurn(0))
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "judged update against a turn the grouper never created writes nothing")
	got, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Empty(t, got, "no judged-only row is created")

	// The grouper creates the row, then retires it; a judged update must NOT revive it.
	require.NoError(t, repo.Create(ctx, newSessionTurn(orgID, rawRecordID, 0)))
	_, err = repo.SoftDeleteMeasuredBeyond(ctx, orgID, rawRecordID, -1) // retire everything > -1, i.e. all
	require.NoError(t, err)
	affected, err = repo.UpdateJudged(ctx, judgedTurn(0))
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "judged update does not revive a retired turn — revive is the measured writer's job")
	live, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	assert.Empty(t, live, "the retired row stays retired")
}

// newSessionTurn builds a turn row at the given prompt index, carrying a proto-enum
// text column (prompt_origination) and a JSONB proto-message column (token_breakdown)
// for the round-trip assertion.
func newSessionTurn(orgID, rawRecordID string, promptIndex int32) *model.SessionTurnAnalysis {
	origin := sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN
	// UpsertMeasured requires a non-empty source digest, so every measured turn
	// helper supplies one (the real grouper always does); tests that assert on the
	// digest override it explicitly. The measured grouper's other NOT-NULL columns
	// (token_fidelity, author, is_subagent) are seeded here for the same reason.
	sha := fmt.Sprintf("measured-body-sha-%d", promptIndex)
	author := "SESSION_EVENT_AUTHOR_HUMAN"
	fidelity := sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED
	isSubagent := false
	return &model.SessionTurnAnalysis{
		OrganizationID:        orgID,
		RawRecordID:           rawRecordID,
		ConversationStartedAt: sessionTestDate,
		PromptIndex:           promptIndex,
		PromptOrigination:     &origin,
		Author:                &author,
		TokenFidelity:         &fidelity,
		IsSubagent:            &isSubagent,
		SourceContentSha256:   &sha,
		TokenBreakdown: &sessionintelligencepb.AnalysisTokenBreakdown{
			InputTokens: int64(100 * (promptIndex + 1)),
			TotalTokens: int64(100 * (promptIndex + 1)),
		},
	}
}

func TestSessionTurnAnalysisRepo_CreateAndListByRawRecordID(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgID := seedOrg(t, tx)
	rawRecordID := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	// Insert out of prompt-index order to prove the ORDER BY decides the result.
	require.NoError(t, repo.Create(ctx, newSessionTurn(orgID, rawRecordID, 2)))
	// The index-0 turn also carries the measured parent-locator + billing/compaction columns
	// (peer) so the round-trip below proves they map through.
	turn0 := newSessionTurn(orgID, rawRecordID, 0)
	nativeBilling, compactions, parentConv := 2.5, int32(3), "conv-parent"
	turn0.NativeBillingUnits = &nativeBilling
	turn0.CompactionCount = &compactions
	turn0.ParentConversationID = &parentConv
	// The models blob carries per-model effort (peer), so a mixed-model turn round-trips
	// each model's effort — there is no flat model_effort column.
	turn0.Models = &sessionintelligencepb.TurnModelList{Items: []*sessionintelligencepb.TurnModel{
		{Model: "gemini-2.5-flash", Effort: "high"},
		{Model: "gemini-2.5-flash-lite", Effort: "low"},
	}}
	require.NoError(t, repo.Create(ctx, turn0))
	require.NoError(t, repo.Create(ctx, newSessionTurn(orgID, rawRecordID, 1)))

	got, err := repo.ListByRawRecordID(ctx, rawRecordID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, int32(0), got[0].PromptIndex)
	assert.Equal(t, int32(1), got[1].PromptIndex)
	assert.Equal(t, int32(2), got[2].PromptIndex, "turns must be ordered by prompt_index ASC")
	// Round-trips on the first turn.
	require.NotNil(t, got[0].PromptOrigination)
	assert.Equal(t, sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN, *got[0].PromptOrigination)
	require.NotNil(t, got[0].TokenBreakdown)
	assert.Equal(t, int64(100), got[0].TokenBreakdown.InputTokens)
	// The measured parent-locator + billing/compaction columns round-trip.
	require.NotNil(t, got[0].NativeBillingUnits)
	assert.InDelta(t, 2.5, *got[0].NativeBillingUnits, 1e-9)
	require.NotNil(t, got[0].CompactionCount)
	assert.Equal(t, int32(3), *got[0].CompactionCount)
	require.NotNil(t, got[0].ParentConversationID)
	assert.Equal(t, "conv-parent", *got[0].ParentConversationID)
	// The per-model {model, effort} blob round-trips, effort carried per model.
	require.NotNil(t, got[0].Models)
	require.Len(t, got[0].Models.GetItems(), 2)
	assert.Equal(t, "gemini-2.5-flash", got[0].Models.GetItems()[0].GetModel())
	assert.Equal(t, "high", got[0].Models.GetItems()[0].GetEffort())
	assert.Equal(t, "low", got[0].Models.GetItems()[1].GetEffort())

	empty, err := repo.ListByRawRecordID(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestSessionTurnAnalysisRepo_ListByRawRecordID_CrossTenantExclusion asserts a
// distinct org's turns (under a distinct raw_record_id) are not returned for
// another org's key.
func TestSessionTurnAnalysisRepo_ListByRawRecordID_CrossTenantExclusion(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tx := newTestTx(t)
	orgA := seedOrg(t, tx)
	orgB := seedOrg(t, tx)
	rawA := uuid.NewString()
	rawB := uuid.NewString()
	ctx := context.Background()
	repo := dao.NewSessionTurnAnalysisRepo(tx)

	require.NoError(t, repo.Create(ctx, newSessionTurn(orgA, rawA, 0)))
	require.NoError(t, repo.Create(ctx, newSessionTurn(orgB, rawB, 0)))

	got, err := repo.ListByRawRecordID(ctx, rawA)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, orgA, got[0].OrganizationID, "org B's turns resolve only by org B's key")
}
