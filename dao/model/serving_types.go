// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

// Serving and security model types used by the session/security DAOs.

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// SecurityFlag is a persisted security-layer finding on a raw record.
type SecurityFlag struct {
	ID             string                                      `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string                                      `gorm:"column:organization_id;type:uuid;not null"`
	PersonID       string                                      `gorm:"column:person_id;type:uuid;not null"`
	RawRecordID    string                                      `gorm:"column:raw_record_id;type:uuid;not null"`
	FlagType       string                                      `gorm:"column:flag_type;not null"`
	Severity       string                                      `gorm:"column:severity;not null;default:'medium'"`
	FlagDetails    *sessionintelligencepb.SecurityFlagDetails  `gorm:"column:flag_details;type:jsonb"`
	Status         string                                      `gorm:"column:status;not null;default:'open'"`
	ResolvedBy     *string                                     `gorm:"column:resolved_by;type:uuid"`
	ResolvedAt     *time.Time                                  `gorm:"column:resolved_at"`
	Metadata       *sessionintelligencepb.SecurityFlagMetadata `gorm:"column:metadata;type:jsonb"`
	FlagDate       time.Time                                   `gorm:"column:flag_date;type:date;not null"`
	CreatedAt      time.Time                                   `gorm:"column:created_at"`
	UpdatedAt      time.Time                                   `gorm:"column:updated_at"`
	DeletedAt      gorm.DeletedAt                              `gorm:"column:deleted_at"`
}

// RawRecordModel is the raw connector record the engine reads as input.
type RawRecordModel struct {
	ID             string    `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string    `gorm:"column:organization_id;type:uuid;not null"`
	ConnectorID    string    `gorm:"column:connector_id;not null"`
	OperationID    string    `gorm:"column:operation_id;not null"`
	DataDate       time.Time `gorm:"column:data_date;not null"`
	RecordKey      string    `gorm:"column:record_key;not null"`
	// allow:rawjson — raw vendor record; must preserve unknown fields
	Payload   json.RawMessage `gorm:"column:payload;type:jsonb;not null"`
	FetchedAt time.Time       `gorm:"column:fetched_at;not null"`
	// allow:rawjson — opaque vendor metadata; varies per connector/operation
	Metadata         json.RawMessage  `gorm:"column:metadata;type:jsonb;default:'{}'"`
	ProcessingStatus ProcessingStatus `gorm:"column:processing_status;not null;default:'pending'"`
	RetryCount       int              `gorm:"column:retry_count;not null;default:0"`
	ParseError       string           `gorm:"column:parse_error"`
	SyncRunID        *string          `gorm:"column:sync_run_id;type:uuid"`
	CreatedAt        time.Time        `gorm:"column:created_at"`
	UpdatedAt        time.Time        `gorm:"column:updated_at"`
	DeletedAt        gorm.DeletedAt   `gorm:"column:deleted_at"`
}
