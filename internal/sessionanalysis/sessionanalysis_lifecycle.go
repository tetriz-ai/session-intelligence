// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// claim takes the record. A no-state candidate is claimed by INSERT ON CONFLICT DO
// NOTHING (the double-claim backstop); an existing row is re-claimed (the lease).
// Returns the prior state (nil for a fresh claim) and the rotated claim token (empty
// when another tick won).
func (p *SessionAnalysisProcessor) claim(ctx context.Context, orgID string, candidate *dao.ActionableCandidate, record *model.RawRecordModel, meta *rawRecordMetadata) (*model.SessionAnalysisState, string, error) {
	var prior *model.SessionAnalysisState
	var token string
	err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		if candidate.Category == dao.CategoryNoState {
			state := &model.SessionAnalysisState{
				OrganizationID:        orgID,
				RawRecordID:           record.ID,
				ConversationStartedAt: record.DataDate,
				VocabularyVersion:     versionForDate(p.cfg, record.DataDate),
				// Stamp repo_name on claim (like person_id) so a processed→JUDGED row carries it,
				// matching the skip path (applySkip) — else only SKIPPED rows have repo_name and the
				// repo-scoped scan legs cannot see a claimed row.
				RepoName: nilIfEmpty(meta.RepoName),
			}
			if meta.PersonID != "" {
				state.PersonID = &meta.PersonID
			}
			claimed, err := p.sessionStateDAO.ClaimForProcessing(sctx, state)
			if err != nil {
				return fmt.Errorf("claiming fresh state (raw_record %s): %w", record.ID, err)
			}
			if !claimed {
				return nil
			}
			token = *state.ClaimToken
			return nil
		}
		existing, err := p.sessionStateDAO.GetByRawRecordID(sctx, record.ID)
		if err != nil {
			return fmt.Errorf("load prior state: %w", err)
		}

		// A vocabulary bump must NOT reprocess the old generation in place: its
		// enum meanings changed, so the old and new generations must coexist (trends
		// never cross the boundary). Supersede the old generation (state + content) and
		// claim a FRESH generation at the new vocab. This also bypasses the spine-only
		// path — a vocab bump leaves the content unchanged, so ReadInputHash is
		// unchanged, and without this the row would be classified spine-only, restore
		// the old vocab, and stay version-behind forever (the new vocab never applied).
		newVocab := versionForDate(p.cfg, record.DataDate)
		if candidate.Category == dao.CategoryVersionBehind && newVocab > existing.VocabularyVersion {
			claimToken, supErr := p.supersedeAndReclaimFreshGeneration(sctx, orgID, record, meta, existing.VocabularyVersion, newVocab)
			if supErr != nil {
				return fmt.Errorf("superseding + claiming fresh generation (raw_record %s): %w", record.ID, supErr)
			}
			// prior stays nil: the fresh generation is a first-time analysis, run full.
			token = claimToken
			return nil
		}

		prior = existing
		resetAttempts := candidate.Category == dao.CategoryContentChanged || candidate.Category == dao.CategoryVersionBehind
		reclaimed, err := p.sessionStateDAO.Reclaim(sctx, record.ID, existing.ClaimToken, resetAttempts)
		if err != nil {
			return fmt.Errorf("reclaiming state (raw_record %s): %w", record.ID, err)
		}
		if !reclaimed {
			return nil
		}
		// A content-change reopen invalidates the served verdict: the live session_analyses
		// content still carries a non-null judged_at for the OLD body, so if this re-run
		// then fails, sessionServingWhere would keep serving the stale verdict for the
		// now-changed conversation. Clear judged_at (in this same tenant transaction as the
		// reclaim) so the changed session drops out of serving until it is re-judged; a
		// successful re-judge restamps it in place. A version-behind reopen is NOT
		// invalidated — its content is unchanged, so the prior verdict stays correct while
		// it re-runs (the spine-only measured path must keep the prior judged verdict).
		if candidate.Category == dao.CategoryContentChanged {
			if _, err := p.sessionAnalysisDAO.ClearServedVerdict(sctx, record.ID); err != nil {
				return fmt.Errorf("clearing served verdict on content-change reopen (raw_record %s): %w", record.ID, err)
			}
		}
		refreshed, err := p.sessionStateDAO.GetByRawRecordID(sctx, record.ID)
		if err != nil {
			return fmt.Errorf("reload claimed state: %w", err)
		}
		if refreshed.ClaimToken != nil {
			token = *refreshed.ClaimToken
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("claiming session analysis (raw_record %s): %w", record.ID, err)
	}
	return prior, token, nil
}

// supersedeAndReclaimFreshGeneration supersedes the old generation's content and state
// rows (at oldVocab) and claims a fresh state row at newVocab, so a vocabulary bump
// creates a coexisting new generation instead of overwriting the old. Returns the fresh generation's claim token. Runs inside the caller's tx.
func (p *SessionAnalysisProcessor) supersedeAndReclaimFreshGeneration(ctx context.Context, orgID string, record *model.RawRecordModel, meta *rawRecordMetadata, oldVocab, newVocab int32) (string, error) {
	supersededOld, err := p.sessionStateDAO.Supersede(ctx, record.ID, oldVocab)
	if err != nil {
		return "", fmt.Errorf("supersede old state generation: %w", err)
	}
	// Only the worker that wins the old-generation state CAS may touch content.
	// SupersedeContent supersedes whichever generation is currently live, so a loser
	// (Supersede matched zero rows — another worker already superseded oldVocab and owns
	// the fresh claim) must not proceed, or it could supersede the winner's freshly judged
	// generation and leave no live content for serving. Bail; the winner owns the reclaim.
	if !supersededOld {
		return "", nil
	}
	if _, err := p.sessionAnalysisDAO.SupersedeContent(ctx, record.ID); err != nil {
		return "", fmt.Errorf("supersede old content generation: %w", err)
	}
	state := &model.SessionAnalysisState{
		OrganizationID:        orgID,
		RawRecordID:           record.ID,
		ConversationStartedAt: record.DataDate,
		VocabularyVersion:     newVocab,
		// Stamp repo_name on claim (like person_id) so the fresh generation's state row carries it,
		// matching the skip path — see the NO_STATE claim above.
		RepoName: nilIfEmpty(meta.RepoName),
	}
	if meta.PersonID != "" {
		state.PersonID = &meta.PersonID
	}
	claimed, err := p.sessionStateDAO.ClaimForProcessing(ctx, state)
	if err != nil {
		return "", fmt.Errorf("claim fresh generation: %w", err)
	}
	if !claimed || state.ClaimToken == nil {
		return "", nil // another tick won the fresh claim
	}
	// A vocabulary bump is rare and reprocesses a full generation; log it at Info so the
	// cutover of enum meanings is observable per org in prod.
	logger.Info(ctx, "session analysis: superseded old vocabulary generation, claimed fresh",
		zap.String(logger.OrgID, orgID),
		zap.String("raw_record_id", record.ID),
		zap.Int32("old_vocabulary_version", oldVocab),
		zap.Int32("new_vocabulary_version", newVocab))
	return *state.ClaimToken, nil
}

// transitionSpineDone moves the state row to SPINE_DONE and, in the same transaction,
// writes the measured-only content row (judged_at NULL) via UpsertMeasured — so a
// session whose producers then fail or stay dark still serves its cost/token cards
// instead of showing nothing until R resolves.
// The write is skipped on a spine-only reprocess: sp.spineOnly means the body and
// contracts are unchanged since the last judged generation, so the measured columns
// would recompute identically to what already serves, and the commit's own
// UpsertMeasuredReaffirmingJudged (never plain UpsertMeasured) is the sole writer for
// that path. Elsewhere, judged=false unconditionally: a spine-only reprocess's judged
// reaffirmation happens only at the real commit, and UpsertMeasured itself never
// touches judged_at/judged_disposition, so this early write cannot clobber a prior
// generation's verdict either way.
func (p *SessionAnalysisProcessor) transitionSpineDone(ctx context.Context, orgID string, record *model.RawRecordModel, personID, claimToken string, sp *spineResult) error {
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		ok, err := p.sessionStateDAO.TransitionToSpineDone(sctx, record.ID, claimToken, sp.contentSha256, sp.sourceUpdatedAt, sp.spineVersion, sp.conversationStartedAt)
		if err != nil {
			return fmt.Errorf("transition to spine_done: %w", err)
		}
		if !ok {
			return errRequeue
		}
		if sp.spineOnly {
			return nil
		}
		measured := buildSessionAnalysisRow(orgID, record, personID, sp, nil, nil, false, nil)
		if err := p.sessionAnalysisDAO.UpsertMeasured(sctx, measured); err != nil {
			return fmt.Errorf("upsert early measured session analysis: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("spine_done scope (raw_record %s): %w", record.ID, err)
	}
	return nil
}

func (p *SessionAnalysisProcessor) markFailed(ctx context.Context, orgID, rawRecordID string, dataDate time.Time, claimToken string, cause error) {
	// A provider-side transient failure feeds the outage signal. A failure under an ACTIVE
	// provider-wide outage does NOT increment attempt_count — the failure is the provider's,
	// not the record's, so a long outage cannot inflate the counter past the retry cap and
	// strand the row the moment the signal clears. A
	// record-class failure (a 4xx, a parse/oversize/build fault, a deterministic output
	// truncation) always increments and still exhausts into FAILED at the cap.
	now := time.Now().UTC()
	transient := isProviderTransient(cause)
	if transient {
		p.outage.recordTransient(now, p.cfg.OutageSignalWindow())
	}
	incrementAttempt := !transient || !p.outage.active(now, p.cfg.OutageSignalWindow(), p.cfg.OutageSignalMinRecords())
	// The ctx that reaches markFailed is typically the per-record ctx whose deadline just
	// expired (a commit cut mid-flight routes its own timed-out ctx here). Stamp the terminal
	// FAILED transition under a detached, fresh-deadline ctx so the mark itself cannot time out
	// on the same expiry: WithoutCancel keeps the tenant/plan-scope values RunInScope reads off
	// the ctx while dropping the dead deadline. Without this the transition never lands,
	// attempt_count never increments, and the SPINE_DONE row re-picks forever.
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := tenantdb.RunInScope(failCtx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		// Read the body actually attempted so the failed transition can stamp the staleness anchor
		// to it (SQL-only, no S3): this bounds a persistently-failing changed body to the retry cap
		// instead of the content-changed leg re-admitting it forever. A read miss is non-fatal: it
		// leaves the anchor untouched (empty digest → the NULLIF keeps it NULL; zero time is a benign
		// no-op), so log and stamp FAILED without it rather than fail the mark on an anchor read.
		digest, digestErr := p.rawRecordDAO.CurrentContentDigest(sctx, rawRecordID, dataDate)
		updatedAt, updatedErr := p.rawRecordDAO.CurrentSourceUpdatedAt(sctx, rawRecordID, dataDate)
		if anchorErr := errors.Join(digestErr, updatedErr); anchorErr != nil {
			logger.WarnWithCtx(sctx, "session analysis: failure-anchor read incomplete; stamping FAILED without it",
				zap.String("raw_record_id", rawRecordID), zap.Error(anchorErr))
		}
		ok, attemptCount, terr := p.sessionStateDAO.TransitionToFailed(sctx, rawRecordID, claimToken, cause.Error(), incrementAttempt, digest, updatedAt)
		if terr != nil {
			return fmt.Errorf("transition to failed (raw_record %s): %w", rawRecordID, terr)
		}
		if !ok {
			// The claim token no longer matches: the lease was stolen and another worker
			// owns this generation and its terminal state. Skip the compat write, or a
			// stale FAILED would overwrite the winner's 'processed' — hiding the row from
			// the loops-retry join and publishing a false failure to CDC/BQ.
			return nil
		}
		// Phase-5 compat write (a no-op unless the global flag is on): a hard FAILED
		// session maps to processing_status 'failed', sharing this transition's transaction.
		if err := p.stampCompatProcessingStatus(sctx, orgID, rawRecordID, dataDate, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED); err != nil {
			return fmt.Errorf("compat write (failed): %w", err)
		}
		// Static-on-failure serving: stamp the
		// terminal JUDGE_FAILED disposition over the measured row spine_done already wrote,
		// so a session whose R never resolves still serves its cost/token cards. The DAO's
		// judged_at IS NULL guard makes this a no-op both pre-spine_done and against a row
		// that already carries a valid judged verdict.
		//
		// Stamp it only once the failure is terminal — the retry budget is exhausted
		// (attempt_count >= RetryCap). A still-retryable failure keeps judged_at NULL so the
		// row stays hidden (measured-only) and does not serve as terminally JUDGE_FAILED during
		// its retry window; a later successful retry judges it normally. Under a provider outage
		// attempt_count is not incremented (incrementAttempt=false), so it never reaches the cap
		// and never prematurely stamps JUDGE_FAILED.
		if attemptCount >= p.cfg.RetryCap() {
			if _, err := p.sessionAnalysisDAO.MarkJudgeFailed(sctx, rawRecordID); err != nil {
				return fmt.Errorf("mark judge-failed: %w", err)
			}
		}
		return nil
	}); err != nil {
		markFailedWriteErrorsTotal.WithLabelValues(orgID).Inc()
		logger.WarnWithCtx(ctx, "session analysis: could not mark record failed",
			zap.String("raw_record_id", rawRecordID), zap.Error(err))
	}
}

// markProducerFailed records an E or D inference failure that R survives: it stamps the producer
// CALL_STATUS_FAILED (so finalize sets its failed_producers bit and the failed-producer re-admission
// leg retries only it, carrying R) and threads the error and the attempt-count decision to the judged
// commit, instead of failing the whole record. The attempt-count decision mirrors markFailed — a
// provider-outage failure does not count toward the retry cap, so an outage cannot strand the producer
// . The record still commits R as JUDGED and serves it; only the failed
// producer's grain is withheld until a retry succeeds or the cap is reached.
func (p *SessionAnalysisProcessor) markProducerFailed(ctx context.Context, orgID, producer string, contract int32, sp *spineResult, cause error) {
	now := time.Now().UTC()
	transient := isProviderTransient(cause)
	if transient {
		p.outage.recordTransient(now, p.cfg.OutageSignalWindow())
	}
	incrementAttempt := !transient || !p.outage.active(now, p.cfg.OutageSignalWindow(), p.cfg.OutageSignalMinRecords())
	sp.setCallStatus(producer, sessionintelligencepb.CallStatus_CALL_STATUS_FAILED, contract)
	sp.failedProducerError = cause.Error()
	// OR so a second producer failure in the same pass (E then D) still counts if either is the
	// record's fault, and a transient one never clears a real one's increment.
	sp.failedProducerIncrementAttempt = sp.failedProducerIncrementAttempt || incrementAttempt
	producerFailedTotal.WithLabelValues(orgID, producer).Inc()
	logger.WarnWithCtx(ctx, "session analysis: producer failed; preserving R and retrying only this producer",
		zap.String(logger.OrgID, orgID), zap.String("producer", producer), zap.Error(cause))
}

// bumpParentFence resolves the parent raw record by its record_key and advances its
// children_seq, so a parent that has already folded its child set (children_seq =
// folded_children_seq) now has children_seq > folded_children_seq and the child-refold scan leg
// re-admits the JUDGED parent to fold the newly-sighted child. It never fails a commit-time CAS
// (the judged CAS is lease-only). Best-effort: a child seen before its parent is ingested bumps
// nothing (the parent folds it when it first runs), and a transient lookup failure only defers
// the re-fold to a later tick.
func (p *SessionAnalysisProcessor) bumpParentFence(ctx context.Context, orgID, childRawRecordID, parentRecordKey string) {
	parent, err := p.rawRecordDAO.GetByRecordKey(ctx, orgID, SessionConnectorID, parentRecordKey)
	if err != nil {
		if !errors.Is(err, dao.ErrNotFound) {
			logger.WarnWithCtx(ctx, "session analysis: could not resolve parent for child fence",
				zap.String("raw_record_id", childRawRecordID), zap.Error(err))
		}
		return
	}
	if _, err := p.sessionStateDAO.BumpChildrenSeq(ctx, parent.ID); err != nil {
		logger.WarnWithCtx(ctx, "session analysis: could not bump parent child fence",
			zap.String("raw_record_id", childRawRecordID), zap.Error(err))
	}
}
