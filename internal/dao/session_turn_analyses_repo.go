// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
)

// turnJudgedColumns are the columns this engine's judged (read-pass) writer owns on the
// two-writer turn table. A column-scoped upsert writes ONLY these on conflict, so the
// peer measured grouper's columns are never clobbered. It
// deliberately excludes every measured column and the generated *_tokens columns — and,
// critically, the turn IDENTITY columns prompt_origination / author / is_subagent, which
// the measured grouper owns and the judged buildTurnRow never populates: including them
// here would NULL-clobber a measured-first turn's identity on the judged upsert.
// It also EXCLUDES deleted_at: the judged pass is UPDATE-only (UpdateJudged) and its WHERE
// matches only a LIVE (non-retired) measured row, so it never writes deleted_at — reviving
// a regrown slot or retiring a trailing one is the measured writer's job, not the judge's.
// The measured grouper is the tail cutter (writeMeasuredTurns → SoftDeleteMeasuredBeyond
// runs first on the shared cut) and owns deleted_at for revive-on-regrow, so the judged
// pass only updates the live rows it created and never revives or retires a slot the
// measured writer owns.
var turnJudgedColumns = []string{
	"session_analysis_id", "phase_id",
	"prompt_type", "user_steering", "prompt_completeness", "prompt_objective_met",
	"agent_helpfulness", "agent_initiative", "tool_use", "was_retry", "failure_class",
	"token_locus", "waste_cause", "decision_type", "downstream_impact", "feedback_target",
	"feedback_disposition", "referenced_artifact", "referenced_finding_ids",
	"round_trip_fault", "round_trip_evidence_prompt_index", "prompt_issues",
	"judged_content_sha256", "evidence_seq", "chunk_index", "updated_at",
}

// SessionTurnAnalysisRepo implements ISessionTurnAnalysisDAO for
// session_turn_analyses (data-window-only: no person_id). The table carries the
// data_window policy, so writes go through planscoped.WriteDB. Reads MUST compose
// seat scoping through the parent session_analyses join or an explicit seat/person
// predicate — no organization_id-only read path may exist on this table
// . Surface: Create, ListByRawRecordID, the two-writer
// column-scoped UpsertJudged, SoftDeleteJudgedBeyond, and the seat-joined aggregate reads.
type SessionTurnAnalysisRepo struct {
	db *gorm.DB
}

// NewSessionTurnAnalysisRepo creates a new SessionTurnAnalysisRepo.
func NewSessionTurnAnalysisRepo(db *gorm.DB) *SessionTurnAnalysisRepo {
	return &SessionTurnAnalysisRepo{db: db}
}

// Create inserts one turn row. The four *_tokens columns are generated in the DB
// from token_breakdown and are read-only, so they are never written here.
func (r *SessionTurnAnalysisRepo) Create(ctx context.Context, turn *model.SessionTurnAnalysis) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "Create", time.Now())
	if turn == nil {
		return fmt.Errorf("creating session turn analysis: nil turn")
	}
	if turn.ID == "" {
		turn.ID = uuid.NewString()
	}
	if err := planscoped.WriteDB(ctx, r.db).Create(turn).Error; err != nil {
		return fmt.Errorf("creating session turn analysis (org %s, raw_record %s, prompt %d): %w", turn.OrganizationID, turn.RawRecordID, turn.PromptIndex, err)
	}
	return nil
}

// ListByRawRecordID returns the turns for one raw record, ordered by prompt index.
// It reads by raw_record_id (the seat-scoped parent's key) rather than a bare
// organization_id, so it does not expose an org-wide read path over this table.
func (r *SessionTurnAnalysisRepo) ListByRawRecordID(ctx context.Context, rawRecordID string) ([]*model.SessionTurnAnalysis, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "ListByRawRecordID", time.Now())
	var turns []*model.SessionTurnAnalysis
	err := planscoped.DB(ctx, r.db).
		Where("raw_record_id = ? AND deleted_at IS NULL", rawRecordID).
		Order("prompt_index ASC, id ASC").
		Find(&turns).Error
	if err != nil {
		return nil, fmt.Errorf("list session turn analyses by raw record (%s): %w", rawRecordID, err)
	}
	return turns, nil
}

// UpdateJudged overlays this engine's judged half onto the LIVE measured turn row the
// grouper already created, keyed on the two-writer key (organization_id, raw_record_id,
// prompt_index) and touching ONLY turnJudgedColumns, so the peer measured grouper's columns
// are preserved. It is UPDATE-only — it NEVER inserts: the measured grouper is the single
// creator of a turn's identity and lifecycle, so a judged verdict for a prompt_index the
// grouper did not produce has no row to attach to (the caller requeues rather than land a
// judged-only row with the measured columns — author, token_fidelity, source_content_sha256
// — left NULL). The soft-delete scope means only a LIVE (non-retired) measured row matches,
// and deleted_at is not in turnJudgedColumns, so the judged pass never revives a retired
// turn. Returns the rows affected: 0 means no live measured row matched.
func (r *SessionTurnAnalysisRepo) UpdateJudged(ctx context.Context, turn *model.SessionTurnAnalysis) (int64, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "UpdateJudged", time.Now())
	if turn == nil {
		return 0, fmt.Errorf("updating session turn analysis: nil turn")
	}
	turn.UpdatedAt = time.Now()
	res := planscoped.WriteDB(ctx, r.db).
		Model(&model.SessionTurnAnalysis{}).
		Where("organization_id = ? AND raw_record_id = ? AND prompt_index = ?", turn.OrganizationID, turn.RawRecordID, turn.PromptIndex).
		Select(turnJudgedColumns).
		Updates(turn)
	if res.Error != nil {
		return 0, fmt.Errorf("update judged turn (org %s, raw_record %s, prompt %d): %w", turn.OrganizationID, turn.RawRecordID, turn.PromptIndex, res.Error)
	}
	return res.RowsAffected, nil
}

// SoftDeleteJudgedBeyond retires the trailing turns a shorter re-analysis dropped:
// turns whose prompt_index exceeds the new count are soft-deleted so a shorter
// re-analysis cannot leave stale higher-index turns to be counted in aggregates
// . Soft-delete only (an UPDATE of deleted_at), so the
// turn→phase composite FK is never tripped.
func (r *SessionTurnAnalysisRepo) SoftDeleteJudgedBeyond(ctx context.Context, orgID, rawRecordID string, maxPromptIndex int32) (int64, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "SoftDeleteJudgedBeyond", time.Now())
	res := planscoped.WriteDB(ctx, r.db).
		Where("organization_id = ? AND raw_record_id = ? AND prompt_index > ?", orgID, rawRecordID, maxPromptIndex).
		Delete(&model.SessionTurnAnalysis{})
	if res.Error != nil {
		return 0, fmt.Errorf("soft-delete trailing judged turns (org %s, raw_record %s): %w", orgID, rawRecordID, res.Error)
	}
	return res.RowsAffected, nil
}

// turnMeasuredColumns are the columns the measured grouper (the spine's turn
// component) owns on the two-writer turn table — the disjoint complement of
// turnJudgedColumns. A column-scoped upsert writes ONLY these on conflict, so the
// peer judged writer's evaluation columns are never clobbered. It carries the turn
// IDENTITY (conversation_started_at, prompt_origination, author, is_subagent) — the
// grouper is the one cutter, so it owns which prompt_index a turn is — plus every
// measured column. It EXCLUDES the four generated *_tokens columns (Postgres rejects
// a write to a generated column) and session_analysis_id (the judged pass back-fills
// that). It INCLUDES deleted_at for regrow-revive: a later upload that grows back to a
// retired prompt_index conflicts and must REVIVE the row (deleted_at = EXCLUDED = NULL).
var turnMeasuredColumns = []string{
	"conversation_started_at", "prompt_origination", "author", "is_subagent",
	"models", "token_breakdown", "token_fidelity", "native_billing_units",
	"lines_added", "lines_removed", "tool_call_count", "tool_error_count", "tools",
	"tool_retry_count", "api_retry_count", "compaction_count", "files_touched",
	"surface", "started_at", "ended_at", "repo_full_name", "context_tokens",
	"context_pct", "subagent_count", "parent_conversation_id", "parent_prompt_index",
	"parent_tool_use_id", "source_content_sha256",
	"deleted_at", "updated_at",
}

// measuredUpsertAssignments is the ON CONFLICT DO UPDATE set for UpsertMeasured. Every
// measured column overwrites from EXCLUDED, EXCEPT parent_prompt_index: that one
// preserves an existing non-null value when the incoming row resolved NO index for the
// SAME parent locator. A sub-agent child and its parent can be processed concurrently —
// the child is upserted with a null parent_prompt_index (parent not yet grouped), the
// parent then backfills the child's row (BackfillChildParentPromptIndex), and a later
// child re-upsert would otherwise NULL-clobber that just-resolved link, permanently
// orphaning the child's rollup with nothing to repair it. When the locator differs the
// incoming value wins (a re-parent legitimately discards the stale backfill).
var measuredUpsertAssignments = buildMeasuredUpsertAssignments()

func buildMeasuredUpsertAssignments() clause.Set {
	cols := make([]string, 0, len(turnMeasuredColumns))
	for _, c := range turnMeasuredColumns {
		if c == "parent_prompt_index" {
			continue
		}
		cols = append(cols, c)
	}
	set := clause.AssignmentColumns(cols)
	set = append(set, clause.Assignment{
		Column: clause.Column{Name: "parent_prompt_index"},
		Value: gorm.Expr(
			"CASE WHEN EXCLUDED.parent_prompt_index IS NOT NULL THEN EXCLUDED.parent_prompt_index " +
				"WHEN EXCLUDED.parent_conversation_id IS NOT DISTINCT FROM session_turn_analyses.parent_conversation_id " +
				"AND EXCLUDED.parent_tool_use_id IS NOT DISTINCT FROM session_turn_analyses.parent_tool_use_id " +
				"THEN session_turn_analyses.parent_prompt_index " +
				"ELSE EXCLUDED.parent_prompt_index END",
		),
	})
	return set
}

// UpsertMeasured writes the measured half of one turn as a column-scoped upsert on
// the two-writer key (organization_id, raw_record_id, prompt_index) — ON CONFLICT DO
// UPDATE touching ONLY turnMeasuredColumns, so the peer judged writer's columns are
// preserved. deleted_at is in the set, so a conflict onto a previously tail-retired
// index REVIVES that row (regrow-revive), because the measured grouper fully owns the
// turn set per generation. Paired with SoftDeleteMeasuredBeyond in one txn by the caller.
//
// Requires a non-empty SourceContentSha256: the digest is what lets a row participate in
// the measured-vs-judged coherence check, and a null one blocks the planned NOT NULL
// tightening, so a measured upsert without a per-body digest is rejected rather than
// silently writing an unverifiable row.
func (r *SessionTurnAnalysisRepo) UpsertMeasured(ctx context.Context, turn *model.SessionTurnAnalysis) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "UpsertMeasured", time.Now())
	if turn == nil {
		return fmt.Errorf("upserting measured session turn analysis: nil turn")
	}
	if turn.SourceContentSha256 == nil || *turn.SourceContentSha256 == "" {
		return fmt.Errorf("upsert measured turn (org %s, raw_record %s, prompt %d): source content digest is required", turn.OrganizationID, turn.RawRecordID, turn.PromptIndex)
	}
	if turn.ID == "" {
		turn.ID = uuid.NewString()
	}
	err := planscoped.WriteDB(ctx, r.db).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}, {Name: "prompt_index"}},
		DoUpdates: measuredUpsertAssignments,
	}).Create(turn).Error
	if err != nil {
		return fmt.Errorf("upsert measured turn (org %s, raw_record %s, prompt %d): %w", turn.OrganizationID, turn.RawRecordID, turn.PromptIndex, err)
	}
	return nil
}

// SoftDeleteMeasuredBeyond retires the trailing turns a shorter re-grouping dropped:
// turns whose prompt_index exceeds the new count are soft-deleted so a shorter
// re-analysis cannot leave stale higher-index turns in aggregates. The measured
// grouper owns tail management — the judged writer
// no longer touches deleted_at. Soft-delete only (an UPDATE of deleted_at), so the turn→phase
// composite FK is never tripped; a later regrow conflicts and UpsertMeasured revives.
func (r *SessionTurnAnalysisRepo) SoftDeleteMeasuredBeyond(ctx context.Context, orgID, rawRecordID string, maxPromptIndex int32) (int64, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "SoftDeleteMeasuredBeyond", time.Now())
	res := planscoped.WriteDB(ctx, r.db).
		Where("organization_id = ? AND raw_record_id = ? AND prompt_index > ?", orgID, rawRecordID, maxPromptIndex).
		Delete(&model.SessionTurnAnalysis{})
	if res.Error != nil {
		return 0, fmt.Errorf("soft-delete trailing measured turns (org %s, raw_record %s): %w", orgID, rawRecordID, res.Error)
	}
	return res.RowsAffected, nil
}

// BackfillChildParentPromptIndex resolves a sub-agent turn's parent locator: a child
// turn stored as its own row knows its parent conversation + the dispatching tool-use
// id, but the parent's prompt_index is only known once the parent conversation is
// grouped. Given a parent conversation and a map of dispatch tool_use_id → the parent
// prompt_index it fell in, this stamps parent_prompt_index on the matching child rows.
// Keyed by (org, parent_conversation_id, parent_tool_use_id) — the child rows carry
// both. Returns the rows updated.
//
// The whole map is applied in ONE keyed UPDATE — a VALUES probe joined against the
// child rows — not a per-pair loop: on this RLS/plan-scoped table each pair would
// otherwise pay its own round-trip plus tenant + data_window policy evaluation, so N
// dispatches would cost N of each (analytics-fanout-budget: batch per-pair DAO loops).
func (r *SessionTurnAnalysisRepo) BackfillChildParentPromptIndex(ctx context.Context, orgID, parentConversationID string, toolUseIDToPromptIndex map[string]int32) (int64, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionTurnAnalysisRepo", "BackfillChildParentPromptIndex", time.Now())
	if len(toolUseIDToPromptIndex) == 0 {
		return 0, nil
	}
	// VALUES rows first (they appear first in the SQL, so their binds bind first); the
	// first row is type-annotated so Postgres infers the probe columns' types. The org
	// and parent-conversation predicates that pre-narrow the loop are kept on the join.
	valueRows := make([]string, 0, len(toolUseIDToPromptIndex))
	args := make([]interface{}, 0, len(toolUseIDToPromptIndex)*2+2)
	for toolUseID, promptIndex := range toolUseIDToPromptIndex {
		if len(valueRows) == 0 {
			valueRows = append(valueRows, "(?::text, ?::int)")
		} else {
			valueRows = append(valueRows, "(?, ?)")
		}
		args = append(args, toolUseID, promptIndex)
	}
	args = append(args, orgID, parentConversationID)
	sql := "UPDATE session_turn_analyses AS t SET parent_prompt_index = v.prompt_index, updated_at = now() " +
		"FROM (VALUES " + strings.Join(valueRows, ", ") + ") AS v(tool_use_id, prompt_index) " +
		"WHERE t.organization_id = ? AND t.parent_conversation_id = ? " +
		"AND t.parent_tool_use_id = v.tool_use_id AND t.deleted_at IS NULL"
	res := planscoped.WriteDB(ctx, r.db).Exec(sql, args...)
	if res.Error != nil {
		return 0, fmt.Errorf("backfill child parent prompt index (org %s, parent_conv %s): %w", orgID, parentConversationID, res.Error)
	}
	return res.RowsAffected, nil
}
