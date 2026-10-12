// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// SessionAnalysisStateRepo implements ISessionAnalysisStateDAO for
// session_analysis_state. The state table carries tenant_isolation only (no
// data_window, not plan-scoped), so it reads and writes through tenantdb.DB — the
// actionable-work scan must see out-of-window rows the content tables hide. The repo's
// methods span this file plus session_analysis_state_scan.go (the actionable scan,
// ClaimForProcessing, and the CAS state-machine transitions) and the reprocess file.
type SessionAnalysisStateRepo struct {
	db *gorm.DB
}

// sessionAnalysisStateCopyColumns is every column the Session backfill owns on a copied
// state row — refreshed by a deliberate re-copy. It excludes the keys and origin (the copy
// discriminator, which stays SESSION_COPY and is the DO-UPDATE guard).
var sessionAnalysisStateCopyColumns = []string{
	"person_id", "conversation_started_at", "status", "vocabulary_version", "spine_version",
	"contract_version_read", "contract_version_evidence", "contract_version_discard",
	"source_content_sha256", "source_updated_at",
	"flag_skipped_producers", "pending_data_producers", "failed_producers",
	// children_seq / folded_children_seq are set to 0 on the initial insert and NOT refreshed on a
	// re-copy: BumpChildrenSeq never advances a copied parent's fence, so there is nothing to
	// re-freeze, and leaving them out means a re-copy can never resurrect a stale fence value.
	"attempt_count",
	"updated_at",
}

// UpsertCopiedState writes one copy-migrated orchestration row (a terminal JUDGED row with origin =
// SESSION_COPY), conflict-targeted on the session_analysis_state_one_live partial unique
// (organization_id, raw_record_id) WHERE status <> SUPERSEDED AND deleted_at IS NULL
// (session-analyses-serving-parity design). It is the same conflict clause the live engine's
// claim uses, so a session the live lane already owns (any live, non-superseded, non-copy state
// row) fails the DO-UPDATE guard (origin <> SESSION_COPY) and is left untouched — the live
// generation wins and this call reports owned == false, which the copy job uses to skip the whole
// pair (dedupe). On conflict with a prior COPY row it refreshes sessionAnalysisStateCopyColumns
// (re-carrying Session's current anchors / versions), so a re-run is idempotent and resumable and
// a deliberate re-copy re-freshens a copied row.
//
// The caller must set OrganizationID, RawRecordID, ConversationStartedAt, Status (JUDGED),
// Origin (SESSION_COPY), VocabularyVersion / SpineVersion / ContractVersion*, the content anchors
// (SourceContentSha256 / SourceUpdatedAt), and the clean producer state (bitmasks 0, ChildrenSeq ==
// FoldedChildrenSeq == 0) — every field a re-admission leg keys on, so the row is terminal. This
// stamps only the id. Runs inside the caller's tenant-scoped transaction.
func (r *SessionAnalysisStateRepo) UpsertCopiedState(ctx context.Context, state *model.SessionAnalysisState) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "UpsertCopiedState", time.Now())
	if state == nil {
		return false, fmt.Errorf("upserting copied session-analysis state: nil state")
	}
	if state.ID == "" {
		state.ID = uuid.NewString()
	}
	// The arbiter predicate INLINES the SUPERSEDED literal (never a bind parameter) for the same
	// reason ClaimForProcessing does: Postgres proves a partial-index conflict target only against a
	// literal predicate, and a bound `status <> $1` fails that inference (42P10) once the statement
	// warms to a generic plan. stSuperseded is a proto enum name, never user input, so inlining is
	// injection-safe. The DO UPDATE fires only for an existing copy row (origin = SESSION_COPY).
	res := tenantdb.DB(ctx, r.db).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "status <> '" + stSuperseded + "' AND deleted_at IS NULL"}}},
		Where:       clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "session_analysis_state.origin = ?", Vars: []any{originSessionCopy}}}},
		DoUpdates:   clause.AssignmentColumns(sessionAnalysisStateCopyColumns),
	}).Create(state)
	if res.Error != nil {
		return false, fmt.Errorf("upserting copied session-analysis state (org %s, raw_record %s): %w", state.OrganizationID, state.RawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// NewSessionAnalysisStateRepo creates a new SessionAnalysisStateRepo.
func NewSessionAnalysisStateRepo(db *gorm.DB) *SessionAnalysisStateRepo {
	return &SessionAnalysisStateRepo{db: db}
}

// Create inserts one state row.
func (r *SessionAnalysisStateRepo) Create(ctx context.Context, state *model.SessionAnalysisState) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "Create", time.Now())
	if state == nil {
		return fmt.Errorf("creating session analysis state: nil state")
	}
	if state.ID == "" {
		state.ID = uuid.NewString()
	}
	if err := tenantdb.DB(ctx, r.db).Create(state).Error; err != nil {
		return fmt.Errorf("creating session analysis state (org %s, raw_record %s): %w", state.OrganizationID, state.RawRecordID, err)
	}
	return nil
}

// GetByRawRecordID returns the live (non-superseded) state row for a raw record, or
// ErrNotFound. A vocabulary upgrade lets a SUPERSEDED generation and its live
// replacement coexist for the same raw record (the generation key carries
// vocabulary_version); excluding SUPERSEDED matches the session_analysis_state_one_live
// partial-unique index so this returns the single live row, never an obsolete
// generation by primary-key order.
func (r *SessionAnalysisStateRepo) GetByRawRecordID(ctx context.Context, rawRecordID string) (*model.SessionAnalysisState, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "GetByRawRecordID", time.Now())
	var result model.SessionAnalysisState
	err := tenantdb.DB(ctx, r.db).
		Where("raw_record_id = ? AND status <> ? AND deleted_at IS NULL",
			rawRecordID, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SUPERSEDED.String()).
		First(&result).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get session analysis state by raw record (%s): %w", rawRecordID, err)
	}
	return &result, nil
}
