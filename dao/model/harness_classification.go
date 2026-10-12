// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

import (
	"time"

	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
)

// HarnessArtifactClassification is the classified intent of a harness artifact.
type HarnessArtifactClassification struct {
	ID                string                                `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID    string                                `gorm:"column:organization_id;type:uuid;not null"`
	Task              coreaidlcv1.HarnessClassificationTask `gorm:"column:task;type:text;not null"`
	HarnessArtifactID string                                `gorm:"column:harness_artifact_id;type:uuid;not null"`
	// VerdictsByVersion is NOT NULL (default '{}'); a nil pointer would serialise
	// to SQL NULL and fail the constraint — the repo substitutes an empty wrapper.
	VerdictsByVersion *coreaidlcv1.HarnessClassificationVerdicts `gorm:"column:verdicts_by_version;type:jsonb;not null"`
	// OverrideVerdict, when set, wins over every version entry. It is paired with
	// OverrideSetByPersonID (DB CHECK: both set or both NULL).
	OverrideVerdict       *coreaidlcv1.HarnessClassificationVerdict `gorm:"column:override_verdict;type:jsonb"`
	OverrideSetByPersonID *string                                   `gorm:"column:override_set_by_person_id;type:uuid"`
	OverrideReason        *string                                   `gorm:"column:override_reason;type:text"`
	// AttemptingVersion, AttemptingDigest and LeaseExpiresAt are all set while an
	// attempt is IN_FLIGHT and NULL otherwise (DB CHECK ties them to AttemptState);
	// they are cleared only by a conclusion.
	AttemptingVersion *string                                       `gorm:"column:attempting_version;type:text"`
	AttemptingDigest  *string                                       `gorm:"column:attempting_digest;type:text"`
	AttemptState      coreaidlcv1.HarnessClassificationAttemptState `gorm:"column:attempt_state;type:text;not null"`
	AttemptCount      int                                           `gorm:"column:attempt_count;not null"`
	LeaseExpiresAt    *time.Time                                    `gorm:"column:lease_expires_at"`
	LeaseGeneration   int64                                         `gorm:"column:lease_generation;not null"`
	// The five IntentClass* fields are written only by the intent-classify sweep — the
	// ingest/seed Upsert and EnsureIdleRows column lists must never name them, exactly as
	// for VerdictsByVersion. All nullable; NULL means unclassified.
	IntentClass                   *coreaidlcv1.ArtifactIntentClass `gorm:"column:intent_class;type:text"`
	IntentClassSource             *coreaidlcv1.IntentClassSource   `gorm:"column:intent_class_source;type:text"`
	IntentClassifierVersion       *int                             `gorm:"column:intent_classifier_version"`
	IntentClassifiedFromInputHash *string                          `gorm:"column:intent_classified_from_input_hash;type:text"`
	IntentClassifiedAt            *time.Time                       `gorm:"column:intent_classified_at"`
	CreatedAt                     time.Time                        `gorm:"column:created_at"`
	UpdatedAt                     time.Time                        `gorm:"column:updated_at"`
}
