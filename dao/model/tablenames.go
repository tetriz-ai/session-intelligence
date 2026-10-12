// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

// TableName returns the table name for HarnessArtifact.
func (HarnessArtifact) TableName() string { return "harness_artifacts" }

// TableName returns the table name for RawRecordModel.
func (RawRecordModel) TableName() string {
	return "raw_records"
}

// TableName returns the table name for SecurityFlag.
func (SecurityFlag) TableName() string {
	return "security_flags"
}

// TableName returns the table name for HarnessArtifactClassification.
func (HarnessArtifactClassification) TableName() string {
	return "harness_artifact_classifications"
}
