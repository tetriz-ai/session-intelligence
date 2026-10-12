// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
)

// SessionRoundTripFaultRepo implements ISessionRoundTripFaultDAO for the E-owned
// session_round_trip_faults table (data-window-only: no person_id; seat scoping composes
// through the parent session_analyses join, like the turn/phase repos). E writes its per-pair
// faults here instead of stamping R's turn rows, so R stays immutable. Writes
// go through planscoped.WriteDB (the
// data_window GUC for the RETURNING insert); reads key on the parent's session_analysis_id,
// never a bare organization_id. Surface: ReplaceSet (the replace-set write) and
// ListBySessionAnalysisID (the serving read).
type SessionRoundTripFaultRepo struct {
	db *gorm.DB
}

// NewSessionRoundTripFaultRepo creates a new SessionRoundTripFaultRepo.
func NewSessionRoundTripFaultRepo(db *gorm.DB) *SessionRoundTripFaultRepo {
	return &SessionRoundTripFaultRepo{db: db}
}

// ReplaceSet is E's replace-set write for one session's round-trip faults: it soft-deletes
// every live fault for the session_analysis_id, then inserts the given set — so an E re-run
// REPLACES the fault set rather than appending. An empty
// set clears the session's faults (E re-ran and found none). Runs inside the caller's
// tenant-scoped commit transaction, so the delete and the insert commit atomically. The caller
// keys the set by prompt_index (one fault per prompt_index — the writer-key partial unique).
func (r *SessionRoundTripFaultRepo) ReplaceSet(ctx context.Context, orgID, sessionAnalysisID string, faults []*model.SessionRoundTripFault) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionRoundTripFaultRepo", "ReplaceSet", time.Now())
	db := planscoped.WriteDB(ctx, r.db)
	now := time.Now().UTC()
	// Soft-delete the prior generation's faults first: the writer-key partial unique excludes
	// deleted_at rows, so a re-inserted prompt_index does not collide with its retired row.
	if err := db.Model(&model.SessionRoundTripFault{}).
		Where("organization_id = ? AND session_analysis_id = ? AND deleted_at IS NULL", orgID, sessionAnalysisID).
		Update("deleted_at", now).Error; err != nil {
		return fmt.Errorf("soft-delete existing round-trip faults (org %s, session %s): %w", orgID, sessionAnalysisID, err)
	}
	if len(faults) == 0 {
		return nil
	}
	for _, fault := range faults {
		if fault.ID == "" {
			fault.ID = uuid.NewString()
		}
		fault.OrganizationID = orgID
		fault.SessionAnalysisID = sessionAnalysisID
	}
	if err := db.Create(&faults).Error; err != nil {
		return fmt.Errorf("insert round-trip faults (org %s, session %s): %w", orgID, sessionAnalysisID, err)
	}
	return nil
}

// ListBySessionAnalysisID returns a session's round-trip faults, ordered by prompt index. It
// keys on the seat-scoped parent's id, so it exposes no org-wide read path over this table.
//
// Reader invariant: a JUDGE_FAILED terminal stamp nulls the parent's evidence_roundtrip_disposition
// but does NOT delete the fault rows (they belong to E, and a content-change reopen reuses the same
// session_analysis_id), so this can return a prior generation's faults for a row whose current
// disposition is NULL. A serving reader MUST gate on the parent's evidence_roundtrip_disposition
// (PRODUCED) before trusting these rows — do not surface them for a NULL/absent disposition.
func (r *SessionRoundTripFaultRepo) ListBySessionAnalysisID(ctx context.Context, sessionAnalysisID string) ([]*model.SessionRoundTripFault, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionRoundTripFaultRepo", "ListBySessionAnalysisID", time.Now())
	var faults []*model.SessionRoundTripFault
	err := planscoped.DB(ctx, r.db).
		Where("session_analysis_id = ? AND deleted_at IS NULL", sessionAnalysisID).
		Order("prompt_index ASC, id ASC").
		Find(&faults).Error
	if err != nil {
		return nil, fmt.Errorf("list session round-trip faults by session analysis (%s): %w", sessionAnalysisID, err)
	}
	return faults, nil
}
