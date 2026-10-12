// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
)

// producer keys — the CallStateMap keys and the flag_skipped_producers bit indices.
const (
	producerRead     = "READ"
	producerEvidence = "EVIDENCE"
	producerDiscard  = "DISCARD"

	bitRead     int32 = 1 << 0
	bitEvidence int32 = 1 << 1
	bitDiscard  int32 = 1 << 2
)

// producerBits is the single key→bit registry the mask machinery derives from. Adding a
// producer is one entry here (plus its key/bit const above); producerBit reads only this map,
// so a new producer cannot silently miss its flag_skipped_producers / pending_data_producers
// bit. TestProducerBitsRegistryConsistent guards that every key maps to a distinct power-of-two
// bit and that the registry covers exactly the producer-key constants.
var producerBits = map[string]int32{
	producerRead:     bitRead,
	producerEvidence: bitEvidence,
	producerDiscard:  bitDiscard,
}

// spineResult is the spine's output plus the orchestration bookkeeping the commit
// needs. The spine is the free, no-LLM static pass that runs on every session
// : it fetches the body, derives the measured columns,
// and makes the chunk decision. Whether the producers then run is decided by
// spineOnly (a spine-version bump that left every producer-read input unchanged
// runs zero LLM calls — the zero-cost path).
type spineResult struct {
	claimToken string
	// foldedChildrenSeq is what the judge records as folded (written to folded_children_seq): it
	// equals the parent's children_seq at fold time only when the fold completed, and stays at the
	// prior fold point on a fold-read failure so the child-refold scan leg re-admits the parent to
	// retry. A child sighted after the fold leaves children_seq > folded_children_seq, which the
	// child-refold scan leg (never any commit-time CAS) later folds.
	foldedChildrenSeq     int32
	conversationStartedAt time.Time
	// lastActiveAt is the raw record's fetched_at — the desktop app's last-sync time, which the
	// upsert refreshes every re-sync so it tracks the growing session's latest activity (the session
	// last_conversation_at equivalent). Written to session_analyses.last_active_at; the backfill
	// takes the same value from the session insight's last_conversation_at.
	lastActiveAt time.Time
	// conversationID is the live session's conversation identifier (the session insight's
	// conversation_id equivalent), written to session_analyses so a live row carries it like a
	// copied row does. Empty when unavailable.
	conversationID string
	contentSha256  string
	// sourceUpdatedAt is the raw record's updated_at at claim time — the legacy staleness
	// baseline stamped at spine_done and re-read at commit for a body with no content_sha256
	//.
	sourceUpdatedAt   time.Time
	spineVersion      int32
	vocabularyVersion int32

	// llmContent is the LLM-input payload R reads: the rendered conversation transcript
	// (sessionintelligence.ConversationToText) with file paths hashed as a send-transform
	// (anonymizePaths). Built by the spine; read once, by R only.
	llmContent string
	// prompts is the parsed conversation, kept so the chunked read can window the
	// transcript by human turn without re-parsing (nil on the spine-only path, which
	// runs no producer).
	prompts []*sessionintelligencepb.Prompt
	// sessionEvents is the normalized event timeline, kept so the measured turn pass can
	// derive the event-grain columns (tool_retry_count, compaction_count, precise
	// authorship) without re-parsing. Nil for a body whose adapter emits no timeline — the
	// zero-vs-null gate reads that absence as "couldn't observe" and leaves those columns
	// NULL. Also nil on the spine-only path (which builds no turn rows).
	sessionEvents []*sessionintelligencepb.SessionEvent
	// chunked is set when the projected render exceeds the char limit (windowed read).
	chunked bool
	// turnIndexMap is the single-chunk render's Sequence->Index map (sequenceToRawIndex,
	// spine_blocks.go): R tags a turn/phase by its rendered position (Sequence), but the
	// measured turn table is keyed by the raw whole-session ordinal (Index), so the
	// single-chunk read pass (chunking.go's runReadPass) translates through this map before a
	// judged turn or phase is written. Unused on the chunked
	// path, which builds its own per-window map instead.
	turnIndexMap []int32
	// turnBlocks is the turn-grouped body the spine cut once (buildSpineContent), reused by every
	// producer's render — R's spine content and D's discard content — so the two never re-parse the
	// conversation or disagree on the turn set. Populated by buildSpineContent on every spine() call,
	// including the spine-only path; simply unread there, since no producer runs.
	turnBlocks []*turnBlock

	// ledgers are the spine's static (no-LLM) E/D fire signals: the artifact/review ledger
	// gates E, the branch/revert ledger gates D.
	ledgers spineLedgers

	// roundTripCandidates are the PAIRS detector's scored, above-threshold top-N candidate
	// pairs for this session — the recall-oriented input to E's round-trip half, produced by
	// the session-analysis producers. Populated by runProducers only when the per-org detector
	// gate is on; nil (never scored) otherwise. detectorTruncated marks a whale session whose
	// guard-valid pairs overflowed the ceiling and were scored only up to it.
	roundTripCandidates []DetectedPair
	detectorTruncated   bool

	// Measured telemetry the static pass derives from the parsed body — the code-owned
	// half serving reads (cost, tokens, duration, counts). The model never re-derives
	// or contradicts these.
	metrics   *sessionintelligencepb.ConversationMetrics
	usage     *sessionintelligencepb.UsageMetadata
	ideType   sessionintelligencepb.IdeType
	turnCount int32
	repoName  string

	// promptTokenCosts is the measured input+output token cost per turn, indexed by the
	// turn's prompt_index (the human turn ordinal, one entry per user prompt plus its
	// following assistant response — the same pairing the session waste extractor uses).
	// The commit sums it over D's discarded ranges into wasted_tokens (the waste ledger
	// is a MEASURED column over D's judged ranges — the LLM output contract,
	// Call 5).
	promptTokenCosts []int64

	// perTurnHitlGate is the measured human-in-the-loop gate active at each human turn,
	// indexed by the turn's prompt_index — the same index domain as promptTokenCosts, so
	// the phase commit reads it at a phase's RangeStart. The source stamps the mode only where
	// it changes (Claude Code permissionMode is sparse), so this carries the last stamped mode
	// forward to every later turn; an entry is nil until the first stamped mode (and throughout
	// for a source with no per-turn mode). Derived by the code, never judged by the model.
	perTurnHitlGate []*sessionintelligencepb.HitlGate

	// dispatchToolUseToPromptIndex maps a sub-agent dispatch's tool-use id to the parent
	// human-turn (splitHumanTurns group index == prompt_index) it fell in, derived from the
	// normalized session_events timeline. The commit passes it to
	// BackfillChildParentPromptIndex so a delegated child's turn rows resolve which parent
	// turn spawned them. Nil when the body carries no
	// call-id-bearing timeline (a no-timeline adapter — the child stays NULL until it can be
	// resolved). Only ids that are an actual child's parent_tool_use_id ever match a row, so
	// a non-dispatch call id in the map is inert.
	dispatchToolUseToPromptIndex map[string]int32

	// spineOnly is true when no producer needs to re-run (content + contracts
	// unchanged since the last judged generation): every LLM call is skipped.
	spineOnly bool

	callState            *sessionintelligencepb.CallStateMap
	flagSkippedProducers int32
	// pendingDataProducers is the SKIPPED_PENDING_DATA counterpart to flagSkippedProducers —
	// the bitmask of producers whose input dependency was not yet resolvable. finalize
	// recomputes it from call_state so a producer that resolves clears its bit, and the commit
	// stamps it onto session_analysis_state.pending_data_producers for the scan's re-admission
	// leg.
	pendingDataProducers int32
	// failedProducers is the CALL_STATUS_FAILED counterpart to the two masks above — the bitmask of
	// producers whose inference errored while R committed a valid verdict. finalize recomputes it
	// from call_state (so a producer that later succeeds clears its bit), and the commit stamps it
	// onto session_analysis_state.failed_producers for the failed-producer re-admission leg
	//.
	failedProducers int32
	// failedProducerError / failedProducerIncrementAttempt carry a producer failure through to the
	// judged commit: the error to record on the state row, and whether the failure counts toward the
	// retry cap (false for a provider-outage failure, mirroring markFailed). Set by markProducerFailed
	// when E or D errors after R committed a valid verdict.
	failedProducerError            string
	failedProducerIncrementAttempt bool
	disposition                    sessionintelligencepb.JudgedDisposition
}

// ReadInputHash covers the producer-read inputs R depends on — today the body
// content digest plus R's contract version — so a spine change confined to measured
// columns no producer reads leaves this unchanged and R skips, while a body or
// contract change flows through and re-runs R.
func ReadInputHash(contentSha256 string, contractVersionRead int32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", contentSha256, contractVersionRead)))
	return hex.EncodeToString(sum[:])
}

// buildMeasuredUsage assembles the session's full measured usage exactly as the session pipeline
// does, through the shared sessionintelligence.AssembleStaticUsage seam — the derived counts, the SessionMeta
// / SessionFacets signals, installation metadata (plan tier, seat price, account, entrypoint), and
// the cost flags (the vendor-cost overwrite mutates metrics in place). It then runs
// ComputeNormalizedFields, which the session pipeline defers to after its LLM call: that stage
// reads no analysis output (its overallScore arg is unused) and is idempotent, so running it here
// in the spine yields the same normalized fields (effective churn/duration, mcp_server_count,
// delegation level) without an LLM. Sharing the one seam is what keeps the engine and session from
// drifting; it is also a test seam so a corpus test can pin the whole fold from a Conversation.
func buildMeasuredUsage(ctx context.Context, orgID, recordID string, conv *sessionintelligencepb.Conversation, metrics *sessionintelligencepb.ConversationMetrics, repoName string) *sessionintelligencepb.UsageMetadata {
	usage := sessionintelligence.AssembleStaticUsage(ctx, orgID, recordID, conv, metrics, repoName)
	sessionintelligence.ComputeNormalizedFields(conv, usage, 0)
	return usage
}

// spine runs the static pass: fetch the S3 body, resolve the content digest, parse
// the conversation, make the chunk decision, and decide whether the producers must
// re-run. It never calls an LLM.
func (p *SessionAnalysisProcessor) spine(ctx context.Context, record *model.RawRecordModel, meta *rawRecordMetadata, prior *model.SessionAnalysisState, claimToken string) (*spineResult, error) {
	payload, err := parsePayload(record.Payload)
	if err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	body, err := p.s3Client.ReadObject(ctx, payload.S3Key)
	if err != nil {
		return nil, fmt.Errorf("read s3 object %q: %w", payload.S3Key, err)
	}
	// Best-effort: the conversation_id rides the S3 key (…/<started_at>_<conversation_id>.jsonl).
	// A malformed key is not fatal — leave conversation_id empty rather than fail the spine.
	var conversationID string
	if parts, perr := sessionintelligence.ParseS3Key(payload.S3Key); perr == nil { //swallowcheck:ignore -- best-effort: a malformed S3 key leaves conversation_id empty (see comment above), never fails the spine.
		conversationID = parts.ConversationID
	}

	// The cheap staleness signal is the payload's content_sha256 when present; the spine
	// also records its own body digest so a legacy row rejoins the content-hash path once the
	// client starts stamping it. A legacy row with no payload content_sha256 has no cheap body
	// digest to compare — for it the staleness signal is raw_records.updated_at, captured here
	// (sourceUpdatedAt), stamped at spine_done, and re-read at commit and in the category-3
	// scan.
	contentSha256 := payload.ContentSha256
	if contentSha256 == "" {
		contentSha256 = ContentDigest(body)
	}

	conv, err := sessionintelligence.ParseConversation(body)
	if err != nil {
		return nil, fmt.Errorf("parse conversation: %w", err)
	}
	conversationStartedAt := conversationStart(conv, record.DataDate)

	// The measured static pass: token/cost accounting and the session telemetry, code-
	// owned and shared with the session extractors so the two pipelines cannot drift.
	ideType := conv.GetIdeType()
	metrics := sessionintelligence.ExtractMetrics(conv.GetPrompts(), ideType)
	usage := buildMeasuredUsage(ctx, record.OrganizationID, record.ID, conv, metrics, meta.RepoName)
	// buildMeasuredUsage set the cost-provenance flags (CostIsEstimated/CostIsInputOnly) from the
	// parent's OWN cost; the fold below then adds delegated-child cost into metrics.EstimatedCostUsd.
	// This pre-fold order is deliberate parity with the session pipeline, which likewise computes
	// the flags before folding child cost — so a delegated session's served flags match session's
	// rather than describing the folded total. Do not recompute the flags after the fold, or the
	// engine diverges from the source it is replacing.
	// Fold this parent's delegated children's cost/tokens into the measured row before it is
	// built, so delegated usage is not lost. A child
	// seen before its parent folds here; one sighted after the snapshot is left for the child-refold
	// scan leg (children_seq > folded_children_seq), never a commit-time CAS.
	// folded reports whether the fold completed: only then may folded_children_seq advance to the
	// snapshot; on a fold-read failure it stays at the prior fold point so the child-refold scan
	// leg re-admits the parent to retry (else the failed fold would silently undercount forever).
	childrenSeqSnapshot := childrenSeqOf(prior)
	folded := p.foldChildUsage(ctx, record, meta, metrics, usage)
	foldedChildrenSeq := foldedChildrenSeqOf(prior)
	switch {
	case folded:
		foldedChildrenSeq = childrenSeqSnapshot
	case prior == nil:
		// First-run fold-read failure: children_seq is 0 even when a child was sighted before
		// the parent (BumpChildrenSeq no-ops with no parent row to update), so holding the fold
		// point at 0 leaves children_seq(0) == folded_children_seq(0) and the child-refold leg
		// never retries — a silent, permanent undercount of that child's cost. Drop the fold
		// point strictly below the snapshot so children_seq(0) > folded(-1) trips the retry; a
		// later successful fold sets folded == children_seq and settles. (A non-first-run failure
		// already leaves a detectable gap whenever an unfolded child exists, because that child
		// bumped children_seq past the prior fold point.)
		foldedChildrenSeq = childrenSeqSnapshot - 1
	}

	sp := &spineResult{
		claimToken:            claimToken,
		foldedChildrenSeq:     foldedChildrenSeq,
		conversationStartedAt: conversationStartedAt,
		lastActiveAt:          record.FetchedAt,
		conversationID:        conversationID,
		contentSha256:         contentSha256,
		sourceUpdatedAt:       record.UpdatedAt,
		spineVersion:          p.cfg.SpineVersion(),
		vocabularyVersion:     versionForDate(p.cfg, record.DataDate),
		prompts:               conv.GetPrompts(),
		sessionEvents:         conv.GetSessionEvents(),
		disposition:           sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED,
		metrics:               metrics,
		usage:                 usage,
		ideType:               ideType,
		turnCount:             int32(len(conv.GetPrompts())),
		repoName:              meta.RepoName,
		promptTokenCosts:      perTurnTokenCosts(conv.GetPrompts()),
		perTurnHitlGate:       perTurnHitlGates(conv.GetPrompts()),
		ledgers:               buildSpineLedgers(conv),
		// The parent's sub-agent-dispatch → prompt_index map for the child backfill (#7).
		// Derived on every parse (cheap, no LLM); the commit only applies it when the org's
		// turn telemetry is on and the map is non-empty.
		dispatchToolUseToPromptIndex: deriveDispatchPromptIndex(conv),
	}
	// The shared spine blocks (turn-grouped body, measured telemetry, author, capabilities)
	// are built from sp itself, never from a fresh
	// conv.GetPrompts() re-parse, so every producer's content and the measured DB row derive
	// from the identical parsed turns. The LLM-input payload is then a send-transform of that
	// rendering: file paths are hashed keeping extension + depth,
	// so identifiers do not reach the model. Storage is unchanged — session_turn_analyses.files_touched
	// keeps the raw paths. See anonymizePaths for the scope.
	content, turnIndexMap := buildSpineContent(record.OrganizationID, record.ID, sp)
	llmContent := anonymizePaths(content)
	sp.llmContent = llmContent
	sp.turnIndexMap = turnIndexMap
	sp.chunked = len([]rune(llmContent)) > p.cfg.ChunkCharLimit()

	readHash := ReadInputHash(contentSha256, p.cfg.ContractVersionRead())
	if spineOnlyReprocess(prior, readHash, p.cfg.ReadEnabled(record.OrganizationID),
		p.cfg.ContractVersionEvidence(), p.cfg.ContractVersionDiscard()) {
		// Carry the prior generation's call_state unchanged — no producer re-runs.
		sp.spineOnly = true
		sp.callState = prior.CallState
		sp.vocabularyVersion = prior.VocabularyVersion
		sp.finalize()
		// The prior generation's disposition is reconstructed from its carried
		// flag-skipped mask (the state row does not store disposition — that lives on
		// the content row): a dark producer means some judged columns are legitimately
		// NULL, so the row is PARTIAL, else PRODUCED.
		sp.disposition = dispositionFor(sp.flagSkippedProducers | sp.pendingDataProducers | sp.failedProducers)
		return sp, nil
	}

	sp.callState = &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{
		producerRead: {InputHash: readHash, ContractVersion: p.cfg.ContractVersionRead(), Status: sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED},
	}}
	return sp, nil
}

// spineOnlyReprocess reports whether the record can take the zero-LLM reprocess path: a prior
// JUDGED generation exists whose READ input hash is unchanged, whose READ actually produced a
// verdict, AND both tail producers (E, D) are settled. Keying on R's hash alone was wrong two
// ways: (1) a re-admit that only R's hash covers would skip a per-producer contract bump
// (category 4), or the re-admit of a prior generation's flag-skipped E/D (category 9), freezing
// that producer's grain; (2) the READ input hash is stamped at spine time even when R was dark and
// never ran, so a prior whose R was FLAG_SKIPPED has no judged verdict to carry — enabling R must
// force a full run. E and D carry no enable flag, so their settledness turns only on their recorded
// call state and contract version. When any producer is unsettled this returns false, so the full
// producer flow runs and backfills the affected grain.
func spineOnlyReprocess(prior *model.SessionAnalysisState, readHash string, readEnabled bool, evidenceContract, discardContract int32) bool {
	if prior == nil || prior.Status != sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED {
		return false
	}
	if prior.CallState == nil {
		return false
	}
	call, ok := prior.CallState.GetCalls()[producerRead]
	if !ok || call.GetInputHash() != readHash {
		return false
	}
	// R produced no verdict in the prior generation if it was flag-skipped there; with R now
	// enabled, the spine-only re-fold would leave the row's judged verdict permanently NULL
	// (UpsertMeasured never touches judged_at), so force a full run instead.
	if readEnabled && call.GetStatus() == sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED {
		return false
	}
	return producerSettled(prior, producerEvidence, evidenceContract) &&
		producerSettled(prior, producerDiscard, discardContract)
}

// producerSettled reports whether a producer needs no re-run on this pass: it has a recorded call
// state that is not flag-skipped-awaiting-enablement, not awaiting pending data, and ran at (or
// above) the current contract version. A producer with no recorded state, a flag-skipped one whose
// gate is now on (category 9 re-admit), a SKIPPED_PENDING_DATA one, or one whose contract is behind
// (category 4) is UNSETTLED and forces a full run.
func producerSettled(prior *model.SessionAnalysisState, producer string, currentContract int32) bool {
	call, ok := prior.CallState.GetCalls()[producer]
	if !ok {
		return false
	}
	if call.GetStatus() == sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED {
		return false
	}
	// A SKIPPED_PENDING_DATA producer is UNSETTLED: its input dependency has not resolved, so
	// the record must take the full producer flow, where runProducers' re-pick decides whether
	// the data has now landed. Treating it as settled would
	// take the spine-only path and strand the pending half forever.
	if call.GetStatus() == sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA {
		return false
	}
	// A FAILED producer is UNSETTLED: R committed but this producer's inference errored, so the
	// failed-producer re-admission leg must re-run it (carrying R) rather than treat it as done
	//. Treating it as settled would strand a transient E/D fault.
	if call.GetStatus() == sessionintelligencepb.CallStatus_CALL_STATUS_FAILED {
		return false
	}
	return call.GetContractVersion() >= currentContract
}

// setCallStatus stamps one producer's call status AND the contract version it ran (or was
// evaluated) against, tolerating a nil map. Storing the per-producer contract_version is what
// lets a later contract bump to E or D re-run exactly that producer:
// without it the spine-only decision keyed on R alone, freezing E/D at their old contract.
// input_hash is left
// as-is (R's is set at spine time; E/D re-runs are driven by R's content hash + their own
// contract, so they need no separate content digest here).
func (sp *spineResult) setCallStatus(key string, status sessionintelligencepb.CallStatus, contractVersion int32) {
	if sp.callState == nil {
		sp.callState = &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{}}
	}
	if sp.callState.Calls == nil {
		sp.callState.Calls = map[string]*sessionintelligencepb.CallState{}
	}
	call, ok := sp.callState.GetCalls()[key]
	if !ok {
		call = &sessionintelligencepb.CallState{}
		sp.callState.Calls[key] = call
	}
	call.Status = status
	call.ContractVersion = contractVersion
	sp.finalize()
}

// finalize recomputes flag_skipped_producers from call_state (bit set iff that
// producer is FLAG_SKIPPED) — recomputed, not clear-on-write, so a producer
// re-evaluated under a now-on flag clears its bit whether it fired or legitimately
// did not.
func (sp *spineResult) finalize() {
	if sp.callState == nil {
		sp.flagSkippedProducers = 0
		sp.pendingDataProducers = 0
		sp.failedProducers = 0
		return
	}
	var flagMask, pendingMask, failedMask int32
	for key, call := range sp.callState.GetCalls() {
		bit := producerBit(key)
		if bit == 0 {
			continue
		}
		switch call.GetStatus() {
		case sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED:
			flagMask |= bit
		case sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA:
			pendingMask |= bit
		case sessionintelligencepb.CallStatus_CALL_STATUS_FAILED:
			failedMask |= bit
		default:
			// UNSPECIFIED / NOT_FIRED / DONE / SKIPPED set no mask.
		}
	}
	sp.flagSkippedProducers = flagMask
	sp.pendingDataProducers = pendingMask
	sp.failedProducers = failedMask
}

// producerBit maps a CallStateMap key to its flag_skipped_producers / pending_data_producers
// bit via the producerBits registry; 0 for a key that is not a producer. callState only ever
// holds producer keys (setCallStatus is called with the producer constants), so 0 is
// unreachable for real data — it exists so finalize can skip a non-producer key defensively.
func producerBit(key string) int32 {
	return producerBits[key]
}

// setPendingData marks a producer CALL_STATUS_SKIPPED_PENDING_DATA and persists the exact
// unresolved artifact ids plus the spine's body digest into its CallState, so a later re-pick
// reads only those ids (no S3) and abandons the snapshot when the body digest changes
// . finalize then sets the producer's
// pending_data_producers bit. contractVersion is stamped as for any call so a contract bump
// still re-runs the producer. It is the seam E (the E-assembly slice) calls once it finds its
// artifact intent unresolvable; the re-pick decision (evidenceRePickDecision) consumes it.
func (sp *spineResult) setPendingData(key string, contractVersion int32, artifactIDs []string, sourceContentSha256 string) {
	sp.setCallStatus(key, sessionintelligencepb.CallStatus_CALL_STATUS_SKIPPED_PENDING_DATA, contractVersion)
	if call, ok := sp.callState.GetCalls()[key]; ok {
		call.PendingArtifactIds = artifactIDs
		call.SourceContentSha256 = sourceContentSha256
	}
	// setCallStatus already recomputed the mask via finalize(); the two field writes above are
	// not finalize inputs, so no second recompute is needed.
}

// enabledProducerBits is the flag_skipped_producers mask of the producers ENABLED for this org
// right now — R, E and D all ride the master SessionAnalysisEnabled gate (they carry no separate
// per-producer flag). The actionable scan's category-9 leg intersects it with a row's
// flag_skipped_producers so a JUDGED/SPINE_DONE row re-admits only when a producer it flag-skipped
// is now enabled — a permanently-dark producer's bit is masked out and the row never churns
// re-admission. The master gate is the single early return: a
// master-off org's mask is 0, the category-9 intersection is always zero, and the scan re-admits
// nothing — sparing the spine/S3 re-read while the org stays dark.
func enabledProducerBits(cfg Config, orgID string) int32 {
	if !cfg.ReadEnabled(orgID) {
		return 0
	}
	// R and D ride the master gate; E has its own per-org gate (EvidenceEnabled), so its bit is
	// set only when that flag is on. A master-on/evidence-off org therefore keeps the EVIDENCE
	// bit clear: the EVIDENCE call is FLAG_SKIPPED and the category-9 leg re-admits the row to
	// run E once the flag turns on, without re-admitting it while E stays dark.
	bits := bitRead | bitDiscard
	if cfg.EvidenceEnabled(orgID) {
		bits |= bitEvidence
	}
	return bits
}

// dispositionFor maps a producer mask to the judged disposition: any dark (flag-skipped) OR
// pending (SKIPPED_PENDING_DATA) producer makes the row PARTIAL — some judged columns are
// legitimately NULL because that producer has not committed its grain — else PRODUCED. Callers
// pass flagSkippedProducers | pendingDataProducers.
func dispositionFor(producerMask int32) sessionintelligencepb.JudgedDisposition {
	if producerMask != 0 {
		return sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PARTIAL
	}
	return sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED
}

func childrenSeqOf(prior *model.SessionAnalysisState) int32 {
	if prior == nil {
		return 0
	}
	return prior.ChildrenSeq
}

// foldedChildrenSeqOf is the fold point to hold when this pass does not (re)fold — the prior
// generation's folded_children_seq, or 0 for a first-time row. Holding it (rather than advancing
// to the current children_seq) keeps children_seq > folded_children_seq so the child-refold scan
// leg re-admits the parent to retry a failed fold.
func foldedChildrenSeqOf(prior *model.SessionAnalysisState) int32 {
	if prior == nil {
		return 0
	}
	return prior.FoldedChildrenSeq
}

// perTurnTokenCosts returns the measured input+output token cost per turn, one entry
// per user prompt (its own tokens plus its immediately-following assistant response's).
// The slice is indexed by the turn's prompt_index — the human turn ordinal R tags — so
// the commit can sum it over D's discarded prompt-index ranges. This is the same
// user/assistant pairing the session waste extractor (ApplyDiscardedPairWaste) sums.
func perTurnTokenCosts(prompts []*sessionintelligencepb.Prompt) []int64 {
	costs := make([]int64, 0, len(prompts)/2+1)
	for i := 0; i < len(prompts); i++ {
		if prompts[i].GetRole() != sessionintelligencepb.Role_ROLE_USER {
			continue
		}
		cost := prompts[i].GetMetadata().GetInputTokens() + prompts[i].GetMetadata().GetOutputTokens()
		if i+1 < len(prompts) && prompts[i+1].GetRole() == sessionintelligencepb.Role_ROLE_ASSISTANT {
			cost += prompts[i+1].GetMetadata().GetInputTokens() + prompts[i+1].GetMetadata().GetOutputTokens()
		}
		costs = append(costs, cost)
	}
	return costs
}

// perTurnHitlGates returns the measured HITL gate active at each human turn, one entry per user
// prompt, index-aligned with perTurnTokenCosts (both keyed by prompt_index). The source stamps the
// mode only where it changes (Claude Code's permissionMode is sparse), so this forward-fills the
// last non-empty raw mode over the unstamped turns and normalizes it via hitlGateFromApprovalMode.
// An entry stays nil until the first stamped mode, and every entry is nil for a source that carries
// no per-turn mode (empty permission_mode throughout). This is the position-aware placement the
// phase commit reads at a phase's RangeStart, never a single session-level posture.
func perTurnHitlGates(prompts []*sessionintelligencepb.Prompt) []*sessionintelligencepb.HitlGate {
	gates := make([]*sessionintelligencepb.HitlGate, 0, len(prompts)/2+1)
	var lastRawMode string
	for i := 0; i < len(prompts); i++ {
		if prompts[i].GetRole() != sessionintelligencepb.Role_ROLE_USER {
			continue
		}
		if mode := prompts[i].GetMetadata().GetPermissionMode(); mode != "" {
			lastRawMode = mode
		}
		gates = append(gates, hitlGateFromApprovalMode(lastRawMode))
	}
	return gates
}

// conversationStart resolves the conversation's start instant, falling back to the
// record's data_date when the parsed body carries none.
func conversationStart(conv *sessionintelligencepb.Conversation, fallback time.Time) time.Time {
	prompts := conv.GetPrompts()
	if len(prompts) > 0 {
		if ts := prompts[0].GetTimestamp(); ts != nil {
			return ts.AsTime()
		}
	}
	return fallback
}
