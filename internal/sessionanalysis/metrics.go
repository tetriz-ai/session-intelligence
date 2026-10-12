// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// Measured-turn write status labels. The status label
// follows the metrics-conventions.md contract: "success" / "error".
const (
	turnWriteSuccess = "success"
	turnWriteError   = "error"
)

// Measured-turn grouper Prometheus metrics. All carry
// the session_intelligence_ metric prefix so every measured-turn series sorts
// and alerts under one name family, mirroring the merge-path deriver metrics. Labelled
// by org (and write status where it partitions the outcome); per-turn dimensions
// (prompt_index, files touched) belong in analytics events, not here.
//
// The counters increment INLINE at the grouper's write site inside the commit
// transaction, not on transaction commit — a commit that writes the measured turns
// but rolls back on a later CAS step over-counts "written" by that batch. This
// matches the merge-path convention (inline, non-transactional) and is acceptable for
// a rate/ratio signal: turnWriteSuccess records that the measured write path itself
// succeeded, which is what the failure-ratio alert watches, independent of whether the
// enclosing judged transition later re-queued.
var (
	// measuredTurnCommitsTotal counts each measured-turn write attempt by outcome, so
	// the failure ratio (error / (success+error)) is computable from one series. One
	// increment per grouper write that had turns to write (a spine-only reprocess with
	// no re-parsed prompts writes nothing and is not counted).
	measuredTurnCommitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_measured_turn_commits_total",
		Help: "Measured session-turn write attempts by outcome (success|error).",
	}, []string{"org_id", "status"})

	// measuredTurnsWrittenTotal counts measured turn rows upserted — the write-volume
	// signal the coverage query and dashboard read (a per-attempt count is
	// measuredTurnCommitsTotal's job; this is the row throughput).
	measuredTurnsWrittenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_measured_turns_written_total",
		Help: "Measured session-turn rows written by the grouper.",
	}, []string{"org_id"})

	// scanStarvedRunsTotal counts ticks whose scan returned candidates but zero
	// never-analysed (NO_STATE) ones — the session-proven new-work-starvation signal the
	// NO_STATE-first sort defends against. A sustained
	// climb means fresh uploads are being crowded out; per-org so a hot org is isolable.
	scanStarvedRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_scan_starved_runs_total",
		Help: "Scan ticks that returned rows but zero never-analysed (NO_STATE) candidates.",
	}, []string{"org_id"})

	// pendingDataOldestAgeSeconds is the age of the oldest pending-data re-pick candidate a
	// tick saw — the pending-side aging signal that complements StarvedRuns. It is set to now − the candidate's conversation date as a cheap proxy, and
	// reset to 0 on any tick with no pending candidate so a drained backlog does not freeze it.
	pendingDataOldestAgeSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "session_intelligence_pending_data_oldest_age_seconds",
		Help: "Age of the oldest pending-data re-pick candidate observed this tick (0 when none).",
	}, []string{"org_id"})

	// repickDecisionsTotal counts each per-producer re-pick decision by outcome, so the re-pick mix
	// is observable per org: resync abandons the stale snapshot and re-runs in full; resolved carries
	// R and re-runs the now-resolvable half; reskip keeps E pending (no E cost) — but D, or a
	// contract-bumped R, may still run on that same pass, so the label names the E decision, not the
	// pass's total LLM cost.
	repickDecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_repick_decisions_total",
		Help: "Per-producer re-pick decisions by outcome (resync|reskip|resolved).",
	}, []string{"org_id", "decision"})

	// pendingDataShortCircuitTotal counts CategoryPendingData candidates the processor re-skipped
	// from the persisted state alone — no S3 body, no spine — because the pending input is still
	// unresolvable on the unchanged body and no other producer needs the spine. It measures the
	// spine/S3 reads this pending-data short-circuit avoids on the steady-state re-admission loop.
	pendingDataShortCircuitTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_pending_data_short_circuit_total",
		Help: "Pending-data candidates re-skipped before the spine (no S3, no spine).",
	}, []string{"org_id"})

	// producerFailedTotal counts E/D producer failures that preserved R (the record committed a
	// valid verdict and only the failed producer is retried), by producer. It measures how often
	// the per-producer failure isolation saves R from a wholesale JUDGE_FAILED.
	producerFailedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_producer_failed_total",
		Help: "E/D producer failures that preserved R and retry only the failed producer, by producer.",
	}, []string{"org_id", "producer"})

	// recordsFailedTotal counts candidates whose processing failed and was isolated to a nil
	// return in the batch loop (an isolated failure never fails the batch). It is the run-grain
	// signal a "completed" processor_runs row otherwise hides — distinct from producerFailedTotal,
	// which counts an E/D producer failure that PRESERVED R; this counts a record that produced no
	// committed judged result this tick.
	recordsFailedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_records_failed_total",
		Help: "Per-record processing failures isolated in the batch loop (record produced no committed result this tick).",
	}, []string{"org_id"})

	// markFailedWriteErrorsTotal counts markFailed passes whose terminal FAILED transition could not
	// be persisted (its own detached-ctx write errored — e.g. a mid-deploy teardown cutting the DB
	// write). A non-zero rate means a record was left non-terminal and will re-pick until a later
	// pass lands the transition; it is the only signal on that otherwise warn-only swallow path.
	markFailedWriteErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_mark_failed_write_errors_total",
		Help: "markFailed passes whose terminal FAILED transition failed to persist (record left non-terminal, will re-pick).",
	}, []string{"org_id"})

	// bodyStaleAtCommitTotal counts judged commits where the body was re-uploaded while the
	// producers ran, so the verdict was for a superseded body: the commit lands JUDGED (durable) but
	// skips the stale security reconcile, and re-analysis of the current body rides the normal
	// CONTENT_CHANGED reprocess throttle. A non-zero rate is the
	// observability signal for the mid-flight-rewrite freshness edge.
	bodyStaleAtCommitTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_body_stale_at_commit_total",
		Help: "Judged commits whose body changed mid-flight (stale verdict served; security reconcile skipped; re-analysis on the reprocess throttle).",
	}, []string{"org_id"})

	// noCutoverFloorTicksTotal counts ticks where an ENABLED org has NO SessionAnalysisCutoverDates
	// entry, so the hard write floor is absent and the engine processes the org's full in-window
	// history (bounded only by OUT_OF_WINDOW/NO_SEAT). Expected and harmless for an org onboarded
	// after session is off (its whole history is small); a spike for an EXISTING org is the
	// operator-footgun signal (enabled without first populating its cutover — a large, unbudgeted
	// Gemini reprocess). The map/master-gate coupling is operator discipline; this metric makes the omission visible rather than silent.
	noCutoverFloorTicksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_no_cutover_floor_ticks_total",
		Help: "Ticks where an enabled org has no cutover floor set, so the engine processes its full in-window history (watch for an existing-org spike — an un-set-cutover footgun).",
	}, []string{"org_id"})
)

// Re-pick decision labels for repickDecisionsTotal.
const (
	repickDecisionResync   = "resync"
	repickDecisionReskip   = "reskip"
	repickDecisionResolved = "resolved"
)

var (
	// sessionEvidenceIntentDeferredTotal counts records E deferred WITHOUT counting because the
	// artifact intent-classification read failed on a real DB fault — the intent-read peer of
	// roundTripDetectorDeferredTotal. A sustained non-zero rate means the classification store is
	// unavailable and armed orgs' E records are waiting (re-admitted via lease expiry), not failing.
	sessionEvidenceIntentDeferredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_evidence_intent_deferred_total",
		Help: "E deferred without counting because the artifact intent-classification read failed.",
	}, []string{"org_id"})

	// sessionEvidenceArtifactListDeferredTotal counts records E deferred WITHOUT counting because
	// the harness_artifacts inventory read (the VCS/DESKTOP live-set fetch) failed on a real DB
	// fault — a distinct failure domain from the intent-classification read. A sustained non-zero
	// rate means the artifact inventory store is unavailable and armed orgs' E records are waiting
	// (re-admitted via lease expiry), not failing.
	sessionEvidenceArtifactListDeferredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_evidence_artifact_list_deferred_total",
		Help: "E deferred without counting because the harness_artifacts inventory read failed.",
	}, []string{"org_id"})

	// sessionEvidenceArtifactUnresolvedTotal counts session invocations whose VCS+DESKTOP
	// resolution returned NO live harness_artifacts row — E proceeds with that intent unknown
	// rather than stranding. A sustained non-zero rate flags a systemic slug-match or inventory
	// defect, which would otherwise be an invisible silent bypass.
	sessionEvidenceArtifactUnresolvedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_evidence_artifact_unresolved_total",
		Help: "Session invocations that resolved to NO live harness_artifacts row (E proceeds intent-unknown).",
	}, []string{"org_id"})
)

// observeScanStarvation records the two-sided starvation signals from one tick's scanned
// candidates: scanStarvedRunsTotal increments when the
// tick returned candidates but none never-analysed (new-work starvation), and
// pendingDataOldestAgeSeconds is set to the oldest pending-data candidate's age (the
// pending-side aging signal). It reads only the already-fetched candidate list, so it adds no
// query. An idle tick counts no starvation but still resets the pending-age gauge to 0.
func observeScanStarvation(orgID string, candidates []*dao.ActionableCandidate, now time.Time) {
	var sawNoState bool
	var oldestPending time.Time
	for _, candidate := range candidates {
		if candidate.Category == dao.CategoryNoState {
			sawNoState = true
		}
		if candidate.Category == dao.CategoryPendingData {
			if oldestPending.IsZero() || candidate.DataDate.Before(oldestPending) {
				oldestPending = candidate.DataDate
			}
		}
	}
	// Reset the pending-age gauge to 0 whenever no pending candidate is present — including a
	// fully-idle tick — so a drained backlog does not leave it frozen at its last (large) value.
	if oldestPending.IsZero() {
		pendingDataOldestAgeSeconds.WithLabelValues(orgID).Set(0)
	} else {
		pendingDataOldestAgeSeconds.WithLabelValues(orgID).Set(now.Sub(oldestPending).Seconds())
	}
	// StarvedRuns is a NON-EMPTY fetch that returned zero never-analysed rows; an idle tick is
	// not starvation.
	if len(candidates) > 0 && !sawNoState {
		scanStarvedRunsTotal.WithLabelValues(orgID).Inc()
	}
}

// Round-trip PAIRS detector Prometheus metrics. The
// enumerate → guard → score → top-N/ceiling pipeline is dark until EvidenceEnabled (the E
// gate the detector rides), so these stay at zero for an un-armed org. Labelled by org (and drop reason where it partitions the
// rejection); per-pair dimensions belong in analytics events, not here.
const (
	// The five guard rejections, one label value each.
	dropReasonNoAgentResponse = "no_agent_response" // no non-empty assistant turn between P and Q
	dropReasonQIdenticalToP   = "q_identical_to_p"  // Q byte-identical to P
	dropReasonPEmpty          = "p_empty"           // P empty after trim
	dropReasonQEmpty          = "q_empty"           // Q empty after trim
	dropReasonDuplicatePair   = "duplicate_pair"    // (P,Q) already seen this session
)

var (
	// roundTripPairsDroppedTotal counts adjacent-human pairs the validity guard rejected, by
	// reason — the guard-drop-rate signal (a high rate on one reason is an enumeration or a
	// parsing regression). One increment per dropped pair.
	roundTripPairsDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_round_trip_pairs_dropped_total",
		Help: "Adjacent-human pairs rejected by the round-trip validity guard, by reason.",
	}, []string{"org_id", "reason"})

	// roundTripPairsScoredTotal counts guard-valid pairs sent to the detector for scoring —
	// the paid-work volume the detector concurrency/ceiling are sized against.
	roundTripPairsScoredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_round_trip_pairs_scored_total",
		Help: "Guard-valid pairs sent to the round-trip detector for scoring.",
	}, []string{"org_id"})

	// roundTripSessionsTruncatedTotal counts sessions whose guard-valid pair count exceeded the
	// hard ceiling and were scored only up to it (flagged detector-truncated) — the legible
	// degrade the same as D's overflow. One increment per truncated session.
	roundTripSessionsTruncatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_round_trip_sessions_truncated_total",
		Help: "Sessions whose guard-valid pairs exceeded the ceiling and were scored only up to it.",
	}, []string{"org_id"})

	// roundTripDetectorDeferredTotal counts records deferred (non-counting retry) because the
	// detector service was unavailable (503/timeout/5xx) — a sustained non-zero rate means the
	// detector is down/warming for an armed org and its records are waiting, not failing.
	roundTripDetectorDeferredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_round_trip_detector_deferred_total",
		Help: "Records deferred without counting because the round-trip detector was unavailable.",
	}, []string{"org_id"})

	// roundTripFaultsRejectedTotal counts E round-trip faults dropped at commit because their
	// prompt_index was not one of the detector-supplied candidate pairs — a hallucinated or
	// out-of-set index the fault table (which has no turn FK) would otherwise persist as a durable
	// fault against a pair E was never offered. A sustained non-zero rate means E is emitting
	// faults outside its supplied pair set.
	roundTripFaultsRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "session_intelligence_round_trip_faults_rejected_total",
		Help: "E round-trip faults dropped at commit because their prompt_index was not a supplied detector candidate.",
	}, []string{"org_id"})
)

// RegisterMetrics registers the batch pipeline's metrics with reg. They are created
// unregistered — see analyze.RegisterMetrics for the public wire-once contract.
func RegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		measuredTurnCommitsTotal,
		measuredTurnsWrittenTotal,
		scanStarvedRunsTotal,
		pendingDataOldestAgeSeconds,
		repickDecisionsTotal,
		pendingDataShortCircuitTotal,
		producerFailedTotal,
		recordsFailedTotal,
		markFailedWriteErrorsTotal,
		bodyStaleAtCommitTotal,
		noCutoverFloorTicksTotal,
		sessionEvidenceIntentDeferredTotal,
		sessionEvidenceArtifactListDeferredTotal,
		sessionEvidenceArtifactUnresolvedTotal,
		roundTripPairsDroppedTotal,
		roundTripPairsScoredTotal,
		roundTripSessionsTruncatedTotal,
		roundTripDetectorDeferredTotal,
		roundTripFaultsRejectedTotal,
	)
}
