// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// SecurityFlagRepo implements ISecurityFlagDAO using PostgreSQL via GORM.
type SecurityFlagRepo struct {
	db *gorm.DB
}

// NewSecurityFlagRepo creates a new SecurityFlagRepo.
func NewSecurityFlagRepo(db *gorm.DB) *SecurityFlagRepo {
	return &SecurityFlagRepo{db: db}
}

// DeleteByRawRecordIDAndStatus deletes a raw record's flags that are in the given status.
func (r *SecurityFlagRepo) DeleteByRawRecordIDAndStatus(ctx context.Context, rawRecordID string, status string) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "DeleteByRawRecordIDAndStatus", time.Now())
	db := planscoped.WriteDB(ctx, r.db)
	if err := db.Model(&model.SecurityFlag{}).
		Where("raw_record_id = ? AND status = ? AND deleted_at IS NULL", rawRecordID, status).
		Update("deleted_at", time.Now().UTC()).Error; err != nil {
		return fmt.Errorf("soft-deleting %s security flags for raw record %s: %w", status, rawRecordID, err)
	}
	return nil
}

// ListOpenByRawRecordID returns the raw record's live open flags — see ISecurityFlagDAO.
func (r *SecurityFlagRepo) ListOpenByRawRecordID(ctx context.Context, rawRecordID string) ([]*model.SecurityFlag, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "ListOpenByRawRecordID", time.Now())
	db := planscoped.DB(ctx, r.db)
	var results []*model.SecurityFlag
	err := db.Where("raw_record_id = ? AND status = 'open'", rawRecordID).Find(&results).Error
	if err != nil {
		return nil, fmt.Errorf("listing open security flags for raw record %s: %w", rawRecordID, err)
	}
	return results, nil
}

// ReconcileOpenFlags replaces a raw record's OPEN security flags with the producer's
// fresh set: it soft-deletes the record's open flags and writes freshFlags in their
// place, so a flag no longer detected is dropped and a still-detected one is refreshed.
// It is the one reconcile every security_flags writer calls (the session-analysis and the
// legacy Session processor), and the last writer for a raw_record wins — there is no
// cross-writer flag preservation. The delete+insert run in ONE transaction: RunInTx opens
// it on r.db when the caller has none, and joins the caller's transaction when the reconcile
// runs inside a wider write (a commit), so a partial write never lands beside a committed
// judged/processed status.
//
// Because it is a replace-set (soft-delete then insert), a still-detected flag gets a fresh
// row each reconcile: its created_at is the LAST reconcile time, not a first-seen timestamp,
// and the readers here order by flag_date/id and never read created_at. Treat created_at as
// the last-reconcile time, not first detection.
func (r *SecurityFlagRepo) ReconcileOpenFlags(ctx context.Context, rawRecordID string, freshFlags []*model.SecurityFlag) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "ReconcileOpenFlags", time.Now())

	return tenantdb.RunInTx(ctx, r.db, func(ctx context.Context) error {
		// Two processors reconcile the same raw_record; without serialization their
		// delete+insert can interleave — each deletes before the other's insert is visible,
		// so both sets survive and the replace-set leaves stale rows. The transaction-scoped
		// advisory lock, taken on this reconcile's own transaction and held until it
		// commits or rolls back, serializes concurrent reconciles of one raw_record_id, so
		// the reconcile is a clean last-writer-wins replace-set. Holding the delete and
		// insert in that same transaction also makes them atomic: a crash between them never
		// drops an open flag the producer still reports — both land or both roll back.
		if err := planscoped.WriteDB(ctx, r.db).Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "security_flags:reconcile:"+rawRecordID).Error; err != nil {
			return fmt.Errorf("advisory lock for security flag reconcile of raw record %s: %w", rawRecordID, err)
		}

		if err := r.DeleteByRawRecordIDAndStatus(ctx, rawRecordID, "open"); err != nil {
			return fmt.Errorf("delete open security flags for raw record %s: %w", rawRecordID, err)
		}
		if len(freshFlags) == 0 {
			return nil
		}
		if err := r.CreateBatch(ctx, freshFlags); err != nil {
			return fmt.Errorf("creating reconciled security flags batch for raw record %s: %w", rawRecordID, err)
		}
		return nil
	})
}

// CreateBatch upserts each flag: it updates the live row for (raw_record_id, flag_type) or
// inserts a fresh one — a per-row update-or-insert loop, not a single batched statement.
func (r *SecurityFlagRepo) CreateBatch(ctx context.Context, flags []*model.SecurityFlag) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "CreateBatch", time.Now())
	if len(flags) == 0 {
		return nil
	}
	db := planscoped.WriteDB(ctx, r.db)
	now := time.Now().UTC()
	for _, f := range flags {
		if f.CreatedAt.IsZero() {
			f.CreatedAt = now
		}
		f.UpdatedAt = now
	}
	// For each flag, update the live row for (raw_record_id, flag_type) or insert a fresh one.
	// This preserves admin-set status (resolved/false_positive) while keeping flag_details and
	// severity current. The update-then-insert is not atomic on its own; ReconcileOpenFlags —
	// the production caller — serializes concurrent reconciles of one raw_record under its
	// pg_advisory_xact_lock, so the two steps do not race. The partial unique index on
	// (raw_record_id, flag_type) WHERE deleted_at IS NULL is the backstop: a caller that bypasses
	// that lock and races a duplicate gets a surfaced unique-violation error, never a duplicate row.
	for _, f := range flags {
		updates := map[string]interface{}{
			"flag_details": f.FlagDetails,
			"severity":     f.Severity,
			"updated_at":   f.UpdatedAt,
		}
		result := db.Model(&model.SecurityFlag{}).
			Where("raw_record_id = ? AND flag_type = ? AND deleted_at IS NULL", f.RawRecordID, f.FlagType).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("update security flag (%s, %s): %w", f.RawRecordID, f.FlagType, result.Error)
		}
		if result.RowsAffected == 0 {
			if err := db.Create(f).Error; err != nil {
				return fmt.Errorf("insert security flag (%s, %s): %w", f.RawRecordID, f.FlagType, err)
			}
		}
	}
	return nil
}

// ListByOrg returns all security flags for an organization.
func (r *SecurityFlagRepo) ListByOrg(ctx context.Context, orgID string, limit int) ([]*model.SecurityFlag, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "ListByOrg", time.Now())
	db := planscoped.DB(ctx, r.db)
	var results []*model.SecurityFlag
	err := db.Where("organization_id = ?", orgID).
		Order("flag_date DESC, id DESC").
		Limit(limit).
		Find(&results).Error
	if err != nil {
		return nil, fmt.Errorf("listing security flags for org %s: %w", orgID, err)
	}
	return results, nil
}

// ListByPerson returns all security flags for a person in an organization.
func (r *SecurityFlagRepo) ListByPerson(ctx context.Context, orgID, personID string, limit int) ([]*model.SecurityFlag, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "ListByPerson", time.Now())
	db := planscoped.DB(ctx, r.db)
	var results []*model.SecurityFlag
	err := db.Where("organization_id = ? AND person_id = ?", orgID, personID).
		Order("flag_date DESC, id DESC").
		Limit(limit).
		Find(&results).Error
	if err != nil {
		return nil, fmt.Errorf("listing security flags for org %s person %s: %w", orgID, personID, err)
	}
	return results, nil
}

// GetByID returns the security flag with the given id.
func (r *SecurityFlagRepo) GetByID(ctx context.Context, id string) (*model.SecurityFlag, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "GetByID", time.Now())
	db := planscoped.DB(ctx, r.db)
	var result model.SecurityFlag
	err := db.Where("id = ?", id).First(&result).Error
	if err != nil {
		return nil, fmt.Errorf("getting security flag %s: %w", id, err)
	}
	return &result, nil
}

// CountOpenByOrg returns the number of open security flags for an organization.
func (r *SecurityFlagRepo) CountOpenByOrg(ctx context.Context, orgID string) (int64, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SecurityFlagRepo", "CountOpenByOrg", time.Now())
	db := planscoped.DB(ctx, r.db)
	var count int64
	err := db.Model(&model.SecurityFlag{}).
		Where("organization_id = ? AND status = 'open'", orgID).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("counting open security flags for org %s: %w", orgID, err)
	}
	return count, nil
}
