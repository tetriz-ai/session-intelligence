// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/errgroup"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/s3"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
	"github.com/tetriz-ai/session-intelligence/security"
)

// ConnectorID is the virtual connector the engine registers under. It reads raw_records
// WHERE connector_id=SessionConnectorID but tracks its own progress in session_analysis_state.
const ConnectorID = "session_analysis"

// SessionConnectorID is the connector_id label of the input session raw records the engine
// reads (never writes). Relabel it to match the connector that produces your session data.
const SessionConnectorID = "session"

// errRequeue is the sentinel a commit returns when its lease-only CAS did not match — the
// lease was stolen (claim_token rotated) or the row left the SPINE_DONE/PENDING claim window:
// the transaction rolls back and the row is left actionable for the next tick, which is not a
// failure. A child sighted mid-flight does NOT cause this (the CAS is lease-only).
var errRequeue = errors.New("session analysis: commit CAS did not match, re-queue")

// errProducersSkipped is the sentinel runProducers returns when no producer ran — a
// spine-only reprocess or a FLAG_SKIPPED dark R. It is a control signal, not a
// failure: the caller commits the measured-only path (read == nil) rather than marking
// the record failed.
var errProducersSkipped = errors.New("session analysis: no producer ran this pass")

// errDetectorUnavailable is the sentinel runProducers returns when the PAIRS detector's
// scoring call fails on AVAILABILITY (a 503/timeout/transport/5xx from the token-free detector
// service — isProviderTransient), before the paid R call. It is a NON-counting deferred retry,
// not a failure: the caller leaves the SPINE_DONE row actionable (no commit, no markFailed, no
// attempt increment) so a later tick re-runs it once the detector recovers, via lease expiry
// rather than the outage-gated re-admission. Without this a detector outage smaller than the
// outage-signal floor would increment every retry and exhaust records to JUDGE_FAILED even
// though R never ran. A DETERMINISTIC detector error (a 4xx/parse — recurs identically) is not
// this: it still exhausts through markFailed at the retry cap.
var errDetectorUnavailable = errors.New("session analysis: detector unavailable, deferring without counting")

// errIntentResolverUnavailable is the sentinel runProducers returns when a real intent-classification
// DB read fails (not a missing classification row, which is ErrNotFound → unresolved). Like
// errDetectorUnavailable it is a NON-counting deferred retry, not a failure: the caller leaves the
// row actionable (no commit, no markFailed, no attempt increment) so a transient DB fault re-admits
// it via lease expiry rather than stranding it or burning its retry budget. It fires on both the
// first-run resolve and the re-pick resolve.
var errIntentResolverUnavailable = errors.New("session analysis: intent resolver unavailable, deferring without counting")

// errReadTurnCoverageMismatch is returned when R's turn verdicts do not cover the supplied
// turn set exactly once — the contract one-verdict-per-supplied-turn invariant. It catches
// under-emit (a supplied turn got no verdict) and a duplicate verdict for one turn,
// neither of which a bare count match would detect (a hallucinated index padding the count
// hides a missing turn). An out-of-set/hallucinated index is instead dropped, not failed — it
// recurs on the identical re-cut, so failing on it would loop the record to JUDGE_FAILED. It
// is a hard failure that flows to markFailed, not the commit's silent out-of-cut drop.
var errReadTurnCoverageMismatch = errors.New("session analysis: read pass turn verdicts do not cover the supplied turn set exactly once")

// SessionAnalysisProcessor is the virtual processor: it scans raw_records for
// actionable sessions, runs the spine and the R/E/D producers, and writes the
// session-grain content plus the security-flag reconcile under its own state
// machine. It is a cross-date, budget-capped processor. It is a sibling of the
// session processor.
type SessionAnalysisProcessor struct {
	rawRecordDAO             dao.IRawRecordDAO
	sessionAnalysisDAO       dao.ISessionAnalysisDAO
	sessionStateDAO          dao.ISessionAnalysisStateDAO
	sessionTurnDAO           dao.ISessionTurnAnalysisDAO
	sessionPhaseDAO          dao.ISessionPhaseAnalysisDAO
	sessionRoundTripFaultDAO dao.ISessionRoundTripFaultDAO
	securityFlagDAO          dao.ISecurityFlagDAO
	// securityReconciler (the #3 detector) maps R's/E's security-layer prompt issues to
	// open security flags and persists them; the engine calls it from the E-pass.
	securityReconciler *security.Reconciler
	// harnessArtifactDAO resolves a session's invoked-artifact slugs to live harness_artifacts
	// rows (VCS-first, DESKTOP fallback); classificationDAO reads each resolved row's intent — the
	// two reads the E artifact-intent resolver is built on.
	harnessArtifactDAO dao.IHarnessArtifactDAO
	classificationDAO  dao.IHarnessArtifactClassificationDAO
	s3Client           s3.S3Client
	producer           LLMProducer
	// detector scores the round-trip candidate pairs that feed E's round-trip half. Nil when
	// the reference-llm detector surface is not wired (older wire graphs); the engine guards on
	// it and on the per-org EvidenceEnabled gate, so a nil detector or an E-dark org is a no-op.
	detector PairDetector
	cfg      Config
	// evidenceResolver reports whether a pending artifact's intent now resolves — the
	// artifact-intent read the E re-pick consults. A real read failure surfaces as an error so
	// the re-pick defers non-counting rather than stranding the row. Defaults to
	// defaultEvidenceResolver.
	evidenceResolver func(ctx context.Context, orgID, artifactID string) (bool, error)
	// outage is the process-local provider-wide-outage signal: a rolling window of
	// provider-side transient failures gates whether a failure consumes the per-record
	// retry budget.
	outage outageDetector
}

// defaultEvidenceResolver is the process-local placeholder for the artifact-intent read: it
// always reports that no artifact resolves (and never errors), so a pending record re-skips
// cheaply on each re-pick rather than running E against unresolved data.
func defaultEvidenceResolver(context.Context, string, string) (bool, error) { return false, nil }

// NewSessionAnalysisProcessor constructs the processor.
func NewSessionAnalysisProcessor(
	rawRecordDAO dao.IRawRecordDAO,
	sessionAnalysisDAO dao.ISessionAnalysisDAO,
	sessionStateDAO dao.ISessionAnalysisStateDAO,
	sessionTurnDAO dao.ISessionTurnAnalysisDAO,
	sessionPhaseDAO dao.ISessionPhaseAnalysisDAO,
	sessionRoundTripFaultDAO dao.ISessionRoundTripFaultDAO,
	securityFlagDAO dao.ISecurityFlagDAO,
	harnessArtifactDAO dao.IHarnessArtifactDAO,
	classificationDAO dao.IHarnessArtifactClassificationDAO,
	s3Client s3.S3Client,
	producer LLMProducer,
	detector PairDetector,
	db *gorm.DB,
	cfg Config,
) *SessionAnalysisProcessor {
	// Single-tenant: register the process database so tenantdb.RunInScope opens a REAL
	// transaction for the multi-statement commit (content + state transition + security-flag
	// reconcile), making it atomic — a mid-commit failure rolls the whole unit back. db is nil
	// for mock-only unit tests, where RunInScope then runs the body directly (mocks need no
	// transaction); an integration/production db makes the commit atomic.
	tenantdb.Register(db)
	processor := &SessionAnalysisProcessor{
		rawRecordDAO:             rawRecordDAO,
		sessionAnalysisDAO:       sessionAnalysisDAO,
		sessionStateDAO:          sessionStateDAO,
		sessionTurnDAO:           sessionTurnDAO,
		sessionPhaseDAO:          sessionPhaseDAO,
		sessionRoundTripFaultDAO: sessionRoundTripFaultDAO,
		securityFlagDAO:          securityFlagDAO,
		securityReconciler:       security.NewReconciler(securityFlagDAO),
		harnessArtifactDAO:       harnessArtifactDAO,
		classificationDAO:        classificationDAO,
		s3Client:                 s3Client,
		producer:                 producer,
		detector:                 detector,
		cfg:                      cfg,
	}
	// The E re-pick's artifact-intent read is the real classification-table read, not the
	// always-false placeholder: a persisted pending artifact id resolves the moment the intent
	// sweep classifies its row. artifactIntentResolved returns (false, nil) for a missing row
	// (ErrNotFound) and an error only on a real DB fault, which defers the re-pick non-counting.
	processor.evidenceResolver = processor.artifactIntentResolved
	return processor
}

// ConnectorID returns the virtual connector this processor handles.
func (p *SessionAnalysisProcessor) ConnectorID() string { return ConnectorID }

// Process is unsupported: like session, the engine uses the flat cross-date path.
func (p *SessionAnalysisProcessor) Process(_ context.Context, _ string, _ time.Time) (int, error) {
	return 0, fmt.Errorf("session_analysis: Process is not supported; use ProcessFlat with an explicit limit")
}

// ProcessFlat scans the actionable set (LIVE then BACKFILL), claims each candidate,
// runs the spine + producers outside any transaction, and commits. It is
// dispatched unscoped, so it opens its own tenantdb scopes (the self-scoping
// contract). limit is the per-tick budget ceiling.
func (p *SessionAnalysisProcessor) ProcessFlat(ctx context.Context, orgID string, limit int) (int, error) {
	if !p.cfg.Enabled(orgID) {
		return 0, nil // dark for this org
	}
	// Plan-context preflight. In this single-tenant build the plan shim always resolves a
	// context (present, unlimited — no seat cap, no data-window cutoff), so this guard is a
	// no-op. It documents the seam a multi-tenant host uses: there ConfigFromContext can be
	// absent (an identity-service outage, or an unenriched ctx), and the host defers the tick
	// rather than run the pipeline with the gates wide open.
	if _, ok := planscoped.ConfigFromContext(ctx); !ok {
		logger.WarnWithCtx(ctx, "session analysis: deferring org tick, plan context unresolved",
			zap.String(logger.OrgID, orgID))
		return 0, nil
	}
	// No cutover floor for an enabled org means the engine processes its full in-window history
	//. Expected for
	// a newly-onboarded org; a spike for an existing org is the un-set-cutover footgun. Emit the
	// visibility signal the map/master-gate coupling (operator discipline) otherwise leaves silent.
	if p.cfg.CutoverDate(orgID).IsZero() {
		noCutoverFloorTicksTotal.WithLabelValues(orgID).Inc()
	}
	if limit <= 0 {
		limit = p.cfg.BatchSize()
	}
	// Clamp the effective limit to min(caller, budget) so a replayed operational job task
	// cannot exceed the per-org, per-tick paid-work ceiling — the scan never fetches
	// more than the budget will let the tick process.
	if budget := p.cfg.GlobalBudgetPerOrgTick(); budget > 0 && limit > budget {
		limit = budget
	}
	params := p.scanParams(ctx, orgID)

	var live, backfill []*dao.ActionableCandidate
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		// LIVE drains fresh uploads first; BACKFILL always yields to it.
		var err error
		if live, err = p.sessionStateDAO.ScanLiveActionable(sctx, params, limit); err != nil {
			return fmt.Errorf("scan live actionable: %w", err)
		}
		if remaining := limit - len(live); remaining > 0 {
			if backfill, err = p.sessionStateDAO.ScanBackfillActionable(sctx, params, remaining); err != nil {
				return fmt.Errorf("scan backfill actionable: %w", err)
			}
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("scanning actionable candidates (org %s): %w", orgID, err)
	}

	// A JUDGED row can match a LIVE leg (content-changed) AND a BACKFILL leg
	// (version-behind / producer-enabled) in one tick; the DISTINCT-ON dedup runs only
	// within one lane, so dedup across lanes here — keeping the LIVE entry, since
	// BACKFILL yields to LIVE. Processing a row twice would Reclaim the just-committed
	// generation and re-run it spine-only, wiping the security flags the first pass
	// wrote.
	candidates := dedupCandidatesKeepLive(live, backfill)

	// Two-sided starvation observation over the fetched set:
	// whether this fetch returned work but no never-analysed row (StarvedRuns) and the oldest
	// pending-data candidate's age. Read off the already-fetched list — no extra query.
	observeScanStarvation(orgID, candidates, time.Now().UTC())

	// The per-org, per-tick paid-work budget is the hard ceiling across both lanes
	// (BatchSize caps each lane; this caps the tick). LIVE is ordered ahead of BACKFILL
	// so the freshest work survives the cut. (The limit clamp above already bounds the
	// scan; this is the belt-and-suspenders cut after dedup.)
	if budget := p.cfg.GlobalBudgetPerOrgTick(); budget > 0 && len(candidates) > budget {
		candidates = candidates[:budget]
	}

	// Process the batch concurrently, bounded by record_concurrency: an
	// analyze_read can take minutes, so a serial loop would let a slow head starve the
	// tail before the batch context expires. Each record carries its own per-record
	// timeout and owns its own tenant scopes — the group holds no
	// enclosing transaction across the slow LLM calls.
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(p.cfg.RecordConcurrency())
	// Bound concurrent detector scoring calls across this batch's records, sized ONCE here from
	// the dynamic DetectorConcurrency knob — a change takes effect on the next batch (matching
	// RecordConcurrency's SetLimit above), so there is no mid-flight resize. nil = unbounded.
	var detectorSem chan struct{}
	if detectorConcurrency := p.cfg.DetectorConcurrency(); detectorConcurrency > 0 {
		detectorSem = make(chan struct{}, detectorConcurrency)
	}
	var processed atomic.Int32
	var failed atomic.Int32
	for _, candidate := range candidates {
		group.Go(func() error {
			recordCtx, cancel := context.WithTimeout(groupCtx, p.cfg.PerRecordTimeout())
			defer cancel()
			if err := p.processCandidate(recordCtx, orgID, params, candidate, detectorSem); err != nil {
				if errors.Is(err, errDetectorUnavailable) || errors.Is(err, errIntentResolverUnavailable) {
					// Deferred (detector or intent-resolver unavailable), already logged + metered
					// upstream: not processed and not a failure — the row stays actionable for a
					// later tick via lease expiry.
					return nil
				}
				logger.WarnWithCtx(groupCtx, "session analysis: candidate processing failed",
					zap.String(logger.OrgID, orgID),
					zap.String("raw_record_id", candidate.RawRecordID),
					zap.String("category", string(candidate.Category)),
					zap.Error(err),
				)
				failed.Add(1)
				return nil // per-record failure is isolated; it never fails the batch
			}
			processed.Add(1)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return int(processed.Load()), fmt.Errorf("processing session-analysis batch: %w", err)
	}

	// Surface the batch's isolated per-record failures at the run grain: each was swallowed to a
	// nil return above (WARN-only, so the batch never fails), so without this a "completed"
	// processor_runs row hides a silently-dropped record.
	if failedCount := failed.Load(); failedCount > 0 {
		recordsFailedTotal.WithLabelValues(orgID).Add(float64(failedCount))
	}
	logger.Info(ctx, "session analysis: batch complete",
		zap.String(logger.OrgID, orgID),
		zap.Int("processed", int(processed.Load())),
		zap.Int("failed", int(failed.Load())),
		zap.Int("candidates", len(candidates)),
	)

	return int(processed.Load()), nil
}

// scanParams resolves the per-org, per-tick scan predicates from config plus the
// plan context (seats, data-window cutoff). The shared PlanScopedEnricher fails
// *open*: an unresolved plan context yields no Config, so the seat leg is left
// inactive (SeatCapEnabled stays false) and the scan admits every person — it does
// not defer. Content reads are raw_record-keyed and writes go through WriteDB, so
// fail-open here spends LLM on non-seated people during a plan blip but does not leak
// a read across seats.
func (p *SessionAnalysisProcessor) scanParams(ctx context.Context, orgID string) dao.SessionAnalysisScanParams {
	now := time.Now().UTC()
	params := dao.SessionAnalysisScanParams{
		OrgID:                   orgID,
		ConnectorID:             SessionConnectorID,
		CutoverDate:             p.cfg.CutoverDate(orgID),
		RetryCap:                p.cfg.RetryCap(),
		ThrottleCutoff:          now.Add(-p.cfg.ReprocessMinInterval()),
		ChildRefoldCutoff:       now.Add(-p.cfg.ChildRefoldMinInterval()),
		LeaseCutoff:             now.Add(-p.cfg.PerRecordTimeout() - leaseMargin),
		VocabularyVersion:       p.cfg.VocabularyVersion(),
		VocabularyEffectiveFrom: p.cfg.VocabularyEffectiveFrom(),
		SpineVersion:            p.cfg.SpineVersion(),
		SpineEffectiveFrom:      p.cfg.SpineEffectiveFrom(),
		ContractVersionRead:     p.cfg.ContractVersionRead(),
		ContractVersionEvidence: p.cfg.ContractVersionEvidence(),
		ContractVersionDiscard:  p.cfg.ContractVersionDiscard(),
		ContractEffectiveFrom:   p.cfg.ContractEffectiveFrom(),
		EnabledProducerBits:     enabledProducerBits(p.cfg, orgID),
		// Provider-wide-outage re-admission: while the rolling provider-side-failure signal
		// is active, a FAILED row whose last attempt predates the outage backoff is
		// re-admitted regardless of attempt_count, so a Gemini outage does not strand a
		// cohort in permanent FAILED. The signal is best-effort
		// (no shared circuit-breaker over the reference-llm call) — a lone per-record failure with
		// no wide-outage signal still exhausts into FAILED at the cap.
		ProviderOutageActive: p.outage.active(now, p.cfg.OutageSignalWindow(), p.cfg.OutageSignalMinRecords()),
		ProviderRetryCutoff:  now.Add(-p.cfg.ProviderRetryBackoff()),
		// Gates the child-reprice re-admit leg: a still-CHILD SKIPPED row whose body content
		// changed is only worth re-sighting once the compat write is live (that is the sole
		// consumer of a child's re-derived cost), so the leg is dormant pre-cutover.
		CompatWriteEnabled: p.cfg.CompatWriteEnabled(),
	}
	// Seat gate: the resolved seat set. HasSeatPersonID / AllowedPersonIDs read the
	// plan context the handler enriches. SeatCapEnabled is the resolved-active/inactive
	// tri-state: a seat-unlimited plan sets it false, so the category-6 leg fails OPEN and
	// re-admits every NO_SEAT-skipped row — the case that matters is an org whose cap was
	// lifted to unlimited, whose prior NO_SEAT skips must re-admit (with the resolved seat
	// set empty, a fail-closed cap would strand them). A resolved-active cap keeps it true.
	//
	// Two knobs stay unset, both fail-open, so neither is a correctness gap — but for
	// different reasons, and one of them cannot be safely turned on in isolation:
	//
	// LiveWatermark stays zero, so the no-state leg is a full anti-join over retained history
	// every tick rather than only rows past a per-org live-upload watermark. Zero is the
	// CORRECT posture, not merely the unoptimised one: the full anti-join never orphans a
	// record. A watermark is a perf optimization (O(history)→O(recent)) that is NOT a clean win
	// on its own, because a no-state record can arrive BELOW an already-advanced watermark and
	// be permanently orphaned — never analysed — two ways: an in-flight consumer commit whose
	// app-stamped updated_at predates the watermark but becomes visible only after the scan
	// advanced it (an MVCC lag), and a backdated/offline-sync upload landing under an old
	// updated_at. That residual gap is closed with a companion
	// low-frequency full-anti-join safety sweep (a slow BACKFILL-lane job) plus a contiguity
	// advance that never passes an unclaimed row on the same axis it filters, and per-org
	// watermark persistence. That sweep is NOT built, so a watermark shipped without it would
	// trade correctness for speed. It stays zero until the sweep + persistence land together.
	//
	// TrackedRepos/RepoFilterEnabled stay unset, so the repo gate (prefetchGate) admits every
	// repo — a widening, never a false skip. Turning it on needs the org's tracked-repo set
	// resolved per tick with the same tri-state discipline as the seat gate (resolved-active
	// fires, resolved-inactive fails open, unresolved defers), which is not wired into this
	// processor's scope; see the fail-open contract on prefetchGate's REPO_UNTRACKED leg.
	if cfg, ok := planscoped.ConfigFromContext(ctx); ok {
		params.Seats = planscoped.AllowedPersonIDs(ctx)
		params.SeatCapEnabled = !cfg.SeatsUnlimited()
		if cutoff, capped := planscoped.DataCutoff(cfg); capped {
			params.Cutoff = cutoff
		}
	}
	return params
}

// leaseMargin pads PerRecordTimeout so a still-running prior-tick worker (a long
// chunked read) is never re-picked before it can plausibly finish.
const leaseMargin = 5 * time.Minute

// processCandidate resolves the pre-fetch skip gates (before any S3 read or LLM
// call), then — if the record passes — claims it, runs the spine and (unless
// spine-only) the producers outside any transaction, and commits. Per-record failure
// is isolated by the caller.
func (p *SessionAnalysisProcessor) processCandidate(ctx context.Context, orgID string, params dao.SessionAnalysisScanParams, candidate *dao.ActionableCandidate, detectorSem chan struct{}) error {
	// The flat handler dispatches on a ctx with no tenant transaction, so the load must
	// open its own scope — an unscoped read runs with no app.current_tenant and the
	// tenant_isolation RLS policy matches zero rows, so every candidate would return
	// not-found under the prod RLS role. GetByIDForDate prunes to the record's partition.
	var record *model.RawRecordModel
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		loaded, loadErr := p.rawRecordDAO.GetByIDForDate(sctx, candidate.RawRecordID, candidate.DataDate)
		if loadErr != nil {
			return fmt.Errorf("get raw record by id+date (%s): %w", candidate.RawRecordID, loadErr)
		}
		record = loaded
		return nil
	}); err != nil {
		return fmt.Errorf("load raw record %s: %w", candidate.RawRecordID, err)
	}
	meta, err := parseMetadata(record.Metadata)
	if err != nil {
		return fmt.Errorf("parse metadata for raw record %s: %w", record.ID, err)
	}

	// The cost-tiered pre-fetch gate runs on every candidate — a re-admitted record
	// re-runs the full gate so it flows OUT_OF_WINDOW → NO_SEAT → processed as
	// conditions change one at a time. A gate hit writes a conditional SKIPPED row and
	// spends no S3 read and no LLM call.
	if reason, gated := p.prefetchGate(ctx, record, meta, params); gated {
		if err := p.applySkip(ctx, orgID, record, meta, reason); err != nil {
			return fmt.Errorf("applying skip (raw_record %s): %w", record.ID, err)
		}
		return nil
	}

	// #6 cheap exit for the steady-state pending re-admission loop: a CategoryPendingData candidate
	// whose pending input is STILL unresolvable on the unchanged body has no new work this tick, yet
	// the full path would re-read S3 and rebuild the spine every scan. Re-decide the re-pick from the
	// persisted state alone (one intent read per waiting artifact, no S3) BEFORE claiming — so no lease
	// is taken and no attempt burned — and leave the JUDGED row actionable for the next scan.
	if candidate.Category == dao.CategoryPendingData && p.pendingDataStillUnresolved(ctx, orgID, record) {
		pendingDataShortCircuitTotal.WithLabelValues(orgID).Inc()
		return nil
	}

	prior, claimToken, err := p.claim(ctx, orgID, candidate, record, meta)
	if err != nil {
		return fmt.Errorf("claiming raw record %s: %w", record.ID, err)
	}
	if claimToken == "" {
		return nil // another tick won the claim
	}

	sp, err := p.spine(ctx, record, meta, prior, claimToken)
	if err != nil {
		p.markFailed(ctx, orgID, record.ID, record.DataDate, claimToken, err)
		return fmt.Errorf("spine for raw record %s: %w", record.ID, err)
	}

	if err := p.transitionSpineDone(ctx, orgID, record, meta.PersonID, claimToken, sp); err != nil {
		if errors.Is(err, errRequeue) {
			return nil // the lease was stolen; the row stays actionable
		}
		return fmt.Errorf("transitioning to spine_done (raw_record %s): %w", record.ID, err)
	}

	read, discard, err := p.runProducers(ctx, orgID, record, meta, prior, sp, detectorSem)
	// A detector-availability or intent-resolver-availability failure is a non-counting deferred
	// retry, NOT a failure: leave the SPINE_DONE row actionable (no commit, no markFailed, no
	// attempt increment) so a later tick re-runs it once the dependency recovers, via lease expiry.
	// Propagated to the batch loop, which counts it as neither processed nor failed (it is already
	// logged + metered upstream).
	if errors.Is(err, errDetectorUnavailable) {
		return errDetectorUnavailable
	}
	if errors.Is(err, errIntentResolverUnavailable) {
		return errIntentResolverUnavailable
	}
	// errProducersSkipped is the spine-only / dark-R signal, not a failure: read is nil
	// and the commit takes the measured-only path. Any other error is a real failure.
	if err != nil && !errors.Is(err, errProducersSkipped) {
		p.markFailed(ctx, orgID, record.ID, record.DataDate, claimToken, err)
		return fmt.Errorf("producers for raw record %s: %w", record.ID, err)
	}

	res, err := p.commit(ctx, orgID, record, meta.PersonID, sp, read, discard)
	if err != nil {
		p.markFailed(ctx, orgID, record.ID, record.DataDate, claimToken, err)
		return fmt.Errorf("commit for raw record %s: %w", record.ID, err)
	}
	// Resolve any already-written child turn rows' parent_prompt_index from this parent's
	// dispatch map. Best-effort and post-commit (decision
	// B): only after the parent's own generation landed, and never failing this record — the
	// child-refold scan leg (children_seq > folded_children_seq) re-runs it on the next re-fold.
	// A re-queued commit (lease stolen, or the row left the claim window) leaves it for that re-fold.
	if res.committed {
		p.backfillChildParentPromptIndex(ctx, orgID, record, sp)
	}
	return nil
}

// dedupCandidatesKeepLive concatenates the LIVE candidates ahead of the BACKFILL
// ones and drops any BACKFILL entry whose raw record already appears in LIVE, so a
// row matching a leg in each lane is processed exactly once, as LIVE (BACKFILL yields
// to LIVE). Order within each lane (freshest-first) is preserved.
func dedupCandidatesKeepLive(live, backfill []*dao.ActionableCandidate) []*dao.ActionableCandidate {
	seen := make(map[string]struct{}, len(live)+len(backfill))
	deduped := make([]*dao.ActionableCandidate, 0, len(live)+len(backfill))
	for _, candidate := range live {
		if _, ok := seen[candidate.RawRecordID]; ok {
			continue
		}
		seen[candidate.RawRecordID] = struct{}{}
		deduped = append(deduped, candidate)
	}
	for _, candidate := range backfill {
		if _, ok := seen[candidate.RawRecordID]; ok {
			continue
		}
		seen[candidate.RawRecordID] = struct{}{}
		deduped = append(deduped, candidate)
	}
	return deduped
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// rawRecordMetadata is the session raw-record metadata the engine reads. On a delegated
// child, RelationType/ParentRecordKey identify the child and point at its parent's
// record_key, and ToolUseID is the parent tool-use id that dispatched this child — the key
// that links the child's measured turn rows back to the parent turn that spawned it
// . The keys mirror the lineage.MetaKey* constants.
type rawRecordMetadata struct {
	PersonID        string `json:"person_id"`
	RepoName        string `json:"repo_name,omitempty"`
	RelationType    string `json:"relation_type,omitempty"`
	ParentRecordKey string `json:"parent_record_key,omitempty"`
	ToolUseID       string `json:"tool_use_id,omitempty"`
}

func parseMetadata(raw json.RawMessage) (*rawRecordMetadata, error) {
	meta := &rawRecordMetadata{}
	if len(raw) == 0 {
		return meta, nil
	}
	if err := json.Unmarshal(raw, meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return meta, nil
}

// rawRecordPayload is the session raw-record payload (the S3 body pointer).
type rawRecordPayload struct {
	S3Key         string `json:"s3_key"`
	ContentSha256 string `json:"content_sha256"`
}

func parsePayload(raw json.RawMessage) (*rawRecordPayload, error) {
	// The payload (the S3 body pointer) is mandatory, unlike the optional metadata; an empty
	// column is a malformed record, so fail with a clear error rather than a cryptic
	// "unexpected end of JSON input" from Unmarshal.
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty raw record payload")
	}
	payload := &rawRecordPayload{}
	if err := json.Unmarshal(raw, payload); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}
	return payload, nil
}

// versionForDate returns the vocabulary version whose effective_from bracket
// contains the record's data_date, so a late upload lands beside its date-peers
// . v1: a single current version gated by its
// effective_from; an older-dated record stamps 0 (the pre-effective_from generation).
func versionForDate(cfg Config, dataDate time.Time) int32 {
	if dataDate.Before(cfg.VocabularyEffectiveFrom()) {
		return 0
	}
	return cfg.VocabularyVersion()
}

// ContentDigest is sha256 of the given bytes, hex-encoded — the spine's stable
// per-body source_content_sha256 when the payload carries no content_sha256. Exported so any caller
// anchors a body's turn rows with the identical digest the live spine would, rather than
// re-implementing it.
func ContentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
