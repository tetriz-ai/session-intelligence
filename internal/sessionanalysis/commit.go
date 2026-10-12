// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// toCommonIdeType converts the conversation's session IdeType to the common IdeType
// the model column stores, by enum name (the two enums share the IDE_TYPE_* vocab).
// Returns nil for UNSPECIFIED / an unmapped value, so the column stays NULL rather
// than storing a wrong IDE.
func toCommonIdeType(ide sessionintelligencepb.IdeType) *commonv1.IdeType {
	v, ok := commonv1.IdeType_value[ide.String()]
	if !ok || v == 0 {
		return nil
	}
	out := commonv1.IdeType(v)
	return &out
}

// The security-layer slug→flag_type vocabulary and severity ranking live in the
// security package (the #3 detector's reconcile half); the engine calls into it.

// severityString maps an issue-severity enum to the lowercase string the taxonomy and
// flag columns store. General to any issue layer, not only security.
var severityString = map[sessionintelligencepb.IssueSeverity]string{
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_LOW:      "low",
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_MEDIUM:   "medium",
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_HIGH:     "high",
	sessionintelligencepb.IssueSeverity_ISSUE_SEVERITY_CRITICAL: "critical",
}

// promptIssueSlugKebab is the inverse of slugToEnum (llm_client.go): the kebab-case slug the
// taxonomy_issues column stores — the canonical form R re-parses, matching security_flags'
// flag_type for the three security slugs (e.g. "api-key-leak").
func promptIssueSlugKebab(slug sessionintelligencepb.PromptIssueSlug) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(slug.String(), "PROMPT_ISSUE_SLUG_"), "_", "-"))
}

// wasteCauseKebab derives the kebab-case category the waste_categories column stores from a
// WasteCause enum (e.g. "over-fetch"), keeping both new-vocabulary columns kebab.
func wasteCauseKebab(cause sessionintelligencepb.WasteCause) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(cause.String(), "WASTE_CAUSE_"), "_", "-"))
}

// commitResult carries the CAS outcome so the caller can distinguish a re-queue
// (the lease was stolen, or the row left the SPINE_DONE/PENDING claim window) from a
// hard error.
type commitResult struct {
	committed bool
}

// commit lands the judged session-grain content, the state transition, and the
// security-flag reconcile in ONE tenant-scoped transaction: a crash never leaves a half-written session. The state transition is CAS-gated on the
// claim token (the lease) ALONE — never on the children_seq snapshot (the durable staged
// commit): a valid R verdict must always reach JUDGED and serve, so a state conflict can no
// longer roll a paid producer result back. A child sighted mid-flight (children_seq >
// folded_children_seq) re-admits the now-JUDGED parent via the child-refold leg to fold the delta,
// carrying R by rehydration. Staleness is guarded pre-R (the claim anchor + the CONTENT_CHANGED
// scan leg), so there is no post-producer body re-check that could discard the verdict here.
func (p *SessionAnalysisProcessor) commit(ctx context.Context, orgID string, record *model.RawRecordModel, personID string, sp *spineResult, read *ReadResult, discard *DiscardResult) (commitResult, error) {
	var result commitResult
	err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		// judged is true for a full R run or a spine-only reprocess carrying a prior JUDGED
		// generation; false only for a first-time session whose R is flag-dark. A dark-R row
		// lands measured-only with judged_at NULL, so the serving gate (judged_at IS NOT NULL)
		// withholds it until R is enabled and the producer-enabled scan leg re-admits it for a
		// full re-run. The state row still transitions to JUDGED
		// (terminal, claim-guarded, no lease-expiry churn) — it is the content-row judged_at, not
		// the state, that gates serving.
		judged := read != nil || sp.spineOnly
		// Build the measured turn grain once per commit: buildSessionAnalysis derives the
		// session-level turn-author rollups from it and writeMeasuredTurns persists the same rows,
		// so the grouping pass (splitHumanTurns + the event-timeline sort) is not run twice. nil on
		// a spine-only reprocess (carries prior turns) and for a source with no turn grouping.
		var measuredTurns []*model.SessionTurnAnalysis
		if shouldBuildMeasuredTurns(sp) {
			measuredTurns = buildMeasuredTurnRows(orgID, record.ID, sp)
		}
		analysis := buildSessionAnalysisRow(orgID, record, personID, sp, read, discard, judged, measuredTurns)
		// A spine-only reprocess (read == nil) refreshes ONLY the measured columns; the prior
		// generation's judged verdict is carried unchanged. A full run overwrites the live
		// generation (measured + judged) in place. The read == nil path splits by whether a valid
		// prior verdict exists to re-affirm:
		// - sp.spineOnly: spineOnlyReprocess proved a prior JUDGED generation with settled
		// producers and an unchanged body, so the carried verdict is valid — restore judged_at
		// (a cat-3 legacy reopen may have cleared it via ClearServedVerdict; a plain
		// UpsertMeasured would then leave the still-valid session absent from serving forever).
		// - first-time dark-R (read == nil && !sp.spineOnly): R never produced a verdict, so
		// judged_at stays NULL (withheld until R is enabled and category 9 re-admits it).
		switch {
		case read == nil && sp.spineOnly:
			if err := p.sessionAnalysisDAO.UpsertMeasuredReaffirmingJudged(sctx, analysis); err != nil {
				return fmt.Errorf("upsert measured session analysis (reaffirm judged): %w", err)
			}
		case read == nil:
			if err := p.sessionAnalysisDAO.UpsertMeasured(sctx, analysis); err != nil {
				return fmt.Errorf("upsert measured session analysis: %w", err)
			}
		default:
			// E's grain is authoritative this pass when E ran (EvidenceRan) OR E errored (its FAILED
			// disposition is set this pass) — either way UpsertJudged must NOT carry the prior E columns
			// over it. Only a pass that neither ran nor failed E (an R-only re-judge, a D-only retry that
			// carries a DONE E) carries prior E forward.
			evidenceAuthoritative := read.EvidenceRan || evidenceCallStatus(sp) == sessionintelligencepb.CallStatus_CALL_STATUS_FAILED
			if err := p.sessionAnalysisDAO.UpsertJudged(sctx, analysis, evidenceAuthoritative); err != nil {
				return fmt.Errorf("upsert session analysis: %w", err)
			}
		}

		// The measured turn grain — written by the grouper on EVERY commit that re-parsed
		// the body (unlike the judged turn write below, which is read-gated), before the
		// judged overlay so a measured-first turn owns its identity. The measured writer
		// owns tail-retire/regrow. Skipped when the body was not re-parsed (a spine-only
		// zero-cost reprocess carries the prior measured turns unchanged).
		if err := p.writeMeasuredTurns(sctx, orgID, record, sp, measuredTurns); err != nil {
			return fmt.Errorf("write measured turn grain: %w", err)
		}

		// The turn/phase grain, FK-safe (phases upsert by ordinal preserving id, turns
		// re-point to the fresh phase ids, trailing rows soft-deleted) — all in this one
		// commit transaction.
		if err := p.writeTurnPhase(sctx, orgID, record, analysis.ID, sp, read, measuredTurns); err != nil {
			return fmt.Errorf("write turn/phase grain: %w", err)
		}

		// E's per-pair round-trip faults, written replace-set to the E-owned table — never
		// stamped onto R's immutable turn rows (by design).
		if err := p.writeRoundTripFaults(sctx, orgID, record, analysis.ID, sp, read); err != nil {
			return fmt.Errorf("write round-trip fault grain: %w", err)
		}

		// A body re-uploaded while the producers ran makes this verdict stale. Only a full R pass (read != nil) publishes a fresh verdict + a security reconcile, so
		// only it needs the re-check: a spine-only reprocess already proved the body unchanged, and a
		// dark-R measured row has no verdict to be stale. The durable staged commit still lands JUDGED
		// (the paid result is not discarded) — it only skips the stale security reconcile so a verdict
		// known to be for a superseded body never publishes its slugs. Re-analysis of the current body
		// is NOT forced here: it rides the normal CONTENT_CHANGED re-admit on the reprocess throttle,
		// exactly like any other re-upload of a JUDGED session, so an actively-edited session is not
		// re-billed on every change.
		bodyStale := false
		if read != nil {
			changed, err := p.bodyChangedSinceClaim(sctx, record, sp.contentSha256, sp.sourceUpdatedAt)
			if err != nil {
				return fmt.Errorf("re-check body freshness at commit: %w", err)
			}
			if changed {
				bodyStale = true
				bodyStaleAtCommitTotal.WithLabelValues(orgID).Inc()
				logger.Info(sctx, "session analysis: body changed mid-flight; landing durable verdict, skipping stale security reconcile (fresh run on the reprocess throttle)",
					zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID))
			}
		}

		commit := dao.JudgedCommit{
			CallState:               sp.callState,
			ContractVersionRead:     p.cfg.ContractVersionRead(),
			ContractVersionEvidence: p.cfg.ContractVersionEvidence(),
			ContractVersionDiscard:  p.cfg.ContractVersionDiscard(),
			VocabularyVersion:       sp.vocabularyVersion,
			FlagSkippedProducers:    sp.flagSkippedProducers,
			PendingDataProducers:    sp.pendingDataProducers,
			// A producer that errored this pass (E or D) commits R as JUDGED and stamps its
			// failed_producers bit + counts toward the retry cap, so the failed-producer leg retries
			// only it — the record is never wholesale JUDGE_FAILED.
			FailedProducers:  sp.failedProducers,
			IncrementAttempt: sp.failedProducerIncrementAttempt,
			LastError:        nilIfEmpty(sp.failedProducerError),
			Disposition:      sp.disposition,
			PersonID:         nilIfEmpty(personID),
		}
		ok, err := p.sessionStateDAO.TransitionToJudged(sctx, record.ID, sp.claimToken, sp.foldedChildrenSeq, commit)
		if err != nil {
			return fmt.Errorf("transition to judged: %w", err)
		}
		if !ok {
			// The lease was stolen (claim_token no longer matches) or the row left the
			// SPINE_DONE/PENDING claim window (e.g. superseded): roll back the content write and
			// re-queue rather than land this pass's verdict over another generation. A child
			// sighted mid-flight does NOT trip this — the CAS is lease-only, so R still lands
			// JUDGED and the child-refold leg folds the delta on the next tick.
			return errRequeue
		}

		// A verdict known to be for a superseded body (re-uploaded mid-flight) must NOT publish its
		// security flags — a stale verdict never overwrites the security surface. The current body's flags are reconciled by the normal CONTENT_CHANGED
		// re-admit once the reprocess throttle elapses; the prior generation's flags stand until then.
		if !bodyStale {
			if err := p.reconcileSecurityFlags(sctx, record, personID, read); err != nil {
				return fmt.Errorf("reconcile security flags: %w", err)
			}
		}
		// Phase-5 compat write (a no-op unless the global flag is on): a JUDGED session
		// maps to processing_status 'processed', sharing this commit's transaction.
		if err := p.stampCompatProcessingStatus(sctx, orgID, record.ID, record.DataDate, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED); err != nil {
			return fmt.Errorf("compat write (judged): %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errRequeue) {
			return result, nil // re-queue, not fatal
		}
		return result, fmt.Errorf("committing judged session analysis (raw_record %s): %w", record.ID, err)
	}
	result.committed = true
	return result, nil
}

// bodyChangedSinceClaim reports whether the raw record's body was re-uploaded while this pass's
// producers ran. A hash-bearing body compares the current payload content_sha256 to the digest the
// producers analyzed (claimedSha, captured at claim time). A LEGACY body (no payload
// content_sha256, current digest "") has no cheap digest, so it compares raw_records.updated_at
// against claimedUpdatedAt. That comparison is deliberately conservative: CreateBatch bumps
// updated_at on any conflicting upsert, so a byte-identical re-emit (an at-least-once SQS
// redelivery) reads as "changed" — the SAFE direction, since it only over-triggers the
// stale-security skip, and never lets a stale verdict publish its flags.
//
// This gates ONLY the stale-security skip — it NEVER discards the paid verdict and never changes
// the re-analysis cadence (the current body rides the normal CONTENT_CHANGED reprocess throttle) —
// so unlike a claim CAS it is read, not enforced, at commit. It is a point-in-time read, not a
// lock: a body landing in the tiny window between this read and the commit is not seen here, but is
// caught by that same CONTENT_CHANGED leg — so the worst case is the baseline every re-upload of a
// JUDGED session already takes, never a stale verdict that never heals.
func (p *SessionAnalysisProcessor) bodyChangedSinceClaim(ctx context.Context, record *model.RawRecordModel, claimedSha string, claimedUpdatedAt time.Time) (bool, error) {
	current, err := p.rawRecordDAO.CurrentContentDigest(ctx, record.ID, record.DataDate)
	if err != nil {
		return false, fmt.Errorf("re-read content digest (raw_record %s): %w", record.ID, err)
	}
	if current == "" {
		currentUpdatedAt, err := p.rawRecordDAO.CurrentSourceUpdatedAt(ctx, record.ID, record.DataDate)
		if err != nil {
			return false, fmt.Errorf("re-read source updated_at (raw_record %s): %w", record.ID, err)
		}
		return !currentUpdatedAt.Equal(claimedUpdatedAt), nil
	}
	return current != claimedSha, nil
}

// writeTurnPhase writes R's judged turn and phase grain in the commit transaction,
// FK-safely: the phase set is replaced by ordinal
// (preserving each row's id), then each turn is upserted column-scoped with its
// phase_id resolved from the fresh phase set, then the trailing turns a shorter
// re-analysis dropped are soft-deleted. A spine-only reprocess (read nil) carries the
// prior grain unchanged and writes nothing here.
func (p *SessionAnalysisProcessor) writeTurnPhase(ctx context.Context, orgID string, record *model.RawRecordModel, sessionAnalysisID string, sp *spineResult, read *ReadResult, measuredTurns []*model.SessionTurnAnalysis) error {
	if read == nil {
		return nil
	}
	// The measured grouper cuts turn rows only for supported sources (supportsTurnGrouping);
	// skip the judged turn/phase overlay for an unsupported source too, so the judged writer
	// never finds a missing measured base and drops each verdict with a warning.
	if !supportsTurnGrouping(sp.ideType, sp.usage.GetSessionSource()) {
		return nil
	}
	ordinalToPhaseID, err := p.sessionPhaseDAO.ReplacePhaseSet(ctx, orgID, sessionAnalysisID, buildPhaseRows(orgID, sessionAnalysisID, sp, read.Phases, measuredTurns))
	if err != nil {
		return fmt.Errorf("replace phase set: %w", err)
	}

	for _, turn := range read.Turns {
		row := buildTurnRow(orgID, record.ID, sessionAnalysisID, sp, turn)
		if turn.PhaseOrdinal != nil {
			if phaseID, ok := ordinalToPhaseID[*turn.PhaseOrdinal]; ok {
				row.PhaseID = &phaseID
			}
		}
		// UPDATE-only: overlay the verdict onto the LIVE measured row the grouper created
		// on the same shared cut earlier in this commit. The measured writer ran first in
		// THIS transaction, so a zero-match is not a lease/CAS race (those requeue upstream) —
		// it means the LLM named a prompt_index outside the measured cut [0,N). Drop that one
		// verdict and continue rather than requeue: re-reading the unchanged body re-cuts the
		// same range, so the out-of-cut index would recur and requeue would loop forever on a
		// paid read. The measured row is untouched; only the bad-index overlay is skipped
		// (mirrors the out-of-range drop in evidence.go).
		affected, uerr := p.sessionTurnDAO.UpdateJudged(ctx, row)
		if uerr != nil {
			return fmt.Errorf("update judged turn %d: %w", turn.PromptIndex, uerr)
		}
		if affected == 0 {
			logger.WarnWithCtx(ctx, "session analysis: judged turn has no measured row; dropping out-of-cut verdict",
				zap.String(logger.OrgID, orgID),
				zap.String("raw_record_id", record.ID),
				zap.Int32("prompt_index", turn.PromptIndex))
			continue
		}
	}
	// Tail-retire is NOT done here: the measured grouper is the single cutter and owns
	// the turn set's lifecycle (writeMeasuredTurns → SoftDeleteMeasuredBeyond), which runs
	// on the same shared cut before this judged overlay. The judged pass only updates the
	// live rows the measured writer created.
	return nil
}

// writeMeasuredTurns writes the grouper's measured turn rows in the commit transaction:
// one UpsertMeasured per turn, then SoftDeleteMeasuredBeyond retires the trailing turns a
// shorter re-grouping dropped (a regrow later revives them). Only a spine-only reprocess
// (sp.spineOnly) short-circuits: it re-ran no producer and re-parsed nothing, so the prior
// measured turns must be left intact. A genuine empty re-parse (zero prompts but not
// spine-only) still falls through — no UpsertMeasured runs and maxPromptIndex stays -1, so
// SoftDeleteMeasuredBeyond retires the whole prior tail rather than leaving stale turns live.
// Runs on every commit of a supported source (the master SessionAnalysisEnabled gate already
// governs whether the processor touches the org at all — ProcessFlat is dark for a disabled org).
func (p *SessionAnalysisProcessor) writeMeasuredTurns(ctx context.Context, orgID string, record *model.RawRecordModel, sp *spineResult, measuredTurns []*model.SessionTurnAnalysis) error {
	// Gate persistence on the same predicate that built the rows (shouldBuildMeasuredTurns), so the
	// turn-author rollups on session_analyses cannot diverge from what lands in session_turn_analyses.
	// Two reasons reach the not-building branch, handled differently:
	// - a spine-only reprocess re-ran no producer and re-parsed nothing, so the prior measured
	// turns must be left intact (nothing to do here);
	// - an unsupported source owns an empty turn set. One such source can still have PRIOR rows to
	// retire — Kiro Crew, told from interactive Kiro (supported) only by session_source: a legacy
	// upload that left session_source empty grouped a Crew session as interactive Kiro and wrote
	// turn rows; now that session_source resolves it regroups as Crew (unsupported) and those rows
	// would orphan with no cleanup path. Retire the whole prior tail (a no-op when there were
	// none). An always-unsupported source (by ide_type, e.g. Copilot) never had rows, so it needs
	// no retire.
	if !shouldBuildMeasuredTurns(sp) {
		if !sp.spineOnly && sp.ideType == sessionintelligencepb.IdeType_IDE_TYPE_KIRO {
			if _, err := p.sessionTurnDAO.SoftDeleteMeasuredBeyond(ctx, orgID, record.ID, -1); err != nil {
				measuredTurnCommitsTotal.WithLabelValues(orgID, turnWriteError).Inc()
				return fmt.Errorf("soft-delete measured turns for Kiro Crew (now unsupported): %w", err)
			}
		}
		return nil
	}
	if err := p.upsertAndRetireMeasured(ctx, orgID, record.ID, measuredTurns); err != nil {
		return fmt.Errorf("writing measured turns (raw_record %s): %w", record.ID, err)
	}
	return nil
}

// UpsertMeasuredTurns writes the measured turn rows in order, then retires the trailing turns a
// shorter re-grouping dropped — SoftDeleteMeasuredBeyond at the max prompt_index written, which stays
// -1 when there are no rows so an empty re-parse retires the whole prior tail. It is the single
// write/retire path, so the retire semantics are the same for every caller. Takes the DAO explicitly
// so any caller can share it; runs in the caller's already-open tenant scope.
func UpsertMeasuredTurns(ctx context.Context, turnDAO dao.ISessionTurnAnalysisDAO, orgID, rawRecordID string, rows []*model.SessionTurnAnalysis) error {
	var maxPromptIndex int32 = -1
	for _, row := range rows {
		if err := turnDAO.UpsertMeasured(ctx, row); err != nil {
			return fmt.Errorf("upsert measured turn %d: %w", row.PromptIndex, err)
		}
		if row.PromptIndex > maxPromptIndex {
			maxPromptIndex = row.PromptIndex
		}
	}
	if _, err := turnDAO.SoftDeleteMeasuredBeyond(ctx, orgID, rawRecordID, maxPromptIndex); err != nil {
		return fmt.Errorf("soft-delete trailing measured turns beyond %d: %w", maxPromptIndex, err)
	}
	return nil
}

// upsertAndRetireMeasured is the live pipeline's turn write/retire — the shared UpsertMeasuredTurns
// plus the turn-commit metric accounting, on the caller's already-open tenant scope. Used by the
// parent commit (writeMeasuredTurns) and the delegated-child path (writeChildMeasuredTurns).
func (p *SessionAnalysisProcessor) upsertAndRetireMeasured(ctx context.Context, orgID, rawRecordID string, rows []*model.SessionTurnAnalysis) error {
	if err := UpsertMeasuredTurns(ctx, p.sessionTurnDAO, orgID, rawRecordID, rows); err != nil {
		measuredTurnCommitsTotal.WithLabelValues(orgID, turnWriteError).Inc()
		return err //nolint:wrapcheck // UpsertMeasuredTurns already wraps each return with context
	}
	measuredTurnCommitsTotal.WithLabelValues(orgID, turnWriteSuccess).Inc()
	measuredTurnsWrittenTotal.WithLabelValues(orgID).Add(float64(len(rows)))
	return nil
}

func buildPhaseRows(orgID, sessionAnalysisID string, sp *spineResult, phases []*PhaseJudgment, measuredTurns []*model.SessionTurnAnalysis) []*model.SessionPhaseAnalysis {
	rows := make([]*model.SessionPhaseAnalysis, 0, len(phases))
	for _, phase := range phases {
		ordinal := phase.PhaseOrdinal
		rows = append(rows, &model.SessionPhaseAnalysis{
			OrganizationID:        orgID,
			SessionAnalysisID:     &sessionAnalysisID,
			ConversationStartedAt: sp.conversationStartedAt,
			PhaseOrdinal:          &ordinal,
			RangeStart:            phase.RangeStart,
			RangeEnd:              phase.RangeEnd,
			BoundaryOverridden:    phase.BoundaryOverridden,
			OverrideReason:        phase.OverrideReason,
			PhaseTheme:            phase.PhaseTheme,
			SdlcStageSet:          phase.SdlcStageSet,
			TaskComplexity:        phase.TaskComplexity,
			UserSteering:          phase.UserSteering,
			PhaseObjective:        phase.PhaseObjective,
			PhaseObjectiveMet:     phase.PhaseObjectiveMet,
			AgentHelpfulness:      phase.AgentHelpfulness,
			Smoothness:            phase.Smoothness,
			FrictionCause:         phase.FrictionCause,
			Speed:                 phase.Speed,
			WentWell:              phase.WentWell,
			WentWrong:             phase.WentWrong,
			Coaching:              phase.Coaching,
			// turn_count is R-owned (the parser derives it from the phase range, so it round-trips
			// on a re-pick); tokens is code-owned, summed over the phase's range from the spine's
			// per-turn cost table.
			TurnCount: phase.TurnCount,
			Tokens:    phaseTokens(sp, phase),
			// hitl_gate is the measured HITL gate active where the phase begins (its RangeStart
			// turn), forward-filled per turn by the spine. Code-owned; R must not emit it.
			HitlGate: phaseHitlGate(sp, phase),
			// tool_call_count is summed from the measured per-turn counts over the phase's range.
			// Code-owned; R must not emit it.
			ToolCallCount: phaseToolCalls(measuredTurns, phase),
		})
	}
	return rows
}

// phaseTurnCountFromRange derives a phase's turn_count as its inclusive prompt-index span
// (end-start+1), returning nil for an inverted range so a malformed phase stores NULL.
func phaseTurnCountFromRange(start, end int32) *int32 {
	if end < start {
		return nil
	}
	// int64 intermediate so an extreme (hallucinated) endpoint pair cannot overflow int32; the
	// read-pass phase validation clamps the range to the real prompt domain before this is stored,
	// so a realistic count is always well within int32.
	count := int64(end) - int64(start) + 1
	if count > math.MaxInt32 {
		count = math.MaxInt32
	}
	out := int32(count)
	return &out
}

// clampPhaseRangesToDomain bounds each R phase range to the valid prompt-index domain [0, n) and
// drops a phase whose range does not intersect it, re-deriving turn_count from the clamped range so
// the stored span, turn_count and token sum stay consistent. R emits phase ranges as prompt indices;
// a hallucinated end past the real prompt count (n = the measured prompt count) would otherwise
// store a corrupt turn_count and a token sum that silently covers only the in-domain prefix. n<=0
// leaves the phases unchanged (no domain to validate against, and nothing to sum either).
func clampPhaseRangesToDomain(phases []*PhaseJudgment, promptCount int32) []*PhaseJudgment {
	if promptCount <= 0 {
		return phases
	}
	kept := make([]*PhaseJudgment, 0, len(phases))
	for _, phase := range phases {
		if phase.RangeStart == nil || phase.RangeEnd == nil {
			kept = append(kept, phase)
			continue
		}
		start, end := *phase.RangeStart, *phase.RangeEnd
		if start < 0 {
			start = 0
		}
		if end > promptCount-1 {
			end = promptCount - 1
		}
		if start > end || start > promptCount-1 {
			continue // the range does not intersect the domain — drop the phase entirely
		}
		phase.RangeStart, phase.RangeEnd = &start, &end
		phase.TurnCount = phaseTurnCountFromRange(start, end)
		kept = append(kept, phase)
	}
	return kept
}

// phaseHitlGate returns the measured HITL gate active at the phase's first turn (its RangeStart),
// read from the spine's per-turn forward-filled gates. It is position-aware: a mode change between
// phases lands on the phase whose range it starts, never the session's last posture. Returns nil
// (column stays NULL) when the phase carries no RangeStart or the index is outside the per-turn
// table — mirroring phaseTokens's bounds discipline so a malformed range stores NULL, not a wrong
// gate.
func phaseHitlGate(sp *spineResult, phase *PhaseJudgment) *sessionintelligencepb.HitlGate {
	if phase.RangeStart == nil {
		return nil
	}
	start := *phase.RangeStart
	if start < 0 || start >= int32(len(sp.perTurnHitlGate)) {
		return nil
	}
	return sp.perTurnHitlGate[start]
}

// phaseTokens sums the measured per-turn token cost over a phase's inclusive prompt-index range
// (spine's promptTokenCosts), returning nil when the phase carries no valid range so a malformed
// range stores NULL rather than a wrong sum.
func phaseTokens(sp *spineResult, phase *PhaseJudgment) *int64 {
	if phase.RangeStart == nil || phase.RangeEnd == nil {
		return nil
	}
	start, end := *phase.RangeStart, *phase.RangeEnd
	costCount := int32(len(sp.promptTokenCosts))
	// A range that is inverted or entirely outside the cost table has no derivable token sum —
	// return nil (unknown) rather than a pointer to 0, which would read as a real "cost nothing".
	// The read-pass phase validation clamps a range that only overruns the end, so a stored phase
	// intersects the domain; this guards a phase that slips through (or a spine-only carry).
	if start < 0 || end < start || start >= costCount {
		return nil
	}
	var tokens int64
	for promptIndex := start; promptIndex <= end && promptIndex < costCount; promptIndex++ {
		tokens += sp.promptTokenCosts[promptIndex]
	}
	return &tokens
}

// phaseToolCalls sums the measured per-turn tool_call_count over a phase's inclusive prompt-index
// range, mirroring phaseTokens. Returns nil (stores NULL, an unknown) when the range is malformed
// (nil / negative / inverted bounds) or intersects no measured turn — the latter covering both an
// empty measuredTurns and a range entirely outside the turn domain, so a phase that carries no
// derivable count reads as unknown rather than a real "zero tool calls", exactly as phaseTokens and
// the session-grain rollup do. Otherwise the sum, where a turn with a nil count (a source with no
// session_events timeline) contributes 0 — keeping phase and session tool_call_count consistent.
func phaseToolCalls(measuredTurns []*model.SessionTurnAnalysis, phase *PhaseJudgment) *int32 {
	if phase.RangeStart == nil || phase.RangeEnd == nil {
		return nil
	}
	start, end := *phase.RangeStart, *phase.RangeEnd
	if start < 0 || end < start {
		return nil
	}
	var toolCalls int32
	matched := false
	for _, turn := range measuredTurns {
		if turn.PromptIndex >= start && turn.PromptIndex <= end {
			matched = true
			if turn.ToolCallCount != nil {
				toolCalls += *turn.ToolCallCount
			}
		}
	}
	if !matched {
		return nil
	}
	return &toolCalls
}

// hitlGateFromApprovalMode normalizes a raw per-turn HITL mode string (Claude Code permissionMode
// or Codex approval_policy, captured verbatim onto the turn's permission_mode) into the HitlGate
// posture the column stores. A single flat map is correct because the two IDEs' vocabularies are
// currently DISJOINT (no token means one gate in Claude and another in Codex). An empty or
// unrecognized mode returns nil so the column stays NULL (UNSPECIFIED) rather than a wrong posture.
// //exhaustive: unknown tokens fall through to nil deliberately — a new IDE vocab adds entries here
// rather than being silently mis-mapped.
func hitlGateFromApprovalMode(mode string) *sessionintelligencepb.HitlGate {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	// Claude Code permissionMode.
	case "default":
		return sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT.Enum()
	case "plan":
		return sessionintelligencepb.HitlGate_HITL_GATE_PLAN.Enum()
	case "acceptedits", "accept_edits", "auto":
		return sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()
	case "bypasspermissions", "bypass_permissions":
		return sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()
	// Codex approval_policy (both the legacy suggest/auto-edit/full-auto and the modern
	// never/on-request/on-failure/untrusted vocabularies).
	case "never", "full-auto", "fullauto":
		return sessionintelligencepb.HitlGate_HITL_GATE_BYPASS_PERMISSIONS.Enum()
	case "auto-edit", "autoedit", "on-failure", "onfailure":
		return sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS.Enum()
	case "suggest", "on-request", "onrequest", "untrusted":
		return sessionintelligencepb.HitlGate_HITL_GATE_DEFAULT.Enum()
	default:
		return nil
	}
}

func buildTurnRow(orgID, rawRecordID, sessionAnalysisID string, sp *spineResult, turn *TurnJudgment) *model.SessionTurnAnalysis {
	sessionID := sessionAnalysisID
	return &model.SessionTurnAnalysis{
		OrganizationID:        orgID,
		RawRecordID:           rawRecordID,
		SessionAnalysisID:     &sessionID,
		ConversationStartedAt: sp.conversationStartedAt,
		PromptIndex:           turn.PromptIndex,
		PromptType:            turn.PromptType,
		UserSteering:          turn.UserSteering,
		PromptCompleteness:    turn.PromptCompleteness,
		PromptObjectiveMet:    turn.PromptObjectiveMet,
		AgentHelpfulness:      turn.AgentHelpfulness,
		AgentInitiative:       turn.AgentInitiative,
		ToolUse:               turn.ToolUse,
		WasRetry:              turn.WasRetry,
		FailureClass:          turn.FailureClass,
		TokenLocus:            turn.TokenLocus,
		WasteCause:            turn.WasteCause,
		DecisionType:          turn.DecisionType,
		DownstreamImpact:      turn.DownstreamImpact,
		FeedbackTarget:        turn.FeedbackTarget,
		FeedbackDisposition:   turn.FeedbackDisposition,
		ReferencedArtifact:    turn.ReferencedArtifact,
		ReferencedFindingIDs:  turn.ReferencedFindingIDs,
		EvidenceSeq:           turn.EvidenceSeq,
		PromptIssues:          turn.PromptIssues,
		// E's per-pair round-trip fault (nil unless this turn was a pair's P turn) and the
		// chunk index the windowed read stitched the turn from (nil on the single-chunk path).
		RoundTripFault:               turn.RoundTripFault,
		RoundTripEvidencePromptIndex: turn.RoundTripEvidencePromptIndex,
		ChunkIndex:                   turn.ChunkIndex,
		JudgedContentSha256:          &sp.contentSha256,
	}
}

// writeRoundTripFaults persists E's per-pair round-trip faults to the E-owned
// session_round_trip_faults table, replace-set, inside the commit transaction. E owns
// this table, so only a pass E actually ran on
// (read.EvidenceRan) may touch it: a full re-judge where E fired replaces the fault set — an
// empty set means E ran and found none. A pass where E did not run — a spine-only reprocess, or
// a full R re-judge with no artifact/round-trip trigger — preserves the prior generation's
// fault set unchanged rather than clearing it. Faults are keyed by prompt_index (P's index — the
// earlier prompt of each pair); a duplicate index keeps the last, matching the table's writer-key
// partial unique.
func (p *SessionAnalysisProcessor) writeRoundTripFaults(ctx context.Context, orgID string, record *model.RawRecordModel, sessionAnalysisID string, sp *spineResult, read *ReadResult) error {
	if read == nil || !read.EvidenceRan {
		return nil
	}
	// A fault is only persistable for a pair the detector actually supplied to E — the table has
	// no turn foreign key, so an LLM-hallucinated prompt_index would otherwise become a durable
	// fault attributed to a pair E was never offered. Validate each fault against the exact
	// candidate P-index set (the old turn-row writer dropped an out-of-table index the same way);
	// a fault outside it is dropped and counted.
	candidateIndexes := make(map[int32]struct{}, len(sp.roundTripCandidates))
	for _, candidate := range sp.roundTripCandidates {
		candidateIndexes[candidate.PromptIndex] = struct{}{}
	}
	byIndex := make(map[int32]*model.SessionRoundTripFault, len(read.RoundTripFaults))
	for _, roundTrip := range read.RoundTripFaults {
		if _, offered := candidateIndexes[roundTrip.PromptIndex]; !offered {
			roundTripFaultsRejectedTotal.WithLabelValues(orgID).Inc()
			continue
		}
		byIndex[roundTrip.PromptIndex] = &model.SessionRoundTripFault{
			OrganizationID:        orgID,
			SessionAnalysisID:     sessionAnalysisID,
			RawRecordID:           record.ID,
			ConversationStartedAt: sp.conversationStartedAt,
			PromptIndex:           roundTrip.PromptIndex,
			Fault:                 roundTrip.Fault,
			EvidencePromptIndex:   roundTrip.EvidencePromptIndex,
		}
	}
	faults := make([]*model.SessionRoundTripFault, 0, len(byIndex))
	for _, fault := range byIndex {
		faults = append(faults, fault)
	}
	if err := p.sessionRoundTripFaultDAO.ReplaceSet(ctx, orgID, sessionAnalysisID, faults); err != nil {
		return fmt.Errorf("replace round-trip fault set: %w", err)
	}
	return nil
}

// reconcileSecurityFlags extracts the security-layer prompt issues from R's and E's
// readouts and delegates the replace-set into security_flags to the shared DAO
// reconcile (dao.SecurityFlagRepo.ReconcileOpenFlags — the same method the legacy
// Session writer calls), sharing the commit's transaction. The reconcile is last-writer-wins: this pass's fresh set replaces the raw
// record's open flags outright, with no cross-writer preservation — see the shared
// method's doc. Never swallow the error: a swallowed replacement-insert failure would
// commit a deleted/partial flag set beside a judged status, corrupting the security
// data with no path to repair.
func (p *SessionAnalysisProcessor) reconcileSecurityFlags(ctx context.Context, record *model.RawRecordModel, personID string, read *ReadResult) error {
	// R did not run this pass (a spine-only reprocess or a FLAG_SKIPPED dark R): the
	// prior generation's security readout is carried unchanged, so touch nothing.
	// Clearing here would conflate "R ran and found nothing" (a genuine clear) with "R
	// did not run" and silently wipe a live api-key-leak flag on a measured-only
	// re-commit.
	if read == nil {
		return nil
	}
	// The slug→flag_type mapping, severity promotion and the replace-set persist live in
	// the security package (the #3 detector). The engine applies only the E-gate: R's
	// readout always publishes; E's net-new round-trip slugs publish only when the org's
	// EvidenceSecurityFlagPublishEnabled config is on.
	return p.securityReconciler.ReconcileOpenFlags(
		ctx, record.OrganizationID, personID, record.ID, startOfDayUTC(record.FetchedAt),
		read.SecurityIssues, read.EvidenceSecurityIssues,
		p.cfg.EvidenceSecurityFlagPublishEnabled(record.OrganizationID),
	)
}

// buildSessionAnalysis maps the spine's measured columns and R's judged verdict onto
// the session_analyses content row. The measured half is code-owned (the model never
// re-derives it); the judged half comes from R, and the waste ledger (wasted_tokens +
// discarded_ranges) is code-summed over D's discarded ranges.
// judged is true when this pass produces (or carries) a judged verdict — a full R run, or a
// spine-only reprocess carrying a prior JUDGED generation. It is false only for a first-time
// session whose R is flag-dark: that row must land measured-only with judged_at NULL so the
// serving gate (judged_at IS NOT NULL) keeps it out until R is enabled. Passing judged=false leaves JudgedAt nil.
// measuredTurns is the pre-built measured turn set, used here to derive the session-level
// turn-author + tool-call rollups. It is nil on the measured-only spine_done write (read == nil,
// no rollups) and for a source with no turn grouping.
// buildSessionAnalysisRow reads nothing off the processor, so it is a package-level function
// shared verbatim by the batch commit and the single-session front door (single_session.go): the
// two build byte-identical session rows from the same spine result.
func buildSessionAnalysisRow(orgID string, record *model.RawRecordModel, personID string, sp *spineResult, read *ReadResult, discard *DiscardResult, judged bool, measuredTurns []*model.SessionTurnAnalysis) *model.SessionAnalysis {
	// origin marks this as an engine-produced (LIVE) row, distinct from a row carried over by the
	// Session copy backfill (SESSION_COPY). Written on insert and immutable across re-judges
	// (absent from every upsert DoUpdates set), so the provenance stays stable.
	originLive := sessionintelligencepb.SessionAnalysisOrigin_SESSION_ANALYSIS_ORIGIN_LIVE
	analysis := &model.SessionAnalysis{
		Origin:                &originLive,
		OrganizationID:        orgID,
		PersonID:              personID,
		RawRecordID:           record.ID,
		ConversationStartedAt: sp.conversationStartedAt,
		VocabularyVersion:     sp.vocabularyVersion,
		// ide_type carries the agent for the whole row, so a per-phase hitl_gate is read per-agent
		// through it — the four agents' gate vocabularies do not share a scale, so any cross-agent
		// comparison of hitl_gate must go through a crosswalk (the contract), never a
		// direct enum compare.
		IdeType:           toCommonIdeType(sp.ideType),
		JudgedDisposition: &sp.disposition,
	}
	if judged {
		now := time.Now().UTC()
		analysis.JudgedAt = &now
	}
	applyMeasuredColumns(analysis, sp)
	if read != nil {
		analysis.SessionSteering = read.Verdict.SessionSteering
		analysis.GoalAchieved = read.Verdict.GoalAchieved
		analysis.Smoothness = read.Verdict.Smoothness
		analysis.FrictionCause = read.Verdict.FrictionCause
		analysis.Speed = read.Verdict.Speed
		analysis.TokenEfficiency = read.Verdict.TokenEfficiency
		analysis.GoalIsCodingWork = read.Verdict.GoalIsCodingWork
		analysis.UnderlyingGoal = read.Verdict.UnderlyingGoal
		analysis.WorkType = read.Verdict.WorkType
		analysis.ScoreOverall = read.Verdict.ScoreOverall
		analysis.ScoreClarity = read.Verdict.ScoreClarity
		analysis.ScoreSpecificity = read.Verdict.ScoreSpecificity
		analysis.ScoreContextProvision = read.Verdict.ScoreContextProvision
		analysis.ScoreActionability = read.Verdict.ScoreActionability
		analysis.ScoreCompleteness = read.Verdict.ScoreCompleteness
		analysis.ScoreEfficiency = read.Verdict.ScoreEfficiency
		analysis.InvokedSkills = read.Verdict.InvokedSkills
		analysis.HarnessFindings = read.Verdict.HarnessFindings
		// R's flat security readout is persisted on the row (as well as reconciled into
		// security_flags) so a re-pick can carry it by rehydration even when the source's turn grain
		// is not stored — else the empty turn-flatten would drive the last-writer-wins reconcile to
		// delete R's flags. Nil when R found no security issues.
		if len(read.SecurityIssues) > 0 {
			analysis.ReadSecurityIssues = &sessionintelligencepb.PromptIssueList{Items: read.SecurityIssues}
		}
		// E's artifact adjudications and skill assessments (filled by applyEvidence after E
		// ran) land in their own E-owned columns, never merged into R's invoked_skills; nil
		// when E did not fire, so the columns stay NULL rather than storing an empty list.
		analysis.ArtifactAdjudications = read.Verdict.ArtifactAdjudications
		analysis.SkillAssessments = read.SkillAssessments
		// E's per-half readiness, derived from E's call state and this pass's signals — the two
		// halves resolve independently: the
		// round-trip half can be PRODUCED while the
		// artifact half is SKIPPED_PENDING_DATA on the same session. Stamped only in this judged
		// block (read != nil); a spine-only reprocess takes the read == nil branch above and leaves
		// the live row's prior dispositions untouched (the measured-only upserts omit these columns).
		analysis.EvidenceArtifactDisposition, analysis.EvidenceRoundtripDisposition = evidenceHalfDispositions(sp, read)
		applyNewVocabRollup(analysis, sp, read)
		// phase_count is the count of R's judged phases (the phase grain is new-engine-only —
		// copied history has no phase rows, so there is no parity peer to match). Joins the judged
		// column set, alongside the other R-derived rollups.
		analysis.PhaseCount = int32Ptr(int32(len(read.Phases)))
		// weighted_score mirrors the session definition (analyze_prompt.py): the cost-adjusted
		// quality metric overall_quality_score × tokens_consumed. ScoreOverall (R) and TotalTokens
		// (measured) are both on the row by now; nil if either is. It is a point-in-time snapshot as
		// of this judged pass — a delegated child that folds in later (CHILD_REFOLD) grows
		// total_tokens on a spine-only reprocess without recomputing weighted_score, so the metric
		// lags that late token growth until the next full re-judge (accepted: the folded child cost
		// is a small fraction, and weighted_score is a judged column, not a measured one).
		if analysis.ScoreOverall != nil && analysis.TotalTokens != nil {
			analysis.WeightedScore = float64Ptr(float64(*analysis.ScoreOverall) * float64(*analysis.TotalTokens))
		}
		// Turn-author counts + the session tool-call total, from the shared measured turn set
		// (measuredTurns, built once per commit — nil for a source with no turn grouping, which then
		// leaves these NULL rather than a misleading 0). Computed only on a full judged pass
		// (read != nil ⟹ the producers ran ⟹ the turns were rebuilt this commit); they persist via
		// sessionAnalysisJudgeWrittenMeasuredColumns and are left untouched by a spine-only reaffirm
		// and by MarkJudgeFailed (the measured turn rows are still valid on a judge failure).
		if measuredTurns != nil {
			var human, delegated, toolCalls int32
			for _, turn := range measuredTurns {
				if turn.IsSubagent != nil && *turn.IsSubagent {
					delegated++
				} else {
					human++
				}
				if turn.ToolCallCount != nil {
					toolCalls += *turn.ToolCallCount
				}
			}
			analysis.HumanTurnCount = int32Ptr(human)
			analysis.DelegatedTurnCount = int32Ptr(delegated)
			analysis.ToolCallCount = int32Ptr(toolCalls)
		}
		// cap_token_split — the session's token-split capability, from the same data-presence
		// signal (capabilityFor) the LLM capabilities block uses. Computed only inside this full
		// judged pass (read != nil): it is a judge-written column, so a spine-only reaffirm must
		// leave the carried value untouched rather than recompute and clobber it. The other cap_*
		// columns have no clean signal today and stay unset.
		analysis.CapTokenSplit = tokenSplitCapabilityFor(sp.prompts)
	}
	applyWasteLedger(analysis, sp, discard)
	// D's readiness, stamped only on a judged pass (read != nil) — a measured-only build leaves it
	// nil so the measured upserts never publish a disposition for a producer that has not been
	// evaluated. On a judged pass: PRODUCED when D ran (discard != nil), else NOT_APPLICABLE — D
	// never pends (its gate is the spine's own revert/abandon ledger, no external data), so a
	// judged pass where D did not fire is a settled absence. Omitted from the measured-only upserts'
	// conflict-update sets so a spine-only reprocess never clobbers a prior disposition.
	if read != nil {
		discardDisposition := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_NOT_APPLICABLE
		switch {
		case discard != nil:
			discardDisposition = sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED
		case discardCallStatus(sp) == sessionintelligencepb.CallStatus_CALL_STATUS_FAILED:
			// D errored this pass; R committed, so the record is JUDGED-servable and D shows FAILED
			// (retried alone under the failed-producer leg), not a settled NOT_APPLICABLE absence.
			discardDisposition = sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_FAILED
		}
		analysis.DiscardDisposition = &discardDisposition
	}
	return analysis
}

// discardCallStatus returns D's recorded call status from the pass's call state (UNSPECIFIED when
// unset), so the commit can tell a D that errored (FAILED) from one that never fired (NOT_FIRED).
func discardCallStatus(sp *spineResult) sessionintelligencepb.CallStatus {
	if sp.callState == nil {
		return sessionintelligencepb.CallStatus_CALL_STATUS_UNSPECIFIED
	}
	return sp.callState.GetCalls()[producerDiscard].GetStatus()
}

// evidenceCallStatus returns E's recorded call status from the pass's call state (UNSPECIFIED when
// unset) — the commit uses it to tell an E that errored this pass (FAILED, its grain authoritative)
// from one carried DONE or never evaluated.
func evidenceCallStatus(sp *spineResult) sessionintelligencepb.CallStatus {
	if sp.callState == nil {
		return sessionintelligencepb.CallStatus_CALL_STATUS_UNSPECIFIED
	}
	return sp.callState.GetCalls()[producerEvidence].GetStatus()
}

// evidenceHalfDispositions derives E's two per-half readiness values from E's call state and this
// pass's signals. The round-trip half depends only
// on the detector's pairs; the artifact half depends on classified intent, so it alone can be
// SKIPPED_PENDING_DATA. Returns (artifact, roundtrip); a NULL (nil) half means E is dark or unset —
// not a live judged state. read carries EvidenceRan (whether E actually ran a half this pass).
func evidenceHalfDispositions(sp *spineResult, read *ReadResult) (artifact, roundtrip *sessionintelligencepb.ProducerReadiness) {
	produced := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_PRODUCED
	pending := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_SKIPPED_PENDING_DATA
	notApplicable := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_NOT_APPLICABLE
	hasArtifacts := len(sp.ledgers.invocations) > 0
	hasPairs := len(sp.roundTripCandidates) > 0
	evidenceRan := read != nil && read.EvidenceRan

	var status sessionintelligencepb.CallStatus
	if sp.callState != nil {
		if call, ok := sp.callState.GetCalls()[producerEvidence]; ok {
			status = call.GetStatus()
		}
	}
	switch status {
	case sessionintelligencepb.CallStatus_CALL_STATUS_DONE:
		// Both halves adjudicated this pass. A half with no input to adjudicate is a settled
		// absence (NOT_APPLICABLE), not PRODUCED.
		if hasArtifacts {
			artifact = &produced
		} else {
			artifact = &notApplicable
		}
		if hasPairs {
			roundtrip = &produced
		} else {
			roundtrip = &notApplicable
		}
	case sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA:
		// The artifact half is waiting on classified intent; the round-trip half ran this pass iff
		// E fired on the pairs (evidenceRan under a pending E is the round-trip-only run).
		artifact = &pending
		if evidenceRan && hasPairs {
			roundtrip = &produced
		} else {
			roundtrip = &notApplicable
		}
	case sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED:
		artifact = &notApplicable
		roundtrip = &notApplicable
	case sessionintelligencepb.CallStatus_CALL_STATUS_FAILED:
		// E errored this pass while R committed a valid verdict: both halves are FAILED (retried alone
		// under the failed-producer leg), not nil — serving distinguishes a failed E from a dark one.
		failed := sessionintelligencepb.ProducerReadiness_PRODUCER_READINESS_FAILED
		artifact = &failed
		roundtrip = &failed
	default:
		// FLAG_SKIPPED (dark) or unset — leave both nil (no live half state to publish).
	}
	return artifact, roundtrip
}

// applyWasteLedger stamps the code-owned waste ledger from D's discarded ranges: each
// range's token_sum is the measured per-turn token cost over its inclusive span, and
// wasted_tokens is the merged (overlap-deduped) total, capped at the session's own
// total_tokens so waste never exceeds spend (the LLM output contract).
// D never rewrites R's token_efficiency (R is immutable by design).
// A range that does not resolve to the supplied turn table is rejected and dropped BEFORE the
// ranges are assigned, so only the validated set is stored. D not running (nil / no ranges), or
// every range rejected, leaves the ledger unset — a spine-only reprocess carries the prior
// generation's ledger unchanged.
func applyWasteLedger(analysis *model.SessionAnalysis, sp *spineResult, discard *DiscardResult) {
	if discard == nil || discard.DiscardedRanges == nil || len(discard.DiscardedRanges.GetItems()) == 0 {
		return
	}
	validRanges, wasted := sumDiscardedTokens(discard.DiscardedRanges.GetItems(), sp.promptTokenCosts)
	if len(validRanges) == 0 {
		return
	}
	if total := analysis.TotalTokens; total != nil && *total > 0 && wasted > *total {
		wasted = *total
	}
	analysis.WastedTokens = &wasted
	// Store the validated set only — an out-of-range range is dropped, never clamped.
	analysis.DiscardedRanges = &sessionintelligencepb.DiscardedRangeList{Items: validRanges}
}

// applyNewVocabRollup derives the two session-grain serving columns (waste_categories,
// taxonomy_issues) from R's per-turn judgments, with zero extra inference — the engine's new
// vocabulary (WasteCause / PromptIssueSlug), distinct from Session's free-text categories on
// copied history. The two coexist across the cutover boundary; a trend that crosses the boundary
// is already forbidden. category_slug / category are the kebab slug
// (the canonical form R parses, matching security_flags' flag_type), severity is lowercase.
//
// These are R-owned judged columns, called only inside the read != nil (full judged) block. When
// turn rows are present they are recomputed. When they are absent — a carried re-pick of a
// turn-grain-unsupported source, where the empty flatten would compute nil — the stored rollups
// rehydrate carried onto read and are re-written unchanged, so a spine-only reprocess leaves them
// untouched and a failed judge still NULLs them.
func applyNewVocabRollup(analysis *model.SessionAnalysis, sp *spineResult, read *ReadResult) {
	if len(read.Turns) == 0 {
		// No turn rows to recompute from (a turn-grain-unsupported source): carry the stored
		// rollups (may be nil) rather than NULLing them via an empty flatten.
		analysis.TaxonomyIssues = read.CarriedTaxonomyIssues
		analysis.WasteCategories = read.CarriedWasteCategories
		return
	}

	// waste_categories: fold each judged turn's measured token cost into its WasteCause bucket.
	// A cause seen only on out-of-cut turns (PromptIndex beyond the measured cost table), an
	// UNSPECIFIED/NONE cause, or a cause whose in-cut turns all measured 0 cost contributes
	// nothing — a cost breakdown carries no 0-cost cause.
	n := int32(len(sp.promptTokenCosts))
	byCause := map[sessionintelligencepb.WasteCause]int64{}
	for _, turn := range read.Turns {
		if turn.WasteCause == nil {
			continue
		}
		cause := *turn.WasteCause
		if cause == sessionintelligencepb.WasteCause_WASTE_CAUSE_UNSPECIFIED || cause == sessionintelligencepb.WasteCause_WASTE_CAUSE_NONE {
			continue
		}
		if turn.PromptIndex < 0 || turn.PromptIndex >= n {
			continue // out of the measured turn table — skip rather than index out of range
		}
		cost := sp.promptTokenCosts[turn.PromptIndex]
		if cost <= 0 {
			continue // a 0-cost turn mints no bucket
		}
		byCause[cause] += cost
	}
	if len(byCause) > 0 {
		causes := make([]sessionintelligencepb.WasteCause, 0, len(byCause))
		for c := range byCause {
			causes = append(causes, c)
		}
		sort.Slice(causes, func(i, j int) bool { return causes[i] < causes[j] }) // deterministic by enum value
		items := make([]*sessionintelligencepb.TokenBreakdown, 0, len(causes))
		for _, c := range causes {
			items = append(items, &sessionintelligencepb.TokenBreakdown{Category: wasteCauseKebab(c), Tokens: byCause[c]})
		}
		analysis.WasteCategories = &sessionintelligencepb.TokenBreakdownList{Items: items}
	}

	// taxonomy_issues: a flat collection of R's per-turn issue slugs, in PromptIndex order (read.Turns
	// is in LLM-response order, so sort a copy for a deterministic JSONB). Security-layer slugs are
	// NOT filtered out (they are also reconciled into security_flags) — kept on the taxonomy surface.
	// A new-session issue carries no per-issue wasted-tokens estimate (an accepted loss); the
	// descriptive text fields the engine does not produce stay empty.
	orderedTurns := make([]*TurnJudgment, len(read.Turns))
	copy(orderedTurns, read.Turns)
	sort.SliceStable(orderedTurns, func(i, j int) bool { return orderedTurns[i].PromptIndex < orderedTurns[j].PromptIndex })
	var issues []*sessionintelligencepb.TaxonomyIssue
	for _, turn := range orderedTurns {
		for _, pi := range turn.PromptIssues.GetItems() {
			issues = append(issues, &sessionintelligencepb.TaxonomyIssue{
				CategorySlug: promptIssueSlugKebab(pi.GetSlug()),
				Severity:     severityString[pi.GetSeverity()],
				Layer:        pi.GetLayer(),
			})
		}
	}
	if len(issues) > 0 {
		analysis.TaxonomyIssues = &sessionintelligencepb.TaxonomyIssueList{Items: issues}
	}
}

// sumDiscardedTokens sums the measured per-turn token cost over the union of the VALID
// discarded ranges (both bounds inclusive), so two overlapping ranges never double-count the
// same turn (the fence's non-overlap rule is a model instruction, not a guarantee). A range
// whose bound falls outside the turn table (start < 0 or end > n-1), or whose bounds are
// inverted (start > end), is REJECTED and dropped — not clamped — so a hallucinated bound never
// silently pulls in whole-session turns and an inverted range never silently contributes zero
// while still being stored. Each valid range's own token_sum is stamped on it in passing. Returns
// the validated ranges and the merged total; perTurnCost is indexed by prompt_index.
func sumDiscardedTokens(ranges []*sessionintelligencepb.DiscardedRange, perTurnCost []int64) ([]*sessionintelligencepb.DiscardedRange, int64) {
	n := int32(len(perTurnCost))
	if n == 0 {
		return nil, 0
	}
	covered := make([]bool, n)
	validRanges := make([]*sessionintelligencepb.DiscardedRange, 0, len(ranges))
	for _, r := range ranges {
		start, end := r.GetRangeStart(), r.GetRangeEnd()
		if start < 0 || end > n-1 || start > end {
			continue // out of the turn table, or inverted — reject the whole range rather than clamp it
		}
		var rangeSum int64
		for i := start; i <= end; i++ {
			covered[i] = true
			rangeSum += perTurnCost[i]
		}
		r.TokenSum = rangeSum
		validRanges = append(validRanges, r)
	}
	var total int64
	for i := int32(0); i < n; i++ {
		if covered[i] {
			total += perTurnCost[i]
		}
	}
	return validRanges, total
}

// applyMeasuredColumns stamps the code-owned measured scalars the S1 overview reads —
// cost, tokens, the token breakdown, session telemetry, and the structural counts —
// from the spine's ExtractMetrics / ExtractUsageMetadata results (shared with the
// session extractors so the two pipelines cannot drift). Deferred to the turn/phase
// and E/D grain (documented): weighted_score and the input-quality scores, wasted
// tokens, subagent rollup, active_share, tool_call_count, and the surfaced-set lists.
func applyMeasuredColumns(analysis *model.SessionAnalysis, sp *spineResult) {
	analysis.TurnCount = int32Ptr(sp.turnCount)
	analysis.RepoName = nilIfEmpty(sp.repoName)
	// Identity columns the shared blob mapping deliberately omits — set from the spine so a live
	// row carries them the same way a copied row does (the measured-column parity contract): the
	// copy sets session_id from the usage blob and conversation_id from the insight; the live path
	// takes session_id from the same blob and conversation_id from the parsed S3 key.
	analysis.SessionID = nilIfEmpty(sp.usage.GetSessionId())
	analysis.ConversationID = nilIfEmpty(sp.conversationID)
	// last_active_at is the raw record's fetched_at (the desktop app's last-sync time, refreshed on
	// every re-sync) — the session last_conversation_at equivalent, read by the AI-Leverage
	// contributors "last active" column and the PR-attribution forward window. The backfill
	// takes the same value from the insight's last_conversation_at. A zero fetched_at (never set)
	// stays NULL rather than storing a 0001-01-01 sentinel.
	if !sp.lastActiveAt.IsZero() {
		lastActive := sp.lastActiveAt
		analysis.LastActiveAt = &lastActive
	}
	ApplyMeasuredBlobColumns(analysis, sp.metrics, sp.usage)
}

// ApplyMeasuredBlobColumns stamps the code-owned measured scalars from the metrics and
// usage_metadata blobs — cost, tokens, the token breakdown, session telemetry, the seat/plan
// identity, and the verbatim vendor usage blob (session_meta_raw, which lights the STORED
// generated columns). It is the one shared mapping: every caller feeds the SAME ConversationMetrics / UsageMetadata
// protos through here, so the measured columns cannot drift between callers. It deliberately does
// NOT set turn_count or repo_name — each caller sets those itself.
func ApplyMeasuredBlobColumns(analysis *model.SessionAnalysis, metrics *sessionintelligencepb.ConversationMetrics, usage *sessionintelligencepb.UsageMetadata) {
	if m := metrics; m != nil {
		analysis.EstimatedCostUsd = float64Ptr(m.GetEstimatedCostUsd())
		analysis.OwnEstimatedCostUsd = float64Ptr(m.GetOwnEstimatedCostUsd())
		analysis.TotalPrompts = int32Ptr(m.GetTotalPrompts())
		analysis.TotalTokens = int64Ptr(m.GetTotalTokens())
		analysis.TokenBreakdown = &sessionintelligencepb.AnalysisTokenBreakdown{
			InputTokens:                 m.GetInputTokens(),
			OutputTokens:                m.GetOutputTokens(),
			CacheReadTokens:             m.GetCacheReadInputTokens(),
			CacheWriteTokens:            m.GetCacheCreationInputTokens(),
			CacheCreation_5MInputTokens: m.GetCacheCreation_5MInputTokens(),
			CacheCreation_1HInputTokens: m.GetCacheCreation_1HInputTokens(),
			TotalTokens:                 m.GetTotalTokens(),
		}
	}

	if u := usage; u != nil {
		analysis.IterationCount = int32Ptr(u.GetIterationCount())
		analysis.EffectiveActiveDurationS = int32Ptr(int32(u.GetEffectiveActiveDurationS()))
		analysis.EffectiveAiCodePercent = float64Ptr(u.GetEffectiveAiCodePercent())
		analysis.EffectiveContextUsagePercent = float64Ptr(u.GetEffectiveContextUsagePercent())
		analysis.McpServerCount = int32Ptr(u.GetMcpServerCount())
		analysis.IsSessionEffective = boolPtr(u.GetIsSessionEffective())
		analysis.UserInterruptions = int32Ptr(u.GetUserInterruptions())
		analysis.SessionDurationSeconds = int32Ptr(int32(u.GetSessionDurationSeconds()))
		analysis.CacheHitRatePercent = float64Ptr(u.GetCacheHitRatePercent())
		analysis.CompactionCount = int32Ptr(u.GetCompactionCount())
		analysis.FilesEditedCount = int32Ptr(int32(len(u.GetFilesEdited())))
		analysis.ProjectName = nilIfEmpty(u.GetProjectName())
		if models := u.GetModelsUsed(); len(models) > 0 {
			analysis.ModelsUsed = &sessionintelligencepb.StringList{Values: models}
		}
		// Vendor telemetry strings → the closed proto enums; an unrecognised token maps
		// to nil (abstention), never a plausible default.
		analysis.DelegationLevel = vendorEnum("DELEGATION_LEVEL_", u.GetDelegationLevel(), sessionintelligencepb.DelegationLevel_value, func(v int32) sessionintelligencepb.DelegationLevel { return sessionintelligencepb.DelegationLevel(v) })
		analysis.EffectivenessSource = vendorEnum("EFFECTIVENESS_SOURCE_", u.GetEffectivenessSource(), sessionintelligencepb.EffectivenessSource_value, func(v int32) sessionintelligencepb.EffectivenessSource {
			return sessionintelligencepb.EffectivenessSource(v)
		})
		analysis.SessionOutcome = vendorEnum("SESSION_OUTCOME_", u.GetSessionOutcome(), sessionintelligencepb.SessionOutcome_value, func(v int32) sessionintelligencepb.SessionOutcome { return sessionintelligencepb.SessionOutcome(v) })
		// The folded child usage the spine attached (nil when the session delegated nothing).
		if sub := u.GetSubagentUsage(); sub != nil {
			analysis.SubagentUsage = sub
			applyChildDelegationBump(analysis)
		}

		// The verbatim vendor usage blob — the source of truth the STORED generated columns
		// (session_type, cost_is_estimated, cost_is_input_only, user_message_timestamps,
		// mcp_server_names, tool_errors) and the entrypoint expression index derive from. Written
		// here so those hoists resolve; a scalar-aggregate read projects it away to avoid detoast.
		analysis.SessionMetaRaw = u

		// Seat / plan identity. These flat columns mirror keys session_meta_raw already holds; both
		// are written from the same u here so they cannot drift, and both are point-in-time — the
		// installation metadata the desktop app captured at session time, not current identity (a
		// later email/plan change never rewrites a stored row). plan_tier_origin is the closed
		// queryable dimension classified from the open plan_tier_source by the same classifier the
		// session path uses; vendorEnum's nil is the abstention for an unspecified/unrecognised origin.
		analysis.PlanTier = nilIfEmpty(u.GetPlanTier())
		analysis.PlanTierSource = nilIfEmpty(u.GetPlanTierSource())
		analysis.PlanTierOrigin = vendorEnum("PLAN_TIER_ORIGIN_", sessionintelligence.ClassifyPlanTierOrigin(u.GetPlanTierSource()), sessionintelligencepb.PlanTierOrigin_value, func(v int32) sessionintelligencepb.PlanTierOrigin { return sessionintelligencepb.PlanTierOrigin(v) })
		// seat_monthly_price_usd is 0 when the tier has no resolved list price (enterprise/free);
		// store NULL there rather than a misleading $0.
		if p := u.GetSeatMonthlyPriceUsd(); p > 0 {
			analysis.SeatMonthlyPriceUsd = &p
		}
		analysis.HasExtraUsageEnabled = boolPtr(u.GetHasExtraUsageEnabled())
		analysis.AccountUUID = nilIfEmpty(u.GetAccountUuid())
		analysis.LoginEmail = nilIfEmpty(u.GetLoginEmail())
		if langs := u.GetLanguages(); len(langs) > 0 {
			analysis.Languages = &sessionintelligencepb.StringList{Values: langs}
		}
		if fc := u.GetFrictionCounts(); len(fc) > 0 {
			analysis.FrictionCounts = &sessionintelligencepb.FrictionCounts{Counts: fc}
		}
		// active_share is the fraction of the session's wall-clock that was effectively active
		// (effective_active_duration / session_duration), clamped to [0,1]; nil when the duration
		// is unknown so we never store a divide-by-zero or a misleading 0.
		if dur := u.GetSessionDurationSeconds(); dur > 0 {
			share := float64(u.GetEffectiveActiveDurationS()) / float64(dur)
			if share > 1 {
				share = 1
			}
			analysis.ActiveShare = float64Ptr(share)
		}
		// first_user_msg_ts is the first user message's timestamp (userMessageTimestamps[0]) — the
		// session um_first_user_msg_ts equivalent. The raw list also lights the
		// user_message_timestamps generated column; this is its dedicated flat hoist. A malformed
		// vendor timestamp is left as NULL rather than failing the whole measured write.
		if ts := u.GetUserMessageTimestamps(); len(ts) > 0 {
			if parsed, err := time.Parse(time.RFC3339, ts[0]); err == nil { //swallowcheck:ignore -- best-effort: a malformed vendor timestamp is left as NULL (see comment above), never fails the measured write.
				analysis.FirstUserMsgTs = &parsed
			}
		}
	}
}

// applyChildDelegationBump lifts a session with ≥1 delegated child to at least ASSISTED when
// it would otherwise read INTERACTIVE or UNKNOWN — a delegated child is positive evidence the
// session delegated work, matching the session fold's delegation bump. A DELEGATED session
// stays DELEGATED, and an abstained (nil) level stays nil.
func applyChildDelegationBump(analysis *model.SessionAnalysis) {
	if analysis.SubagentUsage.GetChildCount() == 0 || analysis.DelegationLevel == nil {
		return
	}
	switch *analysis.DelegationLevel {
	case sessionintelligencepb.DelegationLevel_DELEGATION_LEVEL_INTERACTIVE, sessionintelligencepb.DelegationLevel_DELEGATION_LEVEL_UNKNOWN:
		assisted := sessionintelligencepb.DelegationLevel_DELEGATION_LEVEL_ASSISTED
		analysis.DelegationLevel = &assisted
	default:
		// assisted / delegated / unspecified: leave as-is.
	}
}

// vendorEnum maps a lowercase vendor telemetry string (e.g. "cc_facets",
// "fully_achieved") to its proto enum by uppercasing and prefixing to the enum's
// constant name, returning nil for the empty, unrecognised, or zero (UNSPECIFIED)
// value — the abstention posture.
func vendorEnum[T ~int32](prefix, value string, valueMap map[string]int32, mk func(int32) T) *T {
	if value == "" {
		return nil
	}
	v, ok := valueMap[prefix+strings.ToUpper(value)]
	if !ok || v == 0 {
		return nil
	}
	out := mk(v)
	return &out
}

// vendorEnumValue is vendorEnum for a non-pointer (value) field: a missing or unrecognized vendor
// label resolves to the zero (UNSPECIFIED) enum rather than panicking on a nil deref — the model
// can emit a malformed enum, and that must degrade the field, not crash the worker.
func vendorEnumValue[T ~int32](prefix, value string, valueMap map[string]int32, mk func(int32) T) T {
	if parsed := vendorEnum(prefix, value, valueMap, mk); parsed != nil {
		return *parsed
	}
	var zero T
	return zero
}

func int32Ptr(v int32) *int32       { return &v }
func int64Ptr(v int64) *int64       { return &v }
func float64Ptr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool          { return &v }

// startOfDayUTC normalises to the conversation's UTC calendar day — the flag_date
// anchor, matching the session path so a first sync does not stamp today's date.
func startOfDayUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
