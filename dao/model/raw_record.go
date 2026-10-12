// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

// ProcessingStatus is a raw record's processing lifecycle state.
type ProcessingStatus string

// Processing statuses for a raw record.
const (
	// ProcessingStatusPending marks a raw record awaiting processing.
	ProcessingStatusPending   ProcessingStatus = "pending"
	ProcessingStatusProcessed ProcessingStatus = "processed"
	ProcessingStatusFailed    ProcessingStatus = "failed"
	// ProcessingStatusSkipped — permanent skip (e.g. off-scope repo). Use a
	// specific SkippedXxx variant for skips that should retry on config change.
	ProcessingStatusSkipped ProcessingStatus = "skipped"
	// ProcessingStatusSkippedNoSeat marks a record whose actor has no seat
	// allocation in the org. Re-included in the actionable set so that when
	// an admin grants a seat the record is processed on the next run without
	// a backfill job.
	ProcessingStatusSkippedNoSeat ProcessingStatus = "skipped_no_seat"
	// ProcessingStatusSkippedOutOfWindow marks a record older than the org's
	// data_window cutoff. Skipped during unfreeze drain — frozen orgs don't
	// process during the freeze, but on resume there can be a long backlog
	// where most records pre-date the new cutoff and are never displayable.
	ProcessingStatusSkippedOutOfWindow ProcessingStatus = "skipped_out_of_window"
)
