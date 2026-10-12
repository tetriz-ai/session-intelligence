// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package model holds the GORM models for the session and security tables.
package model

import (
	"time"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
)

// HarnessArtifact is a harness artifact the engine reads as evidence input.
type HarnessArtifact struct { //allow:not-seat-scoped org-wide harness config inventory (harness-control-plane-observe design)
	ID             string `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string `gorm:"column:organization_id;type:uuid;not null"`
	// PersonID is the observing engineer for a DESKTOP row; NULL for a VCS row
	// (org-grain, person-less), enforced by harness_artifacts_person_by_source_chk. A
	// pointer so GORM writes SQL NULL, not '', for the person-less case.
	PersonID *string `gorm:"column:person_id;type:uuid"`

	IdeType commonv1.IdeType              `gorm:"column:ide_type;not null"`
	Scope   commonv1.HarnessArtifactScope `gorm:"column:scope;not null"`
	Type    commonv1.HarnessArtifactType  `gorm:"column:type;not null"`
	Name    string                        `gorm:"column:name;not null"`

	// RepositoryID is the resolved vcs_repositories.id; NULL only for USER_GLOBAL
	// (enforced by the harness_artifacts_scope_repo_chk CHECK).
	RepositoryID *string `gorm:"column:repository_id;type:uuid"`
	SourcePath   string  `gorm:"column:source_path;not null"`

	CurrentFileID string `gorm:"column:current_file_id;type:uuid;not null"`
	// BaselineFileID is the committed version's body; set only for REPO_COMMITTED.
	BaselineFileID *string `gorm:"column:baseline_file_id;type:uuid"`
	// DeviationState is NULL unless Scope=REPO_COMMITTED, so it is a pointer with a
	// TEXT column (matches the nullable-proto-enum convention; the gen_sql Scanner
	// is only invoked for a non-NULL value).
	DeviationState   *commonv1.HarnessDeviationState  `gorm:"column:deviation_state;type:text"`
	OperationalState commonv1.HarnessOperationalState `gorm:"column:operational_state;not null"`

	// An artifact's loop type is NOT a field here. It lives on the verdict store
	// (loop_artifacts), which is the only writer that can establish one — a classifier
	// reading the artifact's own body, or an operator override. The columns of the same name
	// on this table are the shipped-seed remnant this model no longer writes or reads; they
	// are dropped in the release after this one, once no running pod still supplies them.

	// HookConfig is the observed settings.json activation (event/matcher/timeout);
	// set only for Type=HOOK (harness_artifacts_hook_config_chk), and NULL for a
	// pre-hook-config client that reports no config. Proto message JSONB.
	HookConfig *commonv1.HarnessHookConfig `gorm:"column:hook_config;type:jsonb"`

	// ArtifactSource discriminates a DESKTOP row (per-engineer observed state) from a
	// VCS row (org-grain committed content). person_id is NULL iff this is VCS, enforced
	// by harness_artifacts_person_by_source_chk. Proto enum name string.
	ArtifactSource commonv1.HarnessArtifactSource `gorm:"column:artifact_source;not null"`
	// Branch is the TreeBranch a VCS row's content was fetched from; NULL for a DESKTOP
	// row (no branch concept). Labels the row by the branch actually fetched.
	Branch *string `gorm:"column:branch"`
	// Files is the per-file manifest of a directory artifact — the primary plus every
	// helper. Proto message JSONB; mirrors HookConfig. The `default` tag makes
	// GORM omit a nil manifest on insert so the column's NOT NULL '{}' default fires for a
	// single-file artifact, whose primary is the whole install.
	//
	// Both sources write it, and they populate different subsets of each entry: a VCS entry
	// carries blob_sha and the generation it was last written at (its change-detection and
	// per-file monotonicity guards), while a desktop entry has no git blob and no per-file
	// generation and leaves both zero. A reader wanting "which files does this artifact
	// consist of" may use either; one wanting a change baseline may not.
	// ContentGeneration is the artifact-level resurrection guard: a delayed record may clear
	// deleted_at only if its generation exceeds this. 0 for a DESKTOP row (per-file grain
	// there). Stamped from the VCS record's one generation (the tree's tree_fetched_at epoch).
	ContentGeneration int64 `gorm:"column:content_generation;not null"`

	CreatedAt    time.Time      `gorm:"column:created_at"`
	LastSyncedAt time.Time      `gorm:"column:last_synced_at"`
	DeletedAt    gorm.DeletedAt `gorm:"column:deleted_at"`
}
