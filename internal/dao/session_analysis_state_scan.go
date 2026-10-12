// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/lineage"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// The session-analysis actionable scan, claim, and CAS state-machine transitions
//. All run through tenantdb.DB: the
// state table carries tenant_isolation only, and the scan must see out-of-window /
// no-seat rows the content tables hide, so it is a full-history lifecycle read.

// ActionableCategory names one of the actionable-scan re-admission legs.
// A JUDGED row can satisfy several at once (content-changed, version-behind,
// producer-enabled); the scan assigns exactly one per row by the priority order
// below so a candidate is never double-listed.
type ActionableCategory string

// Actionable categories for a candidate record.
const (
	CategoryNoState          ActionableCategory = "NO_STATE"          // 1: never analysed
	CategoryLeaseExpired     ActionableCategory = "LEASE_EXPIRED"     // 5: non-terminal, lease expired
	CategoryRetryableFailure ActionableCategory = "RETRYABLE_FAILURE" // 2: FAILED under retry cap
	CategoryContentChanged   ActionableCategory = "CONTENT_CHANGED"   // 3: body sha differs, throttle elapsed
	CategorySeatGranted      ActionableCategory = "SEAT_GRANTED"      // 6: NO_SEAT re-admit
	CategoryVersionBehind    ActionableCategory = "VERSION_BEHIND"    // 4: a version stamp behind current
	CategoryWindowWidened    ActionableCategory = "WINDOW_WIDENED"    // 7: OUT_OF_WINDOW re-admit
	CategoryRepoConnected    ActionableCategory = "REPO_CONNECTED"    // 8: REPO_UNTRACKED_* re-admit
	CategoryProducerEnabled  ActionableCategory = "PRODUCER_ENABLED"  // 9: flag-skipped producer now on
	CategoryChildRefold      ActionableCategory = "CHILD_REFOLD"      // 10: a delegated child arrived after the last fold
	CategoryLineageCorrected ActionableCategory = "LINEAGE_CORRECTED" // 11: SKIPPED(CHILD) re-uploaded as a non-child
	CategoryChildReprice     ActionableCategory = "CHILD_REPRICE"     // 12: SKIPPED(CHILD) re-uploaded with changed content, still a child
	CategoryOwnershipChanged ActionableCategory = "OWNERSHIP_CHANGED" // 13: JUDGED/SPINE_DONE row whose metadata person_id was corrected
	CategoryPendingData      ActionableCategory = "PENDING_DATA"      // 14: JUDGED/SPINE_DONE row with a producer SKIPPED_PENDING_DATA (its input not yet resolvable)
	CategoryProducerFailed   ActionableCategory = "PRODUCER_FAILED"   // 15: JUDGED/SPINE_DONE row with a producer CALL_STATUS_FAILED, under the retry cap — retries only that producer, carrying R
)

// ActionableCandidate is one raw record the scan admits, tagged with the leg that
// admitted it. The processor claims it, then loads the full raw record.
type ActionableCandidate struct {
	RawRecordID string             `gorm:"column:raw_record_id"`
	DataDate    time.Time          `gorm:"column:data_date"`
	Category    ActionableCategory `gorm:"column:category"`
}

// SessionAnalysisScanParams carries the per-org, per-tick config the scan binds as
// predicates. The tri-state membership gates are resolved upstream: an
// *_Enabled=false gate fails open (serves every row), a resolved-active gate fires
// on non-membership, and an unresolved gate defers (the caller omits the org's tick
// entirely, not represented here).
type SessionAnalysisScanParams struct {
	OrgID       string // the tenant; carried as an explicit predicate, not RLS-only
	ConnectorID string // "session" — the body records the engine reads

	// Category-1 (no-state) LIVE watermark: only rows whose updated_at exceeds it, so a
	// steady-state tick with no new uploads would be an empty index range rather than an
	// O(history) anti-join. Zero time admits ALL no-state rows —
	// the full anti-join, which is the correct (never-orphaning) posture. A non-zero watermark
	// is a perf optimization that is unsafe in isolation: a record arriving below an
	// already-advanced watermark (an MVCC-lagged commit, or a backdated upload) would be
	// permanently orphaned, so it may only be advanced alongside its companion full-
	// anti-join safety sweep. Its threading currently leaves it zero (see scanParams).
	LiveWatermark time.Time

	// CutoverDate is the per-org hard write floor: the
	// LIVE-lane no-state leg admits only rr.data_date >= cutover, and there is no
	// pre-cutover no-state leg — records below the cutover are never LLM-processed (they
	// are carried onto the new tables by a backfill job). A zero cutover is the
	// no-floor default: the bound becomes rr.data_date >= 0001-01-01 (every in-window row,
	// unchanged) and an org with no configured cutover processes its full in-window history.
	CutoverDate time.Time

	// Category-2 retry cap and the provider-outage override.
	RetryCap             int32
	ProviderOutageActive bool      // re-admit FAILED regardless of attempt_count
	ProviderRetryCutoff  time.Time // FAILED rows last attempted before this re-admit under an active outage

	// Category-3 content-change reprocess throttle: only re-admit a changed body if
	// the last success is older than this (now - reprocess_min_interval).
	ThrottleCutoff time.Time

	// Category-5 lease: a non-terminal row last attempted before this is dead/stuck
	// (now - per_record_timeout - margin).
	LeaseCutoff time.Time

	// Category-10 child-refold throttle: re-admit a JUDGED parent whose children_seq
	// advanced past folded_children_seq (a delegated child arrived after the last fold)
	// only if its last success is older than this (now - child_refold_min_interval), so a
	// continuously-delegating session re-folds at most once per interval rather than every
	// tick.
	ChildRefoldCutoff time.Time

	// Category-6 seat gate.
	Seats          []string
	SeatCapEnabled bool

	// Category-7 window gate: the current data_window cutoff date.
	Cutoff time.Time

	// Category-8 repo gate.
	TrackedRepos      []string
	RepoFilterEnabled bool

	// Category-4 version-behind: the current version scalars and their effective_from
	// dates (a row is behind only for data_date >= the version's effective_from).
	VocabularyVersion       int32
	VocabularyEffectiveFrom time.Time
	SpineVersion            int32
	SpineEffectiveFrom      time.Time
	ContractVersionRead     int32
	ContractVersionEvidence int32
	ContractVersionDiscard  int32
	ContractEffectiveFrom   time.Time

	// Category-9 producer-enabled re-admit mask: the bitmask of producers ENABLED for this
	// org right now (R always on; E/D per their gate flags). The scan re-admits a JUDGED/
	// SPINE_DONE row only when a producer it flag-skipped is now enabled — (flag_skipped_producers
	// & EnabledProducerBits) <> 0. Without the mask, a permanently-dark producer's bit stays set
	// forever and the row churns re-admission every scan with no work to do.
	EnabledProducerBits int32

	// Category-12 child-reprice gate. Only when the phase-5 compat write is on (Session retired,
	// so the engine owns child pricing) does the scan re-admit a SKIPPED(CHILD) row whose body
	// changed but which is still a child — to re-run StampChildCost and re-fold the parent. During
	// the parallel run Session re-prices changed children, so the leg is inert.
	CompatWriteEnabled bool
}

// enum name shorthands (the proto enum NAME is the stored text).
func statusName(s sessionintelligencepb.SessionAnalysisStatus) string { return s.String() }

func skipName(s sessionintelligencepb.SessionAnalysisSkipReason) string {
	return s.String()
}

var (
	stPending    = statusName(sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING)
	stSpineDone  = statusName(sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SPINE_DONE)
	stJudged     = statusName(sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED)
	stFailed     = statusName(sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED)
	stSkipped    = statusName(sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED)
	stSuperseded = statusName(sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SUPERSEDED)

	srNoSeat      = skipName(sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT)
	srOutOfWindow = skipName(sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW)
	srRepoMeta    = skipName(sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_METADATA)
	srRepoProj    = skipName(sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_PROJECT)
	srChild       = skipName(sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD)

	// originSessionCopy is the stored origin text of a copy-migrated row
	// (session-analyses-serving-parity design). The JUDGED re-admission legs — content-changed,
	// version-behind, and ownership-changed — exclude it so a later raw-record re-upload, a global
	// version bump, or a person-id correction never re-processes frozen Session history. This is the
	// durable, origin-aware terminality guard: only a backfill job own re-run refreshes a
	// copied row; the live engine never reclaims one.
	originSessionCopy = sessionintelligencepb.SessionAnalysisOrigin_SESSION_ANALYSIS_ORIGIN_COPY.String()
)

// scanSubquery is one category leg: its predicate over the (rr LEFT JOIN st) join,
// a literal category label, and a literal priority (lowest wins the DISTINCT ON).
type scanSubquery struct {
	priority  int
	category  ActionableCategory
	predicate string
	args      []any
}

// ScanLiveActionable returns the LIVE-lane actionable candidates — categories 1
// (no-state), 2 (retryable failure), 3 (content changed), 5 (lease-expired), and 6
// (seat granted) — freshest first (data_date DESC). The LIVE lane drains fresh
// uploads and the narrow config-delta re-admissions every tick.
func (r *SessionAnalysisStateRepo) ScanLiveActionable(ctx context.Context, params SessionAnalysisScanParams, limit int) ([]*ActionableCandidate, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "ScanLiveActionable", time.Now())
	subs := []scanSubquery{
		{
			priority: 1, category: CategoryNoState,
			// LIVE takes live uploads only (data_date >= cutover). With a zero cutover
			// the bound is >= 0001-01-01 (every no-state row), preserving P1 behaviour.
			//
			// This leg deliberately admits a delegated child too, unfiltered by relation_type: a
			// child arrives with no cost_usd_micros (only a processor stamps it), so this is its
			// ONLY path to applySkip's CHILD branch, which both prices it (StampChildCost) and
			// bumps the parent's child fence (bumpParentFence). Excluding children here would
			// orphan every future post-cutover child's pricing once Session retires
			//. Pre-cutover children are carried by the
			// a backfill job (their cost folds into the copied parent
			// insight), never LLM-processed by the engine — the cutover hard floor.
			predicate: "st.id IS NULL AND rr.updated_at > ? AND rr.data_date >= ?", args: []any{params.LiveWatermark, params.CutoverDate},
		},
		{
			priority: 2, category: CategoryLeaseExpired,
			predicate: "st.status IN (?,?) AND (st.last_attempted_at IS NULL OR st.last_attempted_at < ?)",
			args:      []any{stPending, stSpineDone, params.LeaseCutoff},
		},
		{
			priority: 3, category: CategoryRetryableFailure,
			predicate: retryablePredicate(params), args: retryableArgs(params),
		},
		{
			priority: 4, category: CategoryContentChanged,
			// Two change signals, split by whether the session payload carries a
			// content_sha256. A hash-bearing row uses the cheap content hash; a legacy row
			// (payload sha absent) has no cheap body digest to compare, so it falls back to
			// raw_records.updated_at against the spine-stamped source_updated_at — a re-upload
			// bumps updated_at, an unchanged body leaves it stable, and the compat
			// processing_status write deliberately never bumps it.
			// Keying each branch on the payload sha's presence stops a legacy row (absent payload
			// sha) from matching the hash branch every tick against its non-null spine digest.
			// NULLIF folds an empty-string sha into the legacy branch, matching the commit
			// re-read's COALESCE(...,'') so the scan and the commit classify a row identically.
			//
			// FAILED is admitted alongside JUDGED: a corrected re-upload of a failed session must
			// reprocess (attempt_count is reset on a content-change re-admit — the reset only
			// matters for a FAILED row), else a body that failed to the retry cap is stranded forever
			// on the next upload. The per-branch `(status = JUDGED OR anchor IS NOT NULL)` guard is
			// what makes that churn-safe: a FAILED row that failed BEFORE the spine stamped its anchor
			// carries a NULL baseline, so `payload_sha IS DISTINCT FROM NULL` would otherwise match
			// every tick (and cat-3's attempt_count reset would restart the failing spine forever). The
			// guard excludes those. It is deliberately RELAXED for JUDGED so a legacy JUDGED row with
			// no stored baseline still re-admits once to establish it (the migration-artifact case the
			// legacy branch exists for) — a JUDGED row already ran the spine, so it never churns.
			// A copy-migrated row (origin = SESSION_COPY) is frozen history and is excluded here:
			// its st.source_content_sha256 / st.source_updated_at match the raw record at copy time,
			// but a LATER re-upload would otherwise re-admit and re-judge it, overwriting Session's
			// carried output and spending LLM. IS DISTINCT FROM (not <>) because a live row carries a
			// NULL origin — `<> 'SESSION_COPY'` is NULL for those and would silently exclude EVERY
			// live row from content-change re-admission (session-analyses-serving-parity design).
			predicate: "st.status IN (?,?) AND (" +
				"(NULLIF(rr.payload->>'content_sha256', '') IS NOT NULL AND NULLIF(rr.payload->>'content_sha256', '') IS DISTINCT FROM st.source_content_sha256 AND (st.status = ? OR st.source_content_sha256 IS NOT NULL)) OR " +
				"(NULLIF(rr.payload->>'content_sha256', '') IS NULL AND rr.updated_at IS DISTINCT FROM st.source_updated_at AND (st.status = ? OR st.source_updated_at IS NOT NULL))" +
				") AND (st.last_succeeded_at IS NULL OR st.last_succeeded_at < ?) AND st.origin IS DISTINCT FROM ?",
			args: []any{stJudged, stFailed, stJudged, stJudged, params.ThrottleCutoff, originSessionCopy},
		},
		{
			// Re-admit a NO_SEAT skip either when the seat set now includes the state's person
			// (a seat grant, the index-seekable common case) OR when an unchanged-payload re-upload
			// CORRECTED the raw record's metadata person_id to a now-seated identity — the state's
			// person_id is stale (set once at skip) and no leg refreshes it, so keying only on it
			// would strand a corrected row forever. prefetchGate re-checks the CURRENT meta.PersonID
			// on claim, so a genuinely-seated corrected row proceeds and a still-unseated one
			// re-skips (MarkSkipped refreshes state.person_id from current meta), which then also
			// fails the metadata arm — no churn. Mirrors the cat-11 metadata-correction re-admit.
			priority: 5, category: CategorySeatGranted,
			predicate: "st.status = ? AND st.skip_reason = ? AND (st.person_id IN (?) OR rr.metadata->>'person_id' IN (?) OR ? = FALSE)",
			args:      []any{stSkipped, srNoSeat, seatList(params.Seats), seatList(params.Seats), params.SeatCapEnabled},
		},
		{
			// A JUDGED row whose metadata person_id was CORRECTED by an unchanged-payload re-upload
			// (an identity-resolution fix) must be re-attributed — cat-3 does not fire (content
			// unchanged), and serving reads the content row's person_id, so the row stays attributed
			// to the old user (wrong seat/team/user scope). Gated on `rr.updated_at > st.updated_at`
			// so it considers ONLY rows re-uploaded since their last processing (a re-upload is the
			// only way metadata changes) — a near-empty seek in steady state — then checks person
			// drift. On re-admit the claim does NOT clear the served verdict or reset attempts (this
			// is a re-attribution, not a content change); the spine-only re-run re-attributes both
			// the content row (person_id is in the measured column set) and the state row
			// (TransitionToJudged refreshes person_id), so the drift — and the leg — clear. NULLIF
			// folds an empty metadata person into a no-match, matching cat-5's handling. A copy-migrated
			// row (origin = SESSION_COPY) is excluded here too: it is frozen history the engine never
			// re-processes, so an unchanged-payload re-upload with a corrected person_id must not spine-
			// only re-run it (session-analyses-serving-parity design).
			priority: 6, category: CategoryOwnershipChanged,
			predicate: "st.status = ? AND rr.updated_at > st.updated_at AND st.person_id::text IS DISTINCT FROM NULLIF(rr.metadata->>'person_id', '') AND st.origin IS DISTINCT FROM ?",
			args:      []any{stJudged, originSessionCopy},
		},
	}
	candidates, err := r.runActionableScan(ctx, params.OrgID, params.ConnectorID, subs, limit)
	if err != nil {
		return nil, fmt.Errorf("scanning live-lane actionable (org %s): %w", params.OrgID, err)
	}
	return candidates, nil
}

// ScanBackfillActionable returns the re-admission-lane candidates — categories 4
// (version behind), 7 (window widened), 8 (repo connected), and 9 (producer
// enabled) — freshest first. This lane drains the bounded one-off bursts a version
// bump or a whole-gate inactivation surfaces, and always yields to LIVE. It carries
// NO historical no-state leg: pre-cutover sessions are never LLM-processed (the
// CutoverDate hard floor) — they are carried onto the
// new tables by a backfill job instead.
func (r *SessionAnalysisStateRepo) ScanBackfillActionable(ctx context.Context, params SessionAnalysisScanParams, limit int) ([]*ActionableCandidate, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "ScanBackfillActionable", time.Now())
	subs := []scanSubquery{
		{
			priority: 1, category: CategoryVersionBehind,
			// A copy-migrated row (origin = SESSION_COPY) is excluded: it is stamped with the
			// CURRENT versions at copy time, but a FUTURE version bump would otherwise re-admit and
			// re-judge frozen Session history. IS DISTINCT FROM (not <>) so a NULL-origin live row is
			// still re-admitted on a real version bump (session-analyses-serving-parity design).
			predicate: "st.status = ? AND (" +
				"(st.vocabulary_version < ? AND rr.data_date >= ?) OR " +
				"(st.spine_version < ? AND rr.data_date >= ?) OR " +
				"(st.contract_version_read < ? AND rr.data_date >= ?) OR " +
				"(st.contract_version_evidence < ? AND rr.data_date >= ?) OR " +
				"(st.contract_version_discard < ? AND rr.data_date >= ?)) AND st.origin IS DISTINCT FROM ?",
			args: []any{
				stJudged,
				params.VocabularyVersion, params.VocabularyEffectiveFrom,
				params.SpineVersion, params.SpineEffectiveFrom,
				params.ContractVersionRead, params.ContractEffectiveFrom,
				params.ContractVersionEvidence, params.ContractEffectiveFrom,
				params.ContractVersionDiscard, params.ContractEffectiveFrom,
				originSessionCopy,
			},
		},
		{
			priority: 2, category: CategoryWindowWidened,
			// UTC-anchor the date cast to match the data_window RLS cutoff, so the re-admit
			// boundary does not shift a day if the DB session TZ is ever not UTC.
			predicate: "st.status = ? AND st.skip_reason = ? AND (st.conversation_started_at AT TIME ZONE 'UTC')::date >= ?::date",
			args:      []any{stSkipped, srOutOfWindow, params.Cutoff},
		},
		{
			priority: 3, category: CategoryRepoConnected,
			predicate: "st.status = ? AND st.skip_reason IN (?,?) AND (st.repo_name IN (?) OR ? = FALSE)",
			args:      []any{stSkipped, srRepoMeta, srRepoProj, seatList(params.TrackedRepos), params.RepoFilterEnabled},
		},
		{
			// flag_skipped_producers <> 0 kept as an explicit conjunct so the partial index
			// (session_analysis_state_producer_enabled) predicate matches literally; the bitmask
			// AND then refines to only rows whose skipped producer is now enabled — a permanently
			// dark producer's bit is masked out, so it never churns re-admission.
			priority: 4, category: CategoryProducerEnabled,
			predicate: "st.status IN (?,?) AND st.flag_skipped_producers <> 0 AND (st.flag_skipped_producers & ?) <> 0 AND st.origin IS DISTINCT FROM ?",
			args:      []any{stJudged, stSpineDone, params.EnabledProducerBits, originSessionCopy},
		},
		{
			// A delegated child arrived after this parent's last fold (children_seq advanced
			// past folded_children_seq), throttled to at most once per child_refold_min_interval
			// so a continuously-delegating session is not re-folded every tick. The parent's
			// body is unchanged, so the reclaim takes the spine-only zero-LLM path and re-folds
			// only the measured child cost.
			priority: 5, category: CategoryChildRefold,
			// A copied row is frozen (session-analyses-serving-parity design): the origin guard keeps it
			// out of child-refold, and BumpChildrenSeq never advances a copied parent's fence, so the
			// children_seq > folded_children_seq predicate can never fire for it in the first place.
			predicate: "st.status = ? AND st.children_seq > st.folded_children_seq AND (st.last_succeeded_at IS NULL OR st.last_succeeded_at < ?) AND st.origin IS DISTINCT FROM ?",
			args:      []any{stJudged, params.ChildRefoldCutoff, originSessionCopy},
		},
		{
			// A SKIPPED(CHILD) row whose re-upload corrected its lineage away from a child type
			// (relation_type now PRIMARY/empty) is a real top-level session that must be analysed.
			// No other leg admits a SKIPPED(CHILD) row — cat-3 (content-changed) is gated to
			// JUDGED/FAILED and the seat/window/repo re-admits key on their own skip_reasons — so
			// without this leg the corrected session stays skipped forever once the old Session
			// pipeline (which still processes it during the parallel run) retires. The child-type
			// set mirrors lineage.IsChild; a still-child row (relation_type unchanged) fails the
			// NOT IN and never churns, and prefetchGate re-runs the full gate on claim so a row
			// that is somehow still a child cheaply re-skips.
			priority: 6, category: CategoryLineageCorrected,
			predicate: "st.status = ? AND st.skip_reason = ? AND COALESCE(rr.metadata->>?, '') NOT IN (?,?,?)",
			args: []any{
				stSkipped, srChild, lineage.MetaKeyRelationType,
				lineage.RelationTypeSubagent, lineage.RelationTypePlugin, lineage.RelationTypeHook,
			},
		},
		{
			// A JUDGED/SPINE_DONE row whose producer went SKIPPED_PENDING_DATA — its input
			// dependency was not yet resolvable when it committed. SKIPPED_PENDING_DATA lives in
			// call_state, which the scan never SQL-filters, so the flat pending_data_producers
			// bitmask is the searchable counterpart: re-pick the row until the data resolves or the
			// body changes. The explicit `<> 0` conjunct is kept so the partial index
			// (session_analysis_state_pending_data) predicate matches literally. Ranked below
			// NO_STATE, peer to the other re-admissions — see runActionableScan's ORDER BY.
			priority: 8, category: CategoryPendingData,
			predicate: "st.status IN (?,?) AND st.pending_data_producers <> 0 AND st.origin IS DISTINCT FROM ?",
			args:      []any{stJudged, stSpineDone, originSessionCopy},
		},
		{
			// A JUDGED/SPINE_DONE row whose E or D went CALL_STATUS_FAILED while R committed a valid
			// verdict: retry ONLY the failed producer, carrying R, until it succeeds or the record's
			// retry cap is reached. Like pending-data, CALL_STATUS_FAILED lives in call_state (never
			// SQL-filtered), so the flat failed_producers bitmask is the searchable counterpart; the
			// explicit `<> 0` conjunct keeps the partial index (session_analysis_state_failed_producers)
			// predicate literal. Capped by attempt_count (unlike pending-data, whose input-arrival is
			// unbounded) so a permanently-failing producer stops churning and stays terminally FAILED
			// with R still served. Ranked below pending-data.
			priority: 9, category: CategoryProducerFailed,
			predicate: "st.status IN (?,?) AND st.failed_producers <> 0 AND st.attempt_count < ? AND st.origin IS DISTINCT FROM ?",
			args:      []any{stJudged, stSpineDone, params.RetryCap, originSessionCopy},
		},
	}
	if params.CompatWriteEnabled {
		// A SKIPPED(CHILD) row whose re-upload CHANGED its body but which is STILL a child must be
		// re-priced: CreateBatch drops the priced cost/tokens metadata on a changed payload, and no
		// other leg admits a SKIPPED(CHILD) still-child (cat-11 admits only children that became
		// non-children). Without this, after Session retires the changed child's cost is absent
		// from the parent fold forever. Gated on CompatWriteEnabled: only post-cutover does the
		// engine own child pricing (during the parallel run Session re-prices changed children, so
		// the leg is inert and never churns). Content-drift is the cheap payload sha vs the anchor
		// stamped at the CHILD skip, with the raw_records.updated_at fallback for legacy children.
		// On re-admit prefetchGate re-classifies it CHILD → applySkip re-prices (StampChildCost),
		// refreshes the anchor (so this leg clears), and bumps the parent fence (so it re-folds).
		subs = append(subs, scanSubquery{
			priority: 7, category: CategoryChildReprice,
			predicate: "st.status = ? AND st.skip_reason = ? AND COALESCE(rr.metadata->>?, '') IN (?,?,?) AND (" +
				"(NULLIF(rr.payload->>'content_sha256', '') IS NOT NULL AND NULLIF(rr.payload->>'content_sha256', '') IS DISTINCT FROM st.source_content_sha256) OR " +
				"(NULLIF(rr.payload->>'content_sha256', '') IS NULL AND rr.updated_at IS DISTINCT FROM st.source_updated_at)" +
				")",
			args: []any{
				stSkipped, srChild, lineage.MetaKeyRelationType,
				lineage.RelationTypeSubagent, lineage.RelationTypePlugin, lineage.RelationTypeHook,
			},
		})
	}
	candidates, err := r.runActionableScan(ctx, params.OrgID, params.ConnectorID, subs, limit)
	if err != nil {
		return nil, fmt.Errorf("scanning backfill-lane actionable (org %s): %w", params.OrgID, err)
	}
	return candidates, nil
}

// retryablePredicate/Args build category 2: FAILED under the per-record retry cap,
// OR (regardless of attempt_count) re-admitted while a provider-wide outage signal
// is active and the row's last attempt predates the outage backoff.
func retryablePredicate(params SessionAnalysisScanParams) string {
	if params.ProviderOutageActive {
		return "st.status = ? AND (st.attempt_count < ? OR (st.last_attempted_at IS NULL OR st.last_attempted_at < ?))"
	}
	return "st.status = ? AND st.attempt_count < ?"
}

func retryableArgs(params SessionAnalysisScanParams) []any {
	if params.ProviderOutageActive {
		return []any{stFailed, params.RetryCap, params.ProviderRetryCutoff}
	}
	return []any{stFailed, params.RetryCap}
}

// seatList returns a slice safe for a positional IN (?) expansion — GORM renders an
// empty slice as IN (NULL), which matches nothing, so an empty seat/repo set makes
// the resolved-active membership predicate false rather than erroring.
func seatList(vals []string) []string {
	if len(vals) == 0 {
		return []string{}
	}
	return vals
}

// runActionableScan composes the category subqueries into one UNION ALL, dedups to a
// single category per raw record (DISTINCT ON, lowest priority wins), and orders the
// survivors freshest-first. raw_records is the driving table (connector_id filtered);
// the LEFT JOIN to the live-generation state row is what makes st.id IS NULL the
// no-state leg.
func (r *SessionAnalysisStateRepo) runActionableScan(ctx context.Context, orgID, connectorID string, subs []scanSubquery, limit int) ([]*ActionableCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	var unionParts []string
	var args []any
	joinSQL := "FROM raw_records rr " +
		"LEFT JOIN session_analysis_state st " +
		"ON st.raw_record_id = rr.id AND st.organization_id = rr.organization_id " +
		"AND st.status <> ? AND st.deleted_at IS NULL " +
		"WHERE rr.organization_id = ? AND rr.connector_id = ? AND rr.deleted_at IS NULL AND ("
	for _, sub := range subs {
		part := "SELECT rr.id AS raw_record_id, rr.data_date, '" + string(sub.category) + "' AS category, " +
			fmt.Sprintf("%d AS priority ", sub.priority) +
			joinSQL + sub.predicate + ")"
		unionParts = append(unionParts, part)
		// The JOIN + WHERE bind three args (superseded status, org, connector) ahead
		// of the leg's own args, in every subquery.
		args = append(args, stSuperseded, orgID, connectorID)
		args = append(args, sub.args...)
	}
	// The final ordering is leg-CLASS first, then freshest-first within a class. NO_STATE
	// (never-analysed) ranks above every re-admission so fresh uploads always fill the batch
	// first — the session-proven NO_STATE-first starvation guard. Every re-admission, pending-data included, is a peer below NO_STATE; within one class
	// the order stays data_date DESC, unchanged from the pre-slice single-key ordering.
	classRank := "CASE category WHEN '" + string(CategoryNoState) + "' THEN 0 ELSE 1 END"
	full := "SELECT raw_record_id, data_date, category FROM (" +
		"SELECT DISTINCT ON (raw_record_id) raw_record_id, data_date, category, priority FROM (" +
		strings.Join(unionParts, " UNION ALL ") +
		") legs ORDER BY raw_record_id, priority" +
		") deduped ORDER BY " + classRank + ", data_date DESC LIMIT ?"
	args = append(args, limit)

	var candidates []*ActionableCandidate
	if err := tenantdb.DB(ctx, r.db).Raw(full, args...).Scan(&candidates).Error; err != nil {
		return nil, fmt.Errorf("scanning actionable session-analysis candidates (connector %s): %w", connectorID, err)
	}
	return candidates, nil
}

// ClaimForProcessing inserts a fresh PENDING state row for a never-analysed raw
// record, rotating a new claim_token, and returns whether this call won the claim.
// The claim is INSERT ... ON CONFLICT (organization_id, raw_record_id) WHERE
// status <> SUPERSEDED AND deleted_at IS NULL DO NOTHING — the live-generation
// partial unique is the double-claim backstop, so two concurrent ticks that both
// pick a no-state row race here and exactly one inserts. The
// caller must set OrganizationID, RawRecordID, ConversationStartedAt,
// VocabularyVersion, and PersonID; this stamps the id, status, and claim_token.
func (r *SessionAnalysisStateRepo) ClaimForProcessing(ctx context.Context, state *model.SessionAnalysisState) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "ClaimForProcessing", time.Now())
	if state == nil {
		return false, fmt.Errorf("claiming session analysis: nil state")
	}
	if state.ID == "" {
		state.ID = uuid.NewString()
	}
	state.Status = sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_PENDING
	token := uuid.NewString()
	state.ClaimToken = &token
	now := time.Now().UTC()
	state.LastAttemptedAt = &now

	// The arbiter predicate is INLINED as a constant, never a bind parameter: Postgres
	// infers a partial-index conflict target by proving the ON CONFLICT WHERE is implied
	// by the index predicate, and a bind parameter (status <> $1) cannot be proven to
	// imply `status <> 'SESSION_ANALYSIS_STATUS_SUPERSEDED'` once the statement warms up to
	// a generic plan — the inference fails with SQLSTATE 42P10 after ~5 executions on a
	// connection. The literal matches session_analysis_state_one_live exactly, so inference
	// holds under both custom and generic plans. stSuperseded is a code constant (the enum
	// name), never user input, so inlining it carries no injection risk.
	res := tenantdb.DB(ctx, r.db).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "status <> '" + stSuperseded + "' AND deleted_at IS NULL"}}},
		DoNothing:   true,
	}).Create(state)
	if res.Error != nil {
		return false, fmt.Errorf("claiming session analysis (org %s, raw_record %s): %w", state.OrganizationID, state.RawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// Reclaim re-takes the lease on an already-existing state row (a retryable failure,
// content-change re-admit, or a lease-expired non-terminal row). It rotates
// claim_token, moves the row to PENDING, and re-stamps
// last_attempted_at. It is CAS-gated on expectedClaimToken (the token the caller read
// at scan time, NULL-safe): once the winning tick rotates the token, a second tick
// racing the same row sees a changed token and matches zero rows — so two cron
// deliveries scanning one failed/changed/expired row cannot both re-claim it and both
// spend S3/LLM. Returns whether this call won the re-claim. attempt_count is reset to
// 0 on a content-change or version re-open (resetAttempts), left intact on a plain
// retry so the retry cap still bounds a single generation's own failures.
func (r *SessionAnalysisStateRepo) Reclaim(ctx context.Context, rawRecordID string, expectedClaimToken *string, resetAttempts bool) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "Reclaim", time.Now())
	token := uuid.NewString()
	updates := map[string]any{
		"status":            stPending,
		"claim_token":       token,
		"last_attempted_at": time.Now().UTC(),
		"updated_at":        time.Now().UTC(),
	}
	if resetAttempts {
		updates["attempt_count"] = 0
	}
	// IS NOT DISTINCT FROM is the NULL-safe equality: a SKIPPED row awaiting
	// re-admission carries a NULL claim_token, and expectedClaimToken is nil there.
	res := tenantdb.DB(ctx, r.db).Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ? AND status <> ? AND deleted_at IS NULL AND claim_token IS NOT DISTINCT FROM ?",
			rawRecordID, stSuperseded, expectedClaimToken).
		Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("reclaiming session analysis (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// TransitionToSpineDone moves a claimed PENDING row to SPINE_DONE, stamping the
// measured-pass anchors (source content sha, source updated_at, spine version). CAS-gated
// on the claim_token so a lease-expired-but-alive worker cannot land after a re-claim
// rotated the token. sourceUpdatedAt is the raw record's
// updated_at at claim time — the legacy staleness baseline the category-3 scan and the
// commit re-read compare against for a body with no content_sha256. Returns whether the
// CAS matched.
func (r *SessionAnalysisStateRepo) TransitionToSpineDone(ctx context.Context, rawRecordID, claimToken, sourceContentSha256 string, sourceUpdatedAt time.Time, spineVersion int32, conversationStartedAt time.Time) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "TransitionToSpineDone", time.Now())
	res := tenantdb.DB(ctx, r.db).Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ? AND claim_token = ? AND status = ? AND deleted_at IS NULL", rawRecordID, claimToken, stPending).
		Updates(map[string]any{
			"status":                stSpineDone,
			"source_content_sha256": sourceContentSha256,
			"source_updated_at":     sourceUpdatedAt,
			"spine_version":         spineVersion,
			// Reconcile the state row's conversation_started_at (seeded from raw_records.data_date
			// at claim) to the instant the spine parsed from the conversation body, so the
			// the reprocess job window filter and the cat-7 window-widen re-admit — both of
			// which read this state column — select on the same authoritative timestamp the
			// content tables carry, not the coarser data_date proxy (they can differ by a day for
			// near-midnight and legacy uploads).
			"conversation_started_at": conversationStartedAt,
			"updated_at":              time.Now().UTC(),
		})
	if res.Error != nil {
		return false, fmt.Errorf("transitioning session analysis to spine_done (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// JudgedCommit carries the state columns the judged commit stamps alongside the
// content-table writes, all in one tenant-scoped transaction.
type JudgedCommit struct {
	CallState               *sessionintelligencepb.CallStateMap
	ContractVersionRead     int32
	ContractVersionEvidence int32
	ContractVersionDiscard  int32
	VocabularyVersion       int32
	FlagSkippedProducers    int32
	// PendingDataProducers is the SKIPPED_PENDING_DATA bitmask; the scan's pending-data
	// re-admission leg predicates on it.
	PendingDataProducers int32
	// FailedProducers is the CALL_STATUS_FAILED bitmask — a producer errored while R committed a
	// valid verdict. The failed-producer re-admission leg predicates on it, retrying only that
	// producer under the retry cap. Non-zero forces the attempt-count semantics below to COUNT
	// (increment, capped) instead of RESET, unlike a fully-successful judge.
	FailedProducers int32
	// IncrementAttempt gates the attempt-count bump on a FailedProducers judge: false for a failure
	// under an active provider-wide outage (the failure is the provider's, not the record's), so a
	// long outage cannot exhaust the retry cap and strand the producer — mirrors TransitionToFailed.
	IncrementAttempt bool
	// LastError is the producer error to record on a FailedProducers judge (nil on full success,
	// which clears it).
	LastError   *string
	Disposition sessionintelligencepb.JudgedDisposition
	// PersonID is the current metadata person_id, re-stamped onto the state row on every judge so
	// an unchanged-payload re-upload that corrected the owner is reflected in state (nil = none).
	PersonID *string
}

// TransitionToJudged is the state half of the Stage-2 read commit: it CAS-validates the
// claim_token (the lease still holds — no re-claim landed) and moves a SPINE_DONE/PENDING row to
// JUDGED with the per-producer contract stamps and the flag-skipped/pending/failed bitmasks.
// Returns whether the CAS matched. Runs inside the caller's commit transaction.
//
// The CAS is deliberately NOT fenced on the children_seq snapshot (the durable staged
// commit): a child sighted mid-flight must never block a valid R verdict from landing JUDGED,
// or the paid R (and E/D) would be discarded and — for a first-time parent with no prompt re-admit
// leg — re-billed after a lease-expiry strand. foldedChildrenSeq records what this pass actually
// folded; when a child arrived during the pass (children_seq > folded_children_seq) the child-refold
// scan leg re-admits the now-JUDGED parent to fold the delta, carrying R by rehydration. The lease
// (claim_token) alone guards against a stolen-lease double-write.
func (r *SessionAnalysisStateRepo) TransitionToJudged(ctx context.Context, rawRecordID, claimToken string, foldedChildrenSeq int32, commit JudgedCommit) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "TransitionToJudged", time.Now())
	now := time.Now().UTC()
	updates := map[string]any{
		"status":                    stJudged,
		"call_state":                commit.CallState,
		"contract_version_read":     commit.ContractVersionRead,
		"contract_version_evidence": commit.ContractVersionEvidence,
		"contract_version_discard":  commit.ContractVersionDiscard,
		"vocabulary_version":        commit.VocabularyVersion,
		"flag_skipped_producers":    commit.FlagSkippedProducers,
		"pending_data_producers":    commit.PendingDataProducers,
		"failed_producers":          commit.FailedProducers,
		// Re-stamp person_id from the current metadata so an unchanged-payload re-upload that
		// corrected the owner refreshes state (the ownership-drift re-admit leg then clears).
		"person_id":           commit.PersonID,
		"last_succeeded_at":   now,
		"folded_children_seq": foldedChildrenSeq,
		"updated_at":          now,
	}
	// attempt_count semantics: a fully-successful judge RESETS it to 0 (the record is done), while a
	// judge that carries a FAILED producer COUNTS toward the retry cap that bounds the failed-producer
	// re-admission leg — unless the failure was a provider outage (IncrementAttempt=false), which leaves
	// the count unchanged so an outage cannot strand the producer at the cap.
	switch {
	case commit.FailedProducers == 0:
		updates["attempt_count"] = 0
		updates["last_error"] = nil
	case commit.IncrementAttempt:
		updates["attempt_count"] = gorm.Expr("attempt_count + 1")
		updates["last_error"] = commit.LastError
	default:
		updates["last_error"] = commit.LastError
	}
	res := tenantdb.DB(ctx, r.db).Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ? AND claim_token = ? AND status IN (?,?) AND deleted_at IS NULL",
			rawRecordID, claimToken, stSpineDone, stPending).
		Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("transitioning session analysis to judged (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// TransitionToFailed records a failed attempt. incrementAttempt is false for a
// failure under an active provider-wide outage (the failure is the provider's, not
// the record's), so a long outage does not inflate attempt_count past the retry cap
// and strand the row when the signal clears. last_error always reflects
// the latest attempt.
// TransitionToFailed also advances the staleness anchor to the body that was actually attempted
// (source_content_sha256 = the current payload digest, NULL for a legacy body; source_updated_at =
// its current updated_at). Without this, a changed body that fails BEFORE spine_done never stamps
// the anchor, so the content-changed scan leg keeps re-admitting it every tick (resetting
// attempt_count) and it never reaches terminal FAILED — a retry-cap-defeat cost loop. Advancing the
// anchor here makes the SAME body read as unchanged next tick, so the retry cap bounds it to FAILED;
// a genuine later re-upload still differs from the stamped digest and legitimately re-admits.
// Returns (claimHeld, attemptCount): claimHeld is false when the lease was stolen (no row
// updated); attemptCount is the row's attempt_count after this transition, so the caller can
// tell a retryable failure (attemptCount < RetryCap) from a terminal one and gate terminal-only
// work (the JUDGE_FAILED serving stamp) on it.
func (r *SessionAnalysisStateRepo) TransitionToFailed(ctx context.Context, rawRecordID, claimToken, lastError string, incrementAttempt bool, sourceContentDigest string, sourceUpdatedAt time.Time) (bool, int32, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "TransitionToFailed", time.Now())
	updates := map[string]any{
		"status":                stFailed,
		"last_error":            lastError,
		"last_attempted_at":     time.Now().UTC(),
		"source_content_sha256": gorm.Expr("NULLIF(?, '')", sourceContentDigest),
		"source_updated_at":     sourceUpdatedAt,
		"updated_at":            time.Now().UTC(),
	}
	if incrementAttempt {
		updates["attempt_count"] = gorm.Expr("attempt_count + 1")
	}
	var updated model.SessionAnalysisState
	res := tenantdb.DB(ctx, r.db).Model(&updated).
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "attempt_count"}}}).
		Where("raw_record_id = ? AND claim_token = ? AND deleted_at IS NULL", rawRecordID, claimToken).
		Updates(updates)
	if res.Error != nil {
		return false, 0, fmt.Errorf("transitioning session analysis to failed (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, updated.AttemptCount, nil
}

// MarkSkipped writes (or updates to) a conditional SKIPPED row stamped with the
// gate that fired and the key it will be re-admitted on (person via seats,
// conversation_started_at via cutoff, or repo_name). Every skip is
// conditional; there is no terminal SKIPPED. Upserts on the live-generation partial
// unique so a re-gate of an existing row overwrites its reason.
// MarkSkipped upserts the SKIPPED marker and reports whether it INSERTED a new row (vs updated
// an existing skip). The caller gates the parent-fence bump on that insert: a delegated-child skip
// uses ON CONFLICT DO NOTHING (a child is immutably CHILD, so a repeat sighting is a no-op that
// reports RowsAffected==0), so two concurrent ticks that both see a not-yet-marked child bump the
// parent's children_seq exactly once. A non-child skip keeps DO UPDATE (a re-admitted row's
// skip_reason can change), where RowsAffected is 1 on both insert and update — but non-child
// callers ignore the flag.
func (r *SessionAnalysisStateRepo) MarkSkipped(ctx context.Context, state *model.SessionAnalysisState, reason sessionintelligencepb.SessionAnalysisSkipReason) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "MarkSkipped", time.Now())
	if state == nil {
		return false, fmt.Errorf("marking session analysis skipped: nil state")
	}
	if state.ID == "" {
		state.ID = uuid.NewString()
	}
	state.Status = sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED
	state.SkipReason = &reason
	onConflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}},
		// The arbiter predicate inlines the SUPERSEDED literal rather than binding it:
		// a bound parameter in a partial-index ON-CONFLICT predicate defeats Postgres's
		// partial-index inference after the statement's plan is cached (~5 execs on a
		// pooled connection), erroring 42P10 on every subsequent skip write. stSuperseded
		// is a proto enum's .String(), never user input, so inlining is injection-safe.
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "status <> '" + stSuperseded + "' AND deleted_at IS NULL"}}},
		DoUpdates:   clause.AssignmentColumns([]string{"status", "skip_reason", "repo_name", "person_id", "updated_at"}),
	}
	if reason == sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD {
		onConflict.DoNothing = true
		onConflict.DoUpdates = nil
	}
	res := tenantdb.DB(ctx, r.db).Clauses(onConflict).Create(state)
	if res.Error != nil {
		return false, fmt.Errorf("marking session analysis skipped (org %s, raw_record %s): %w", state.OrganizationID, state.RawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// RefreshChildContentAnchor updates the content-change anchors (source_content_sha256 +
// source_updated_at) on a SKIPPED(CHILD) row after it was re-priced for a body change. MarkSkipped
// uses ON CONFLICT DO NOTHING for a child (to protect the first-sight fence bump), so it does not
// advance the anchor on a re-sight; this does, so the child-reprice scan leg (which compares the
// new payload sha to the stored anchor) stops matching once the change is priced. Scoped to the
// live SKIPPED(CHILD) row, in the caller's tenant transaction.
func (r *SessionAnalysisStateRepo) RefreshChildContentAnchor(ctx context.Context, rawRecordID string, sourceContentSha256 *string, sourceUpdatedAt time.Time) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "RefreshChildContentAnchor", time.Now())
	res := tenantdb.DB(ctx, r.db).Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ? AND status = ? AND skip_reason = ? AND deleted_at IS NULL", rawRecordID, stSkipped, srChild).
		Updates(map[string]any{
			"source_content_sha256": sourceContentSha256,
			"source_updated_at":     sourceUpdatedAt,
			"updated_at":            time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("refreshing child content anchor (raw_record %s): %w", rawRecordID, res.Error)
	}
	return nil
}

// BumpChildrenSeq increments the parent's child fence on first sighting of a child
// raw record, in the caller's tenant-scoped transaction. The
// caller gates this on the SKIPPED(CHILD) marker insert actually inserting, so two
// concurrent ticks that both see a not-yet-marked child cannot double-bump. Returns
// whether the parent row existed (a child seen before its parent is a no-op here;
// the parent picks up the child when it first runs).
func (r *SessionAnalysisStateRepo) BumpChildrenSeq(ctx context.Context, parentRawRecordID string) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "BumpChildrenSeq", time.Now())
	// A SESSION_COPY parent is frozen history (session-analyses-serving-parity design): never advance
	// its fence, so its children_seq stays == folded_children_seq and the child-refold leg cannot
	// re-admit it. A live delegated child of a copied session is not this engine's to fold.
	res := tenantdb.DB(ctx, r.db).Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ? AND status <> ? AND deleted_at IS NULL AND origin IS DISTINCT FROM ?", parentRawRecordID, stSuperseded, originSessionCopy).
		Updates(map[string]any{
			"children_seq": gorm.Expr("children_seq + 1"),
			"updated_at":   time.Now().UTC(),
		})
	if res.Error != nil {
		return false, fmt.Errorf("bumping children_seq (parent raw_record %s): %w", parentRawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// Supersede marks a specific old generation SUPERSEDED, CAS-targeting the exact
// (raw_record_id, vocabulary_version) rather than "the current live row", so a
// concurrent version-behind detection cannot supersede a fresh generation another
// worker just inserted. The fresh generation is written by ClaimForProcessing.
//
// The state table has no superseded_at column — unlike the session_analyses content
// table, its live-generation gate is status <> SUPERSEDED (the session_analysis_state_one_live
// partial index), so this keys on status alone. The status <> stSuperseded guard also
// makes the transition idempotent.
func (r *SessionAnalysisStateRepo) Supersede(ctx context.Context, rawRecordID string, oldVocabularyVersion int32) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisStateRepo", "Supersede", time.Now())
	res := tenantdb.DB(ctx, r.db).Model(&model.SessionAnalysisState{}).
		Where("raw_record_id = ? AND vocabulary_version = ? AND deleted_at IS NULL AND status <> ?",
			rawRecordID, oldVocabularyVersion, stSuperseded).
		Updates(map[string]any{
			"status":     stSuperseded,
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return false, fmt.Errorf("superseding session analysis generation (raw_record %s, vocab %d): %w", rawRecordID, oldVocabularyVersion, res.Error)
	}
	return res.RowsAffected == 1, nil
}
