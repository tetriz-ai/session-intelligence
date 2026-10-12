// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/lineage"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// prefetchGate resolves the cost-tiered pre-fetch skip gates from raw_records alone
// (no S3): CHILD (a delegated child folds via its parent), OUT_OF_WINDOW (the
// conversation day is before the org's data-window cutoff), NO_SEAT (the person is
// not seated under a resolved-active seat cap), and REPO_UNTRACKED_METADATA (the
// upload's named repo is not in the tracked set). The gates are tri-state through the
// planscoped helpers: a resolved-inactive gate (unlimited seats / unbounded window)
// fails open and admits the record. The check order is the re-admission order — a
// record both out-of-window and unseated binds OUT_OF_WINDOW first, so a window
// widen re-admits it to the NO_SEAT gate next, matching the scan's category-7 →
// category-6 progression. Returns the binding reason and whether a gate fired.
func (p *SessionAnalysisProcessor) prefetchGate(ctx context.Context, record *model.RawRecordModel, meta *rawRecordMetadata, params dao.SessionAnalysisScanParams) (sessionintelligencepb.SessionAnalysisSkipReason, bool) {
	// Classify child lineage by the shared lineage.IsChild classifier — a positive
	// list of genuine child relation types (SUBAGENT / PLUGIN / HOOK), exactly as the
	// session fold does — not the presence of any relation_type or parent_record_key.
	// A caller sending relation_type=PRIMARY (or empty) is a real top-level session,
	// not a child to fold away.
	if lineage.IsChild(meta.RelationType) {
		return sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD, true
	}
	if cfg, ok := planscoped.ConfigFromContext(ctx); ok {
		if cutoff, capped := planscoped.DataCutoff(cfg); capped && startOfDayUTC(record.DataDate).Before(cutoff) {
			return sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW, true
		}
	}
	if !planscoped.HasSeatPersonID(ctx, meta.PersonID) {
		return sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT, true
	}
	// REPO_UNTRACKED fails OPEN, by contract, until the org's tracked-repo set is resolvable in
	// this scope: the gate fires only when RepoFilterEnabled is resolved-active, and no caller
	// resolves it today, so every repo is admitted (processed, never dropped). Fail-open is the
	// safe direction — a false skip would silently withhold a seated person's real session,
	// whereas admitting an untracked repo only spends analysis on a session the read side can
	// still repo-scope out. Wiring the gate needs the org's tracked-repo set resolved per tick
	// with the seat gate's tri-state discipline (resolved-active fires; resolved-inactive —
	// no repo filter — fails open; unresolved defers, never allow-all), threaded into
	// params.TrackedRepos/RepoFilterEnabled; that resolver is not part of this processor's
	// scope. The post-parse REPO_UNTRACKED_PROJECT leg is likewise unbuilt
	// and fails open the same way.
	if params.RepoFilterEnabled && meta.RepoName != "" && !containsString(params.TrackedRepos, meta.RepoName) {
		return sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_METADATA, true
	}
	return sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_UNSPECIFIED, false
}

// applySkip writes the conditional SKIPPED state row stamped with the binding gate
// and its re-admission key, in one tenant scope — no S3, no LLM. Every skip is
// conditional; the scan re-admits it the moment its gate would pass. For a CHILD it
// also advances the parent's children_seq so a parent that already folded its child set
// is re-admitted by the child-refold scan leg to re-fold.
func (p *SessionAnalysisProcessor) applySkip(ctx context.Context, orgID string, record *model.RawRecordModel, meta *rawRecordMetadata, reason sessionintelligencepb.SessionAnalysisSkipReason) error {
	// Price a delegated child BEFORE the phase-5 compat 'processed' stamp. The parent's usage fold
	// reads cost_usd_micros + the deduped tokens off the child's raw_records.metadata, and those
	// keys are written ONLY by StampChildCost — the SessionIntelligenceProcessor does it during the parallel
	// run, but once the global compat flag flips (Session retires), the engine marks the child
	// processed itself, so it must own the pricing too or the parent folds zero cost and a raw,
	// non-deduped token total. Gated on the SAME flag as the compat stamp (one cutover concern) and
	// run in its own scope ahead of the skip transaction (an S3 read, ). A pricing failure
	// fails the skip (retryable) rather than marking an unpriced child processed — Session's rule.
	isChild := reason == sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD
	var childPayload *rawRecordPayload
	if isChild {
		parsed, err := parsePayload(record.Payload)
		if err != nil {
			return fmt.Errorf("parse child payload (raw_record %s): %w", record.ID, err)
		}
		childPayload = parsed
	}
	if isChild && p.cfg.CompatWriteEnabled() {
		if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
			if _, stampErr := sessionintelligence.StampChildCost(sctx, p.s3Client, p.rawRecordDAO, record, childPayload.S3Key); stampErr != nil {
				return fmt.Errorf("stamp child cost: %w", stampErr)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("pricing child before compat stamp (raw_record %s): %w", record.ID, err)
		}
	}
	// Write the delegated child's measured turn rows before MarkSkipped, so a grouping
	// failure fails the skip (retryable) rather than marking a child skipped with no turns
	//. Gated — per decision A — by the same window/seat
	// posture a primary session gets, so a child the read side would exclude gets no rows.
	// The S3 read + parse run in their own scope, ahead of the skip transaction.
	if isChild && p.childInWindowAndSeated(ctx, record, meta) {
		if err := p.writeChildMeasuredTurns(ctx, orgID, record, meta, childPayload); err != nil {
			return fmt.Errorf("grouping child before skip (raw_record %s): %w", record.ID, err)
		}
	}
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		state := &model.SessionAnalysisState{
			OrganizationID:        orgID,
			RawRecordID:           record.ID,
			ConversationStartedAt: record.DataDate,
			VocabularyVersion:     versionForDate(p.cfg, record.DataDate),
			RepoName:              nilIfEmpty(meta.RepoName),
		}
		if meta.PersonID != "" {
			state.PersonID = &meta.PersonID
		}
		if isChild {
			// Stamp the content anchor on the CHILD skip (from the payload — no S3), so a later
			// body change is detectable by the child-reprice scan leg. Legacy children (no payload
			// sha) fall back to the raw record's updated_at, as the rest of the engine does.
			state.SourceContentSha256 = nilIfEmpty(childPayload.ContentSha256)
			updatedAt := record.UpdatedAt
			state.SourceUpdatedAt = &updatedAt
		}
		inserted, err := p.sessionStateDAO.MarkSkipped(sctx, state, reason)
		if err != nil {
			return fmt.Errorf("mark skipped (%s): %w", reason.String(), err)
		}
		// Phase-5 compat write (a no-op unless the global flag is on): every conditional
		// skip maps to processing_status 'processed', reproducing the old processor's
		// contract (its seat/window/repo scoping was serving-side), in this skip's
		// transaction.
		if err := p.stampCompatProcessingStatus(sctx, orgID, record.ID, record.DataDate, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED); err != nil {
			return fmt.Errorf("compat write (skipped): %w", err)
		}
		if isChild && meta.ParentRecordKey != "" {
			switch {
			case inserted:
				// Genuine first sighting: bump the parent fence so it folds this child. MarkSkipped
				// uses ON CONFLICT DO NOTHING for a child, so a concurrent re-sight gets
				// inserted=false and does not double-bump children_seq.
				p.bumpParentFence(sctx, orgID, record.ID, meta.ParentRecordKey)
			case p.cfg.CompatWriteEnabled():
				// A child-reprice re-admit (the only leg that re-sights a still-CHILD row, and only
				// post-cutover): the child was re-priced above. Refresh its content anchor so the
				// reprice leg stops firing, and bump the parent fence so the parent re-folds the new
				// cost (a cost-only change never advanced children_seq on its own).
				if err := p.sessionStateDAO.RefreshChildContentAnchor(sctx, record.ID, nilIfEmpty(childPayload.ContentSha256), record.UpdatedAt); err != nil {
					return fmt.Errorf("refresh child content anchor (raw_record %s): %w", record.ID, err)
				}
				p.bumpParentFence(sctx, orgID, record.ID, meta.ParentRecordKey)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("skip scope (raw_record %s): %w", record.ID, err)
	}
	return nil
}

// pendingDataStillUnresolved re-decides a CategoryPendingData record's E re-pick from the persisted
// state ALONE — no S3, no spine — and reports whether the record can be cheaply re-skipped this tick
// (#6). It returns true only when: E is SKIPPED_PENDING_DATA; R and D are both settled at the current
// contract (so nothing else needs the spine — a contract-bumped R or a newly-enabled D forces the
// full path); the body anchor is available and unchanged; and every pending artifact's intent is
// still unresolved (rePickReSkip). Any read/anchor/resolver problem returns false so the full
// claim+spine+re-pick path runs and handles it — the short-circuit only ever SAVES work, never
// decides a record's fate.
func (p *SessionAnalysisProcessor) pendingDataStillUnresolved(ctx context.Context, orgID string, record *model.RawRecordModel) bool {
	var state *model.SessionAnalysisState
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		loaded, err := p.sessionStateDAO.GetByRawRecordID(sctx, record.ID)
		if err != nil {
			return fmt.Errorf("reading session analysis state for pending-data short-circuit (raw_record %s): %w", record.ID, err)
		}
		state = loaded
		return nil
	}); err != nil {
		// Cannot read the state cheaply — fall through to the full path, which reads it again under
		// the claim and handles the failure there.
		logger.WarnWithCtx(ctx, "session analysis: pending-data short-circuit state read failed; taking the full path",
			zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID), zap.Error(err))
		return false
	}
	if pendingEvidenceCall(state) == nil {
		return false
	}
	// A producer other than E that needs the spine forces the full path: a contract-bumped R must
	// re-infer, and a D whose flag just turned on must run. producerSettled admits DONE and NOT_FIRED
	// (nothing to re-run), and rejects FLAG_SKIPPED / SKIPPED_PENDING_DATA (unsettled).
	if !producerSettled(state, producerRead, p.cfg.ContractVersionRead()) ||
		!producerSettled(state, producerDiscard, p.cfg.ContractVersionDiscard()) {
		return false
	}
	payload, err := parsePayload(record.Payload)
	if err != nil {
		// A malformed payload cannot short-circuit; the full path surfaces it. Log rather than swallow.
		logger.WarnWithCtx(ctx, "session analysis: pending-data short-circuit payload parse failed; taking the full path",
			zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID), zap.Error(err))
		return false
	}
	if payload.ContentSha256 == "" {
		return false // no content anchor without the S3 body — let the full path resolve it
	}
	resolver := p.evidenceResolver
	if resolver == nil {
		resolver = defaultEvidenceResolver
	}
	decision, err := evidenceRePickDecision(state, payload.ContentSha256, func(artifactID string) (bool, error) {
		return resolver(ctx, orgID, artifactID)
	})
	if err != nil {
		// A transient intent-read fault: the full path re-resolves and defers non-counting. Log it.
		logger.WarnWithCtx(ctx, "session analysis: pending-data short-circuit intent read failed; taking the full path",
			zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID), zap.Error(err))
		return false
	}
	return decision == rePickReSkip
}
