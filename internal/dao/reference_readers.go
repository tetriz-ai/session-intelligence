// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

// Reference gorm-backed readers for the engine's input-side interfaces. They read
// the input tables a host populates (raw session records, harness artifacts, their
// classifications). A host may swap in its own implementation of the interfaces;
// these are the batteries-included default the examples and tests use.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	"github.com/tetriz-ai/session-intelligence/internal/lineage"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// subagentUsageWindowDays bounds how many daily partitions the child fold scans
// forward of the parent's date.
const subagentUsageWindowDays = 30

// foldedChildRelationTypes are the child relations whose cost/tokens fold into the parent.
var foldedChildRelationTypes = []string{lineage.RelationTypeSubagent, lineage.RelationTypePlugin, lineage.RelationTypeHook}

// RawRecordRepo is the reference reader for session raw records.
type RawRecordRepo struct {
	db *gorm.DB
}

// NewRawRecordRepo returns a reference IRawRecordDAO backed by db.
func NewRawRecordRepo(db *gorm.DB) *RawRecordRepo { return &RawRecordRepo{db: db} }

// StampProcessingStatusOnly writes only the processing status of a raw record for a date.
func (r *RawRecordRepo) StampProcessingStatusOnly(ctx context.Context, id, orgID string, dataDate time.Time, status model.ProcessingStatus) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "StampProcessingStatusOnly", time.Now())
	db := tenantdb.DB(ctx, r.db)
	return db.Exec(
		"UPDATE raw_records SET processing_status = ? WHERE id = ? AND organization_id = ? AND data_date = ?",
		status, id, orgID, dataDate,
	).Error
}

// GetByIDForDate returns the raw record with the given id on the given date partition.
func (r *RawRecordRepo) GetByIDForDate(ctx context.Context, id string, dataDate time.Time) (*model.RawRecordModel, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "GetByIDForDate", time.Now())
	db := tenantdb.DB(ctx, r.db)
	var result model.RawRecordModel
	err := db.Where("id = ? AND data_date = ?", id, dataDate.UTC()).First(&result).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get raw record by id+date: %w", err)
	}
	return &result, nil
}

// CurrentContentDigest returns the stored content digest of a raw record.
func (r *RawRecordRepo) CurrentContentDigest(ctx context.Context, id string, dataDate time.Time) (string, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "CurrentContentDigest", time.Now())
	var digest string
	res := tenantdb.DB(ctx, r.db).
		Model(&model.RawRecordModel{}).
		Where("id = ? AND data_date = ?", id, dataDate.UTC()).
		Select("COALESCE(payload->>'content_sha256', '')").
		Scan(&digest)
	if res.Error != nil {
		return "", fmt.Errorf("get raw record content digest by id+date (%s): %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return "", ErrNotFound
	}
	return digest, nil
}

// CurrentSourceUpdatedAt returns the source-updated timestamp of a raw record.
func (r *RawRecordRepo) CurrentSourceUpdatedAt(ctx context.Context, id string, dataDate time.Time) (time.Time, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "CurrentSourceUpdatedAt", time.Now())
	var updatedAt time.Time
	res := tenantdb.DB(ctx, r.db).
		Model(&model.RawRecordModel{}).
		Where("id = ? AND data_date = ?", id, dataDate.UTC()).
		Select("updated_at").
		Scan(&updatedAt)
	if res.Error != nil {
		return time.Time{}, fmt.Errorf("get raw record updated_at by id+date (%s): %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return time.Time{}, ErrNotFound
	}
	return updatedAt, nil
}

// GetByRecordKey returns the raw record matching a natural record key.
func (r *RawRecordRepo) GetByRecordKey(ctx context.Context, orgID, connectorID, recordKey string) (*model.RawRecordModel, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "GetByRecordKey", time.Now())
	db := tenantdb.DB(ctx, r.db)
	var result model.RawRecordModel
	err := db.Where("organization_id = ? AND connector_id = ? AND record_key = ?", orgID, connectorID, recordKey).
		First(&result).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get raw record by record key: %w", err)
	}
	return &result, nil
}

// UpdateMetadata updates the mutable metadata fields of a raw record.
func (r *RawRecordRepo) UpdateMetadata(ctx context.Context, id string, metadata json.RawMessage) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "UpdateMetadata", time.Now())
	db := tenantdb.DB(ctx, r.db)
	result := db.Model(&model.RawRecordModel{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"metadata":   metadata,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return fmt.Errorf("update raw record metadata: %w", result.Error)
	}
	return nil
}

// AggregateSubagentUsageByParent sums delegated sub-agent usage grouped by parent record.
func (r *RawRecordRepo) AggregateSubagentUsageByParent(ctx context.Context, orgID, personID, parentRecordKey string, parentDataDate time.Time) ([]SubagentUsageRow, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "RawRecordRepo", "AggregateSubagentUsageByParent", time.Now())
	if orgID == "" || personID == "" || parentRecordKey == "" {
		return nil, fmt.Errorf("aggregating subagent usage: orgID, personID and parentRecordKey cannot be empty")
	}
	from := parentDataDate.AddDate(0, 0, -1)
	to := parentDataDate.AddDate(0, 0, subagentUsageWindowDays)
	var rows []SubagentUsageRow
	err := tenantdb.DB(ctx, r.db).
		Table("raw_records").
		Select("metadata->>'origin_agent_type' AS agent_type, "+
			"metadata->>'plugin_name' AS plugin_name, "+
			"COALESCE(SUM(NULLIF(metadata->>'tokens', '')::bigint), 0) AS tokens, "+
			"COALESCE(SUM(NULLIF(metadata->>'cost_usd_micros', '')::bigint), 0) AS cost_usd_micros, "+
			"COUNT(*) AS count, "+
			"MAX(updated_at) AS latest_updated_at").
		Where("organization_id = ?", orgID).
		Where("data_date >= ? AND data_date <= ?", from, to).
		Where("metadata->>'person_id' = ?", personID).
		Where("metadata->>'parent_record_key' = ?", parentRecordKey).
		Where("metadata->>'relation_type' IN ?", foldedChildRelationTypes).
		Where("metadata->>'linkage_confidence' = ?", lineage.LinkageConfidenceStrong).
		Where("deleted_at IS NULL").
		Group("metadata->>'origin_agent_type', metadata->>'plugin_name'").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("folding children for parent %s: %w", parentRecordKey, err)
	}
	return rows, nil
}

// HarnessArtifactRepo is the reference reader for the org's harness artifact inventory.
type HarnessArtifactRepo struct {
	db *gorm.DB
}

// NewHarnessArtifactRepo returns a reference IHarnessArtifactDAO backed by db.
func NewHarnessArtifactRepo(db *gorm.DB) *HarnessArtifactRepo { return &HarnessArtifactRepo{db: db} }

// ListLiveForSource returns the live harness artifacts for a source.
func (r *HarnessArtifactRepo) ListLiveForSource(
	ctx context.Context, orgID string, ideType commonv1.IdeType, artifactType commonv1.HarnessArtifactType,
	artifactSource commonv1.HarnessArtifactSource, personID *string,
) ([]*model.HarnessArtifact, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "HarnessArtifactRepo", "ListLiveForSource", time.Now())
	if orgID == "" {
		return nil, fmt.Errorf("listing live harness artifacts for source: orgID cannot be empty")
	}
	db := tenantdb.DB(ctx, r.db)
	query := db.
		Where("organization_id = ? AND ide_type = ? AND type = ? AND artifact_source = ?",
			orgID, ideType.String(), artifactType.String(), artifactSource.String())
	if personID == nil {
		query = query.Where("person_id IS NULL")
	} else {
		query = query.Where("person_id = ?", *personID)
	}
	var out []*model.HarnessArtifact
	if err := query.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("listing live harness artifacts for source for org %s: %w", orgID, err)
	}
	return out, nil
}

// HarnessArtifactClassificationRepo is the reference reader for artifact classifications.
type HarnessArtifactClassificationRepo struct {
	db *gorm.DB
}

// NewHarnessArtifactClassificationRepo returns a reference IHarnessArtifactClassificationDAO.
func NewHarnessArtifactClassificationRepo(db *gorm.DB) *HarnessArtifactClassificationRepo {
	return &HarnessArtifactClassificationRepo{db: db}
}

// GetByArtifact returns the classification for a harness artifact.
func (r *HarnessArtifactClassificationRepo) GetByArtifact(ctx context.Context, orgID string, task coreaidlcv1.HarnessClassificationTask, artifactID string) (*model.HarnessArtifactClassification, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "HarnessArtifactClassificationRepo", "GetByArtifact", time.Now())
	var row model.HarnessArtifactClassification
	err := tenantdb.DB(ctx, r.db).
		Where("organization_id = ? AND task = ? AND harness_artifact_id = ?", orgID, task.String(), artifactID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting harness_artifact_classification for org %s artifact %s: %w", orgID, artifactID, err)
	}
	return &row, nil
}
