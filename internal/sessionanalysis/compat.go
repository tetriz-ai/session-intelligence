// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"
	"time"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// The phase-5 processing_status compatibility write.
// The engine tracks all its own progress in session_analysis_state and otherwise never
// writes raw_records.processing_status; this compat write, gated by the GLOBAL
// CompatWriteEnabled flag, is the sole exception. When on, it reproduces the OLD
// SessionIntelligenceProcessor's processing_status contract on the session rows the engine judges,
// so those rows do not sit at 'pending' — which breaks the loops retry join
// (LoopAnalysisRepo.ListRetryableRejections requires 'processed') and the CDC-to-BQ
// freshness feed. The flag therefore gates who owns that column's writes for a session
// row: SessionIntelligenceProcessor while off, this compat write while on.

// compatProcessingStatus maps a terminal session-analysis state to the
// raw_records.processing_status the old SessionIntelligenceProcessor would have stamped (Decision
// 42). The old processor stamped 'processed' for every session row it handled
// regardless of seat/window/repo (that scoping was serving-side), so JUDGED and EVERY
// engine skip — the conditional NO_SEAT / OUT_OF_WINDOW / REPO_UNTRACKED_* and the
// folded CHILD, all carried on SKIPPED — map to 'processed', preserving the loops-retry
// set and the BQ-visible set; only a genuine hard FAILED maps to 'failed'. Mapping any
// skip to 'skipped' would silently shrink both sets. The ok=false states (PENDING /
// SPINE_DONE / SUPERSEDED / UNSPECIFIED) are non-terminal or not-this-generation and are
// left unstamped — exactly as an in-flight row read 'pending' under the old processor.
func compatProcessingStatus(status sessionintelligencepb.SessionAnalysisStatus) (model.ProcessingStatus, bool) {
	switch status {
	case sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED,
		sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED:
		return model.ProcessingStatusProcessed, true
	case sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED:
		return model.ProcessingStatusFailed, true
	default:
		return "", false
	}
}

// stampCompatProcessingStatus stamps the session raw_record's processing_status to match
// the old processor's contract for a terminal engine state — a no-op unless the global
// phase-5 compat-write flag is on. It runs inside the caller's tenant-scoped transaction,
// so it shares the terminal transition's commit, and uses raw SQL that leaves updated_at
// untouched (the legacy-row staleness signal).
func (p *SessionAnalysisProcessor) stampCompatProcessingStatus(ctx context.Context, orgID, rawRecordID string, dataDate time.Time, status sessionintelligencepb.SessionAnalysisStatus) error {
	if !p.cfg.CompatWriteEnabled() {
		return nil
	}
	target, ok := compatProcessingStatus(status)
	if !ok {
		return nil
	}
	if err := p.rawRecordDAO.StampProcessingStatusOnly(ctx, rawRecordID, orgID, dataDate, target); err != nil {
		return fmt.Errorf("stamping raw_records.processing_status=%s for raw record %s: %w", target, rawRecordID, err)
	}
	return nil
}
