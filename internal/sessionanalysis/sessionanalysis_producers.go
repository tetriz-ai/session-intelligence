// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// rehydrateCarriedRead reconstructs R's ReadResult from the live committed rows (no S3) so a
// resolved re-pick carries R rather than re-inferring it. It
// opens its own tenant scope for the three reads — it runs inside the producer flow, which holds
// no open transaction.
func (p *SessionAnalysisProcessor) rehydrateCarriedRead(ctx context.Context, orgID, rawRecordID string) (*ReadResult, error) {
	var analysis *model.SessionAnalysis
	var turns []*model.SessionTurnAnalysis
	var phases []*model.SessionPhaseAnalysis
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		var err error
		if analysis, err = p.sessionAnalysisDAO.GetByRawRecordID(sctx, rawRecordID); err != nil {
			return fmt.Errorf("get session analysis for rehydrate (raw_record %s): %w", rawRecordID, err)
		}
		if turns, err = p.sessionTurnDAO.ListByRawRecordID(sctx, rawRecordID); err != nil {
			return fmt.Errorf("list turns for rehydrate (raw_record %s): %w", rawRecordID, err)
		}
		if phases, err = p.sessionPhaseDAO.ListBySessionAnalysisID(sctx, analysis.ID); err != nil {
			return fmt.Errorf("list phases for rehydrate (raw_record %s): %w", rawRecordID, err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("rehydrate carried read scope (raw_record %s): %w", rawRecordID, err)
	}
	return rehydrateReadResult(analysis, turns, phases), nil
}

// carryReadDone rehydrates R from the persisted rows (never re-inferred) and stamps producerRead
// DONE at the current READ contract. Call it only when R is settled at that contract, so the
// stamp accurately describes the carried prior-generation verdict.
func (p *SessionAnalysisProcessor) carryReadDone(ctx context.Context, orgID, rawRecordID string, sp *spineResult) (*ReadResult, error) {
	carried, err := p.rehydrateCarriedRead(ctx, orgID, rawRecordID)
	if err != nil {
		return nil, fmt.Errorf("rehydrating carried read for raw record %s: %w", rawRecordID, err)
	}
	sp.setCallStatus(producerRead, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, p.cfg.ContractVersionRead())
	return carried, nil
}

// carryDiscardDone rehydrates a settled D from its committed waste ledger and stamps producerDiscard
// DONE at the current DISCARD contract, so an E-only resolved re-pick carries D forward without
// re-billing AnalyzeDiscard. Call it only when D's prior call was DONE at the current contract and R
// is itself carried on an unchanged body — the guarantees rehydrateDiscardResult's identical-ledger
// recompute relies on.
func (p *SessionAnalysisProcessor) carryDiscardDone(ctx context.Context, orgID, rawRecordID string, sp *spineResult) (*DiscardResult, error) {
	var analysis *model.SessionAnalysis
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		var err error
		if analysis, err = p.sessionAnalysisDAO.GetByRawRecordID(sctx, rawRecordID); err != nil {
			return fmt.Errorf("get session analysis for discard rehydrate (raw_record %s): %w", rawRecordID, err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("rehydrate carried discard scope (raw_record %s): %w", rawRecordID, err)
	}
	sp.setCallStatus(producerDiscard, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, p.cfg.ContractVersionDiscard())
	return rehydrateDiscardResult(analysis), nil
}

// anyProducerFailed reports whether the prior generation carries any producer in CALL_STATUS_FAILED
// — the signal that a producer errored while R committed a valid verdict, so the failed-producer
// retry should re-run only that producer.
func anyProducerFailed(prior *model.SessionAnalysisState) bool {
	if prior == nil {
		return false
	}
	for _, call := range prior.CallState.GetCalls() {
		if call.GetStatus() == sessionintelligencepb.CallStatus_CALL_STATUS_FAILED {
			return true
		}
	}
	return false
}

// priorReadInputUnchanged reports whether the current spine's R input hash matches the one the prior
// generation stamped — i.e. the body (and R contract) are unchanged since R last ran. A failed-producer
// retry carries R only when this holds; a changed body is routed to CONTENT_CHANGED by the scan (higher
// precedence) and re-infers R there, but this guard makes a race safe.
func priorReadInputUnchanged(prior *model.SessionAnalysisState, sp *spineResult, contractVersionRead int32) bool {
	if prior == nil {
		return false
	}
	call, ok := prior.CallState.GetCalls()[producerRead]
	if !ok {
		return false
	}
	return call.GetInputHash() == ReadInputHash(sp.contentSha256, contractVersionRead)
}

// producerDone reports whether a prior producer call is DONE at or beyond the current contract —
// a stricter check than producerSettled, which also admits NOT_FIRED. The D carry-forward on an
// E-only re-pick keys on this: only a DONE D made a paid inference worth preserving, whereas a
// NOT_FIRED D re-evaluates its spine-ledger fire gate for free and idempotently on the unchanged body.
func producerDone(prior *model.SessionAnalysisState, producer string, currentContract int32) bool {
	call, ok := prior.CallState.GetCalls()[producer]
	if !ok {
		return false
	}
	return call.GetStatus() == sessionintelligencepb.CallStatus_CALL_STATUS_DONE && call.GetContractVersion() >= currentContract
}

// runProducers runs R, then E (the conditional evidence pass), then the D discard tail,
// unless the record is a spine-only reprocess — a spine-version bump that left every
// producer-read input unchanged, so every call skips at zero LLM cost (the zero-cost
// path). A dark producer's flag leaves its call FLAG_SKIPPED rather than run, and category
// 9 re-admits the row when the flag turns on. E enriches R's ReadResult in place (it runs
// after R), so the returned read carries E's adjudications,
// merged skill assessments, and per-turn round-trip faults.
func (p *SessionAnalysisProcessor) runProducers(ctx context.Context, orgID string, record *model.RawRecordModel, meta *rawRecordMetadata, prior *model.SessionAnalysisState, sp *spineResult, detectorSem chan struct{}) (*ReadResult, *DiscardResult, error) {
	if sp.spineOnly {
		// The prior generation's call_state is carried unchanged (no producer ran),
		// so the commit re-stamps only the measured half.
		return nil, nil, errProducersSkipped
	}
	// The LLM calls must run outside any open transaction: a held-open tx
	// idles until PostgreSQL kills it. AssertNoOpenTx enforces this at runtime.
	tenantdb.AssertNoOpenTx(ctx)
	req := ProducerRequest{
		OrgID:                orgID,
		PersonID:             meta.PersonID,
		SessionID:            record.ID,
		Content:              sp.llmContent,
		UseSystemPromptCache: p.cfg.UseSystemPromptCache(),
		Tags:                 producerTags("analyze_read", orgID, sp.vocabularyVersion),
	}
	if !p.cfg.ReadEnabled(orgID) {
		// R is dark (the master SessionAnalysisEnabled gate is off for this org): R is
		// FLAG_SKIPPED, the row lands spine_done unserved (judged_at stays NULL), and the
		// dual-source router serves the session window instead. R gates the tail — this never runs E or D either.
		//
		// Carry the prior generation's full call_state so every already-settled producer's entry
		// survives this measured-only commit — the commit writes call_state as one column, so a
		// dropped entry (a DONE D, a pending E) reads as unsettled next pass and forces a wasteful
		// re-run. R is stamped FLAG_SKIPPED on top; cloneCallState(nil) yields an empty map for a
		// first-time row.
		sp.callState = cloneCallState(priorCallState(prior))
		sp.setCallStatus(producerRead, sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED, p.cfg.ContractVersionRead())
		sp.disposition = dispositionFor(sp.flagSkippedProducers | sp.pendingDataProducers | sp.failedProducers)
		return nil, nil, errProducersSkipped
	}

	// Per-producer re-pick: a re-admitted record whose E half
	// went SKIPPED_PENDING_DATA is resolved from its PERSISTED dependency — no S3, and R is never
	// re-inferred. The intent read is the evidenceResolver seam. read is threaded through the rest
	// of the producer flow: a resolved re-pick (or a D-only re-run beneath a still-pending E)
	// carries R into it; every other path leaves it nil so R infers below. evidenceStaysPending
	// bypasses the E block when only D re-ran under a pending E.
	var read *ReadResult
	var err error
	var evidenceStaysPending bool
	// evidenceStaysSettled is set on a D-only failed-producer retry where E already succeeded: it
	// carries E's DONE grain (the commit preserves its columns) and bypasses the E block so E is
	// never re-billed while only D retries.
	var evidenceStaysSettled bool
	// carriedDiscard is a settled D rehydrated on an E-only resolved re-pick; discardStaysSettled
	// then bypasses D's fire gate below so a DONE D is never re-billed on an unchanged body.
	var carriedDiscard *DiscardResult
	var discardStaysSettled bool
	if pending := pendingEvidenceCall(prior); pending != nil {
		resolver := p.evidenceResolver
		if resolver == nil {
			resolver = defaultEvidenceResolver
		}
		// R is carried by rehydration across a re-pick ONLY when R itself is settled at the current
		// contract. A concurrent READ contract bump (R unsettled) must re-infer R below so its verdict
		// and stamped contract reflect the new contract: carrying it and stamping the current contract
		// on a prior-contract verdict would freeze R while the row falsely claims the new version, and
		// the category-4 VERSION_BEHIND scan leg would never re-admit it.
		readSettled := producerSettled(prior, producerRead, p.cfg.ContractVersionRead())
		decision, rerr := evidenceRePickDecision(prior, sp.contentSha256, func(artifactID string) (bool, error) {
			return resolver(ctx, orgID, artifactID)
		})
		if rerr != nil {
			// A real intent-classification DB read failed: defer non-counting (no commit, no
			// markFailed) so lease expiry re-admits the row rather than stranding it or burning a
			// retry. Mirrors the detector-unavailable deferral.
			logger.WarnWithCtx(ctx, "session analysis: intent resolver unavailable on re-pick, deferring record without counting",
				zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID), zap.Error(rerr))
			sessionEvidenceIntentDeferredTotal.WithLabelValues(orgID).Inc()
			return nil, nil, errIntentResolverUnavailable
		}
		switch decision {
		case rePickReSkip:
			repickDecisionsTotal.WithLabelValues(orgID, repickDecisionReskip).Inc()
			// The intent is still unresolved: keep E pending. Carry the prior call_state only when R is
			// settled; when R's contract bumped, keep the fresh spine call_state (correct R input hash +
			// contract) so the re-inferred R below is stamped consistently.
			if readSettled {
				sp.callState = cloneCallState(prior.CallState)
			}
			sp.setPendingData(producerEvidence, p.cfg.ContractVersionEvidence(), pending.GetPendingArtifactIds(), sp.contentSha256)
			evidenceStaysPending = true
			switch {
			case !readSettled:
				// R's contract bumped: re-infer R below (read stays nil); E stays pending, D runs per gate.
			case producerSettled(prior, producerDiscard, p.cfg.ContractVersionDiscard()):
				// E is the only unsettled producer and R needs no re-run — take the measured-reaffirm
				// commit path (read == nil && spineOnly); TransitionToJudged re-writes the pending bit.
				sp.disposition = dispositionFor(sp.flagSkippedProducers | sp.pendingDataProducers | sp.failedProducers)
				sp.spineOnly = true
				return nil, nil, errProducersSkipped
			default:
				// D is INDEPENDENTLY unsettled: carry R, keep E pending, let the D block run below.
				read, err = p.carryReadDone(ctx, orgID, record.ID, sp)
				if err != nil {
					return nil, nil, fmt.Errorf("carrying read for D-unsettled evidence re-pick (raw record %s): %w", record.ID, err)
				}
			}
		case rePickResolved:
			repickDecisionsTotal.WithLabelValues(orgID, repickDecisionResolved).Inc()
			// The pending data landed: E (now resolvable) re-runs below on the current spine. Carry R
			// when settled; when R's contract bumped, re-infer it below (read stays nil).
			if readSettled {
				read, err = p.carryReadDone(ctx, orgID, record.ID, sp)
				if err != nil {
					return nil, nil, fmt.Errorf("carrying read for resolved evidence re-pick (raw record %s): %w", record.ID, err)
				}
				// D too: the resolved re-pick leaves the body unchanged and carries R, so a DONE D's
				// ledger is still valid. Re-firing would re-bill AnalyzeDiscard and could overwrite the
				// committed ledger with a nondeterministic result — carry it and let only E re-run. A
				// NOT_FIRED D is not carried: its fire gate re-evaluates for free and idempotently below.
				if producerDone(prior, producerDiscard, p.cfg.ContractVersionDiscard()) {
					carriedDiscard, err = p.carryDiscardDone(ctx, orgID, record.ID, sp)
					if err != nil {
						return nil, nil, fmt.Errorf("carrying discard for resolved evidence re-pick (raw record %s): %w", record.ID, err)
					}
					discardStaysSettled = true
				}
			}
		case rePickResync:
			repickDecisionsTotal.WithLabelValues(orgID, repickDecisionResync).Inc()
			// The body changed since E went pending: fall through to a full re-run (read stays nil,
			// so R re-infers below); the fresh E status clears the pending bit.
		}
	}

	// Failed-producer retry: a re-admitted record (CategoryProducerFailed)
	// whose E or D last errored (CALL_STATUS_FAILED) while R committed a valid verdict. Carry R by
	// rehydration and re-run ONLY the failed producer, never re-billing R or a producer that already
	// succeeded. The scan routes a CHANGED body to CONTENT_CHANGED (higher precedence), so this path is
	// an unchanged body; the R-input-hash guard is belt-and-braces. Not entered when the pending-E block
	// already carried/re-inferred R (read != nil) or is deferring under a pending E (evidenceStaysPending).
	if read == nil && !evidenceStaysPending && anyProducerFailed(prior) &&
		producerSettled(prior, producerRead, p.cfg.ContractVersionRead()) &&
		priorReadInputUnchanged(prior, sp, p.cfg.ContractVersionRead()) {
		read, err = p.carryReadDone(ctx, orgID, record.ID, sp)
		if err != nil {
			return nil, nil, fmt.Errorf("carrying read for failed-producer retry (raw record %s): %w", record.ID, err)
		}
		// A D-only retry (E already DONE): carry E's grain — re-stamp its DONE status so the E block
		// skips it (evidenceStaysSettled) and the commit preserves its columns (UpsertJudged's
		// evidenceRan=false carry). An E retry leaves E unsettled (FAILED) so the E block re-runs it.
		if producerDone(prior, producerEvidence, p.cfg.ContractVersionEvidence()) {
			sp.setCallStatus(producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, p.cfg.ContractVersionEvidence())
			evidenceStaysSettled = true
		}
		// A settled DONE D is carried too (mirrors the resolved-re-pick D carry): only the FAILED
		// producer re-runs. A FAILED or absent D is left to its fire gate below.
		if producerDone(prior, producerDiscard, p.cfg.ContractVersionDiscard()) {
			carriedDiscard, err = p.carryDiscardDone(ctx, orgID, record.ID, sp)
			if err != nil {
				return nil, nil, fmt.Errorf("carrying discard for failed-producer retry (raw record %s): %w", record.ID, err)
			}
			discardStaysSettled = true
		}
	}

	// The PAIRS detector scores the session's candidate round-trip pairs from the completed
	// spine and stashes the recall-oriented top-N on the spine result, so E's round-trip half
	// consumes scored candidates rather than re-detecting.
	// It runs BEFORE the paid R call: its inputs (P/Q/agent-response text) are spine-only, so a
	// detector failure must not re-bill R on retry — the record fails before runReadPass. Gated
	// by the per-org E gate (EvidenceEnabled): the detector produces E's round-trip candidates,
	// so it runs only when E is armed for the org. sp.roundTripCandidates is the seam the E
	// round-trip half reads — the read side is owned by the E-assembly slice, so this only
	// produces the candidates.
	if !evidenceStaysPending && !evidenceStaysSettled && p.detector != nil && p.cfg.EvidenceEnabled(orgID) {
		pairs, drops := enumerateGuardValidPairs(orgID, record.ID, sp)
		recordGuardDrops(orgID, drops)
		// Hold the per-batch detector slot only for the scoring call — the closure's defer
		// releases it before R/E/D run, so the bound governs concurrent detector calls, not the
		// whole (slow) producer chain.
		detected, detectErr := func() (*DetectorResult, error) {
			if detectorSem != nil {
				select {
				case detectorSem <- struct{}{}:
					defer func() { <-detectorSem }()
				case <-ctx.Done():
					return nil, fmt.Errorf("waiting for detector slot for raw record %s: %w", record.ID, ctx.Err())
				}
			}
			return p.detector.Detect(ctx, orgID, record.ID, pairs, p.cfg.DetectorTopN(), p.cfg.DetectorPairCeiling())
		}()
		if detectErr != nil {
			// An availability failure (503/timeout/transport/5xx) is the detector service's, not
			// the record's: defer without counting so a warming/disabled/down detector cannot
			// exhaust the record to JUDGE_FAILED before R runs. A deterministic error still fails.
			if isProviderTransient(detectErr) {
				roundTripDetectorDeferredTotal.WithLabelValues(orgID).Inc()
				logger.WarnWithCtx(ctx, "session analysis: detector unavailable, deferring record without counting",
					zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID), zap.Error(detectErr))
				return nil, nil, errDetectorUnavailable
			}
			return nil, nil, fmt.Errorf("detect round-trip pairs for raw record %s: %w", record.ID, detectErr)
		}
		sp.roundTripCandidates = detected.Candidates
		sp.detectorTruncated = detected.Truncated
	}

	// Resolve the session's invoked artifacts' intent BEFORE the paid R call, mirroring the detector
	// above: a transient intent-classification / inventory DB fault must defer the whole pass without
	// re-billing R on retry. Only relevant when the session invoked resolvable artifacts (an
	// artifact invocation makes firesEvidence true on its own), and not when a still-pending E
	// is only being carried past a D-only re-run (evidenceStaysPending). Each error site
	// inside resolveEvidencePendingArtifactIDs increments its own domain counter before returning.
	var evidencePendingArtifactIDs []string
	var evidenceResolvedIntent map[string]coreaidlcv1.ArtifactIntentClass
	var evidencePendingResolved bool
	if !evidenceStaysPending && !evidenceStaysSettled && len(sp.ledgers.invocations) > 0 {
		ids, resolved, pendingErr := p.resolveEvidencePendingArtifactIDs(ctx, orgID, meta.PersonID, sp)
		if pendingErr != nil {
			logger.WarnWithCtx(ctx, "session analysis: intent resolver unavailable, deferring record without counting",
				zap.String(logger.OrgID, orgID), zap.String("raw_record_id", record.ID), zap.Error(pendingErr))
			return nil, nil, errIntentResolverUnavailable
		}
		evidencePendingArtifactIDs = ids
		evidenceResolvedIntent = resolved
		evidencePendingResolved = true
	}

	// Infer R only when it was not carried by a resolved re-pick (rehydrateReadResult set read
	// above). A carried R's turns are the persisted supplied-turn set, so the coverage check —
	// which validates a freshly-inferred R against the current spine's turn map — applies only to
	// the fresh path.
	if read == nil {
		read, err = p.runReadPass(ctx, sp, req)
		if err != nil {
			return nil, nil, fmt.Errorf("analyze_read for raw record %s: %w", record.ID, err)
		}
		// R must return exactly one verdict per turn supplied to it (PRD one-verdict-per-supplied-
		// turn): every supplied raw index appears in read.Turns exactly once — no duplicate, none
		// missing. A bare count match would pass a duplicate+missing pair (a phantom padding the
		// count while a real turn goes unjudged); this exact-set check does not. turnIndexMap is the
		// whole-session machine-turn-filtered taggable turn set the spine renders once (spine.go):
		// its VALUES are the raw indices supplied to R on the single-chunk path AND, across every
		// chunked window, the union of each window's rebased main-range indices (both cut the same
		// splitHumanTurns/prefixHumanTurnFilter set, and the windows partition the turn set exactly
		// once, so a well-behaved R produces this value set each once) — never the raw sp.turnCount,
		// which would false-fail a machine-turn-filtered session. read.Turns is already rebased to
		// raw whole-session indices at this point (a hallucinated tag rebased to -1).
		//
		// An out-of-set/hallucinated index is DROPPED (not failed): it recurs on the identical
		// re-cut of the unchanged body, so failing on it would loop the record to JUDGE_FAILED on a
		// paid read — the reasoning the commit's out-of-cut drop is built on. The drop is applied to
		// read.Turns itself, not merely to the coverage tally, so a phantom turn can neither fire E
		// (confirmedRoundTrip scans read.Turns) nor — once its security issues are removed from the
		// flattened list (below) — open a security flag (commit reconciles read.SecurityIssues
		// without an index check). A duplicate or an uncovered supplied turn is a hard failure to
		// markFailed.
		// Enforce R's one-verdict-per-supplied-turn contract over the single-chunk turn set via the
		// shared helper (the single-session path calls the identical check), dropping phantoms and
		// rebuilding the flattened security list from the survivors.
		if err := enforceReadTurnCoverage(ctx, read, sp.turnIndexMap, orgID, fmt.Sprintf("raw record %s", record.ID)); err != nil {
			return nil, nil, err
		}
		// Validate R's phase ranges against the same prompt domain, for the same reason: a
		// hallucinated range beyond the real prompt count would store a corrupt phase turn_count /
		// token sum. Clamp each range to [0, promptCount) and drop a phase that does not intersect
		// it, re-deriving turn_count from the clamped range.
		read.Phases = clampPhaseRangesToDomain(read.Phases, int32(len(sp.promptTokenCosts)))
		sp.setCallStatus(producerRead, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, p.cfg.ContractVersionRead())
	}

	// E — the evidence pass. E has two halves: artifact/review adjudication (needs the peer's
	// classified intent) and round-trip fault (needs only the detector's scored pairs, no intent).
	// It FIRES on either signal — a non-empty artifact ledger OR detector-scored round-trip pairs
	//. Only the artifact
	// half waits: when the ledger is non-empty AND its artifacts are unclassified, E still runs its
	// round-trip half and commits those rows now, carrying SKIPPED_PENDING_DATA for E as a whole so
	// the artifact half re-admits — in full, replacing the round-trip rows by prompt index — once
	// intent lands. When nothing is adjudicable, E is NOT_FIRED (a settled absence).
	if evidenceStaysPending {
		// A D-only re-run beneath a still-pending E: E keeps its SKIPPED_PENDING_DATA state
		// (re-stamped above), so this pass does not touch it.
	} else if evidenceStaysSettled {
		// A D-only failed-producer retry: E already succeeded and its DONE status is carried (its
		// columns are preserved by the commit). Do not re-run or re-stamp it.
	} else if sp.ledgers.firesEvidence(read) || len(sp.roundTripCandidates) > 0 {
		artifactHalfPending := evidencePendingResolved && len(evidencePendingArtifactIDs) > 0
		hasRoundTripPairs := len(sp.roundTripCandidates) > 0
		if artifactHalfPending && !hasRoundTripPairs {
			// Nothing E can produce this pass — the artifact half is pending and there are no
			// round-trip pairs to run. Defer the whole producer with exactly the pending ids; the
			// per-producer re-pick resumes it once intent lands, reading only those ids (no S3).
			sp.setPendingData(producerEvidence, p.cfg.ContractVersionEvidence(), evidencePendingArtifactIDs, sp.contentSha256)
		} else {
			evidenceReq := req
			evidenceReq.Tags = producerTags("analyze_evidence", orgID, sp.vocabularyVersion)
			// E reads a compact, E-shaped request (the LEDGER, INVOCATIONS, PAIRS, CAPABILITIES blocks
			// of the ANALYZE_EVIDENCE_FENCE vocabulary), NOT R's full raw transcript. When the artifact
			// half is pending, render round-trip-only so E adjudicates the pairs alone; otherwise render
			// the full request carrying each invocation's resolved intent so E identifies reviewers by
			// classified intent, never by slug name.
			evidenceReq.Content = buildEvidenceContent(orgID, record.ID, sp, artifactHalfPending, evidenceResolvedIntent)
			evidence, evidenceErr := p.producer.AnalyzeEvidence(ctx, evidenceReq)
			if evidenceErr != nil {
				// R already committed a valid verdict — do not fail the whole record. Stamp E FAILED so
				// only E retries (carrying R) under the failed-producer leg; D still runs below (it is
				// independent of E). The record commits R as JUDGED and serves it.
				p.markProducerFailed(ctx, orgID, producerEvidence, p.cfg.ContractVersionEvidence(), sp, evidenceErr)
			} else {
				applyEvidence(read, evidence)
				if artifactHalfPending {
					// The round-trip rows commit this pass (applyEvidence set EvidenceRan), but the artifact
					// half is still pending, so E as a whole is SKIPPED_PENDING_DATA and re-admits when intent
					// lands. Suppress any artifact-half output so a pending pass never persists ungrounded
					// adjudications; the intent-arrival re-run produces and commits them.
					read.Verdict.ArtifactAdjudications = nil
					read.SkillAssessments = nil
					sp.setPendingData(producerEvidence, p.cfg.ContractVersionEvidence(), evidencePendingArtifactIDs, sp.contentSha256)
				} else {
					sp.setCallStatus(producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, p.cfg.ContractVersionEvidence())
				}
			}
		}
	} else {
		sp.setCallStatus(producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, p.cfg.ContractVersionEvidence())
	}

	// D — the discard tail. It runs after R and E (R gates the tail), and only where the
	// spine's branch/revert ledger found revert/abandon evidence; its judged ranges feed the
	// code-summed waste ledger (wasted_tokens). When the session has no revert/abandon
	// evidence it is NOT_FIRED (an absence, which clears its flag-skipped bit) rather than
	// run.
	var discard *DiscardResult
	switch {
	case discardStaysSettled:
		// Carried from the settled prior generation on an E-only resolved re-pick (call_state stamped
		// DONE by carryDiscardDone). The commit re-writes the identical ledger from the unchanged body;
		// D is not re-billed and its committed ranges are not overwritten by a fresh nondeterministic run.
		discard = carriedDiscard
	case sp.ledgers.firesDiscard():
		discardReq := req
		discardReq.Tags = producerTags("analyze_discard", orgID, sp.vocabularyVersion)
		// D reads a compact D-shaped request (goal, phases, the turn body with per-turn
		// downstream_impact, the measured branch ledger, capabilities — the ANALYZE_DISCARD_FENCE
		// vocabulary), NOT R's full raw transcript. read is guaranteed non-nil here (R gates the tail).
		discardReq.Content = buildDiscardContent(sp, read)
		discardResult, discardErr := p.producer.AnalyzeDiscard(ctx, discardReq)
		if discardErr != nil {
			// R (and E) already committed valid grain — do not fail the whole record. Stamp D FAILED so
			// only D retries (carrying R and E) under the failed-producer leg; the record commits R as
			// JUDGED and serves it. discard stays nil this pass (its ledger is left unchanged by the commit).
			p.markProducerFailed(ctx, orgID, producerDiscard, p.cfg.ContractVersionDiscard(), sp, discardErr)
		} else {
			discard = discardResult
			// D tagged its ranges by rendered Sequence position (same whole-session render R reads);
			// rebase them to the raw whole-session Index the waste ledger and turn table use, exactly
			// as R's read is rebased — else a machine-turn-filtered session mis-attributes its waste.
			rebaseDiscardToRawIndex(discard, sp.turnIndexMap)
			sp.setCallStatus(producerDiscard, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, p.cfg.ContractVersionDiscard())
		}
	default:
		sp.setCallStatus(producerDiscard, sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, p.cfg.ContractVersionDiscard())
	}
	sp.disposition = dispositionFor(sp.flagSkippedProducers | sp.pendingDataProducers | sp.failedProducers)
	return read, discard, nil
}

// producerTags builds the per-trace Langfuse tags for one producer call.
func producerTags(taskType, orgID string, vocabularyVersion int32) map[string]string {
	return map[string]string{
		"task_type":          taskType,
		"organization_id":    orgID,
		"vocabulary_version": fmt.Sprintf("%d", vocabularyVersion),
	}
}
