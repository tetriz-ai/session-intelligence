// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
)

// phaseReanalysisColumns are the columns ReplacePhaseSet overwrites on an ordinal
// conflict — everything but the identity key (id, org, session_analysis_id,
// phase_ordinal) and created_at, so the row's id is preserved (keeping the
// turn→phase composite FK satisfied) while every judged/measured value is refreshed.
// It includes deleted_at (revive-on-regrow): the ordinal key is a non-partial unique,
// so a phase soft-deleted by a shorter re-analysis still owns its slot; a later upload
// that grows back to that ordinal must REVIVE the row rather than leave it hidden.
var phaseReanalysisColumns = []string{
	"range_start", "range_end", "boundary_overridden", "override_reason", "phase_theme",
	"sdlc_stage_set", "task_complexity", "user_steering", "phase_objective",
	"phase_objective_met", "agent_helpfulness", "smoothness", "friction_cause", "speed",
	"hitl_gate", "turn_count", "tokens", "tool_call_count", "went_well", "went_wrong",
	"coaching", "segmentation_suspect", "conversation_started_at", "deleted_at",
	"updated_at",
}

// SessionPhaseAnalysisRepo implements ISessionPhaseAnalysisDAO for
// session_phase_analyses (data-window-only: no person_id). Same seat-scope
// obligation as the turn repo: seat scoping composes through the parent
// session_analyses join, and no organization_id-only read path may exist over this
// table. Reads join or key on the
// parent's identifier (session_analysis_id / raw_record_id), never a bare org. Surface:
// Create, ListBySessionAnalysisID, and the ReplacePhaseSet phase upsert-on-reanalysis.
type SessionPhaseAnalysisRepo struct {
	db *gorm.DB
}

// NewSessionPhaseAnalysisRepo creates a new SessionPhaseAnalysisRepo.
func NewSessionPhaseAnalysisRepo(db *gorm.DB) *SessionPhaseAnalysisRepo {
	return &SessionPhaseAnalysisRepo{db: db}
}

// Create inserts one phase row.
func (r *SessionPhaseAnalysisRepo) Create(ctx context.Context, phase *model.SessionPhaseAnalysis) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionPhaseAnalysisRepo", "Create", time.Now())
	if phase == nil {
		return fmt.Errorf("creating session phase analysis: nil phase")
	}
	if phase.ID == "" {
		phase.ID = uuid.NewString()
	}
	if err := planscoped.WriteDB(ctx, r.db).Create(phase).Error; err != nil {
		return fmt.Errorf("creating session phase analysis (org %s): %w", phase.OrganizationID, err)
	}
	return nil
}

// ListBySessionAnalysisID returns the phases of one analysis, ordered by phase
// ordinal. It keys on the seat-scoped parent's id, so it exposes no org-wide read
// path over this table.
func (r *SessionPhaseAnalysisRepo) ListBySessionAnalysisID(ctx context.Context, sessionAnalysisID string) ([]*model.SessionPhaseAnalysis, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionPhaseAnalysisRepo", "ListBySessionAnalysisID", time.Now())
	var phases []*model.SessionPhaseAnalysis
	err := planscoped.DB(ctx, r.db).
		Where("session_analysis_id = ? AND deleted_at IS NULL", sessionAnalysisID).
		Order("phase_ordinal ASC, id ASC").
		Find(&phases).Error
	if err != nil {
		return nil, fmt.Errorf("list session phase analyses by session analysis (%s): %w", sessionAnalysisID, err)
	}
	return phases, nil
}

// ReplacePhaseSet is the FK-safe reanalysis write for one session's full phase set
// : the read pass owns the whole phase set, so each
// phase is upserted by its (organization_id, session_analysis_id, phase_ordinal)
// unique — ON CONFLICT DO UPDATE preserving the row's id (so a re-pointed turn's
// composite FK stays valid) — and the trailing phases the new set no longer has are
// soft-deleted. The soft-delete is safe to do here (before the caller re-points its
// turns) precisely because it is an UPDATE of deleted_at, never a hard DELETE: the
// physical row and its id survive, so the turn→phase RESTRICT FK is never tripped
// even while a turn still references a retired phase. Returns the phase_ordinal →
// phase id map the caller uses to re-point each turn's phase_id in the same commit
// transaction. Runs inside the caller's tenant-scoped commit transaction.
func (r *SessionPhaseAnalysisRepo) ReplacePhaseSet(ctx context.Context, orgID, sessionAnalysisID string, phases []*model.SessionPhaseAnalysis) (map[int32]string, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionPhaseAnalysisRepo", "ReplacePhaseSet", time.Now())
	existing, err := r.ListBySessionAnalysisID(ctx, sessionAnalysisID)
	if err != nil {
		return nil, fmt.Errorf("loading existing phases for replace (session %s): %w", sessionAnalysisID, err)
	}
	existingOrdinals := make(map[int32]string)
	for _, phase := range existing {
		if phase.PhaseOrdinal != nil {
			existingOrdinals[*phase.PhaseOrdinal] = phase.ID
		}
	}

	db := planscoped.WriteDB(ctx, r.db)
	ordinalToID := make(map[int32]string, len(phases))
	kept := make(map[int32]bool, len(phases))
	for _, phase := range phases {
		if phase.PhaseOrdinal == nil {
			return nil, fmt.Errorf("replace phase set (session %s): phase missing ordinal", sessionAnalysisID)
		}
		ordinal := *phase.PhaseOrdinal
		phase.OrganizationID = orgID
		phase.SessionAnalysisID = &sessionAnalysisID
		if phase.ID == "" {
			phase.ID = uuid.NewString()
		}
		// ON CONFLICT preserves the existing row's id (id is not in the update set),
		// and RETURNING populates phase.ID with the live id on both insert and update.
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "session_analysis_id"}, {Name: "phase_ordinal"}},
			DoUpdates: clause.AssignmentColumns(phaseReanalysisColumns),
		}).Create(phase).Error; err != nil {
			return nil, fmt.Errorf("upsert phase (session %s, ordinal %d): %w", sessionAnalysisID, ordinal, err)
		}
		ordinalToID[ordinal] = phase.ID
		kept[ordinal] = true
	}

	// Retire the trailing phases the new set dropped, after their turns are re-pointed
	// by the caller. Soft-delete only — a hard delete would trip the turn→phase
	// RESTRICT FK.
	for ordinal, id := range existingOrdinals {
		if kept[ordinal] {
			continue
		}
		if err := db.Where("id = ?", id).Delete(&model.SessionPhaseAnalysis{}).Error; err != nil {
			return nil, fmt.Errorf("soft-delete trailing phase (session %s, ordinal %d): %w", sessionAnalysisID, ordinal, err)
		}
	}
	return ordinalToID, nil
}
