// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import "time"

// Config is the dynamic, scheduler-homed configuration the processor reads
// . It is an interface so this package does
// not import the scheduler config package (each service group reads only its own
// config); the wire layer adapts genconf.Config to it. Every knob here is owned by
// us uniformly (a version stamp, a throttle, a budget) or per rollout phase (the
// per-producer flags), never by the customer.
type Config interface {
	// Enabled is the per-org master gate. When false the processor is registered and
	// scheduled but is a no-op for the org; only an org it returns true for is analysed.
	// Default false.
	Enabled(orgID string) bool
	// BatchSize caps candidates claimed per lane per tick.
	BatchSize() int
	// RecordConcurrency bounds how many records the batch processes in parallel.
	RecordConcurrency() int
	// GlobalBudgetPerOrgTick is the hard ceiling on paid work per org per tick.
	GlobalBudgetPerOrgTick() int
	// RetryCap bounds a single record's own transient failures.
	RetryCap() int32
	// ReprocessMinInterval is the content-change re-run cooldown (default 24h).
	ReprocessMinInterval() time.Duration
	// ChildRefoldMinInterval throttles the re-fold of a JUDGED parent whose delegated
	// children arrived after its last fold, so a continuously-delegating session re-folds at
	// most once per interval rather than every tick (default 12h).
	ChildRefoldMinInterval() time.Duration
	// PerRecordTimeout bounds one record's spine + producers; it also sizes the
	// category-5 lease margin.
	PerRecordTimeout() time.Duration

	// Provider-wide-outage detector + re-admission knobs.
	// OutageSignalWindow / OutageSignalMinRecords define when the rolling provider-side-
	// failure signal is active; ProviderRetryBackoff is the delay a FAILED row waits before
	// the active-outage re-admission re-picks it (default 30m).
	OutageSignalWindow() time.Duration
	OutageSignalMinRecords() int
	ProviderRetryBackoff() time.Duration
	// ChunkCharLimit is the rendered-character projection above which the spine
	// windows the read pass.
	ChunkCharLimit() int
	// ChunkWindowPrompts is the count of human turns whose tags are authoritative in one
	// windowed-read pass; ChunkLookaheadPrompts is the read-only look-ahead of later turns
	// each window may consult but whose tags are discarded (the next window owns them).
	ChunkWindowPrompts() int
	ChunkLookaheadPrompts() int

	// CutoverDate is the per-org hard write floor: the processor picks only raw_records with
	// data_date >= this date; records before it are never LLM-processed (they are carried onto
	// the new tables by a backfill job). A zero value means
	// the org has no floor — the no-state scan admits the org's full in-window history, bounded
	// only by the prefetch gates (OUT_OF_WINDOW against the plan data-window cutoff, NO_SEAT).
	CutoverDate(orgID string) time.Time

	// Version stamps and their effective_from dates (a row is version-behind only
	// for data_date >= the version's effective_from).
	VocabularyVersion() int32
	VocabularyEffectiveFrom() time.Time
	SpineVersion() int32
	SpineEffectiveFrom() time.Time
	ContractVersionRead() int32
	ContractVersionEvidence() int32
	ContractVersionDiscard() int32
	ContractEffectiveFrom() time.Time

	// UseSystemPromptCache opts every producer call into Gemini system-prompt caching.
	UseSystemPromptCache() bool

	// ReadEnabled is the per-org read-pass gate. When false R's call is FLAG_SKIPPED (no
	// inference), the row lands unserved (judged_at stays NULL), and the category-9
	// producer-enabled scan leg re-admits it once the flag turns on.
	ReadEnabled(orgID string) bool

	// EvidenceEnabled is the per-org gate for the ENTIRE E producer: the round-trip PAIRS detector (enumerate + reference-llm NLI scoring), E's Gemini
	// inference, and E's writes. Dark by default: E ships off until an operator arms it per org,
	// once E's output and security-slug precision are validated for the deployed version. When
	// off, enabledProducerBits omits the EVIDENCE bit — the call is FLAG_SKIPPED and the row
	// serves R's judged columns; the category-9 scan leg re-admits it to run E once the flag is on.
	EvidenceEnabled(orgID string) bool
	// DetectorTopN caps how many above-threshold pairs, highest score first, cross to E per
	// session (N in the tens; one value for all orgs, not per-org). E judges every pair it receives and never
	// chunks, so this bounds E's per-session round-trip cost.
	DetectorTopN() int
	// DetectorConcurrency bounds how many detector (reference-llm) calls run in parallel across a
	// batch's records, separate from the Gemini in-flight bound (one value for all orgs, not per-org).
	DetectorConcurrency() int
	// DetectorPairCeiling is the hard per-session pair ceiling (~800-1000). A pathological
	// whale session is scored up to the ceiling and flagged detector-truncated, the same
	// legible degrade as D's overflow (a cost-safety knob with one value for all orgs, not per-org).
	DetectorPairCeiling() int

	// EvidenceSecurityFlagPublishEnabled gates whether E persists its security slugs to
	// security_flags per org. The security tabs read
	// security_flags directly, so the aggregate serving flag does not cover that egress;
	// this is its dedicated write gate. When false E's reconcile computes its security
	// slugs but does not persist them; R's slugs are unaffected. Default false.
	EvidenceSecurityFlagPublishEnabled(orgID string) bool

	// CompatWriteEnabled gates the phase-5 processing_status compatibility write
	//. It is a GLOBAL toggle, not per-org. When false the
	// engine writes no raw_records.processing_status at all — its sole guarantee is that
	// it never touches that column. When true each terminal state stamps the session
	// row's processing_status (JUDGED/any skip → processed, hard failure → failed) so a
	// downstream loop-retry join and the CDC-to-BQ freshness feed track the engine's
	// outcomes. Default false.
	CompatWriteEnabled() bool
}
