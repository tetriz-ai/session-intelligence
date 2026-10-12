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
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
	"github.com/tetriz-ai/session-intelligence/security"
)

// The single-session front door. ProcessFlat (the batch path) loads a raw_record from Postgres,
// reads the transcript body from S3, claims a lease, runs the spine + R/E/D producers, and commits
// the result through the DAO under a per-org Config and a state machine. AnalyzeConversation is its
// SIBLING over the same authored helpers: it takes ONE in-memory conversation plus the R/E/D
// Provider (and an optional round-trip detector), runs the identical spine → R → E → D pipeline, and
// returns the BUILT rows WITHOUT touching Postgres, S3, the lease, the per-org Config, or any
// re-pick / pending-data / child-fold machinery (a single conversation has no prior generation and
// no delegated children). It changes none of the batch path — the batch real-Gemini E2E guards that.
//
// The public analyze package wraps this behind public types; this stays in sessionanalysis because
// it reuses the package's unexported spine/producer/build helpers directly.

// singleSessionVersion is the version stamp written onto a single-session run's built rows (spine,
// vocabulary and per-producer contract). It has no effect on a standalone run beyond provenance (the
// rows are never persisted and there is no prior generation to compare against), so it is fixed at 1
// rather than configurable.
const singleSessionVersion int32 = 1

// SingleSessionCharLimit is the maximum rendered-transcript size (in runes) AnalyzeConversation sends
// in one read-pass call. The single-session path does NOT window the read the way the batch path does,
// so a transcript whose rendered content exceeds this is rejected with ErrTranscriptTooLarge rather
// than dispatched to fail opaquely. ~600k runes is roughly 150k tokens — within a single Gemini call.
const SingleSessionCharLimit = 600_000

// ErrTranscriptTooLarge is returned (wrapped) when a conversation's rendered transcript exceeds the
// single-call ceiling; the batch path (which chunks) handles such sessions.
var ErrTranscriptTooLarge = errors.New("transcript too large for single-session analysis")

// ErrNoTaggableTurns is returned (wrapped) when a conversation has no taggable (human) turns to
// analyze — an empty or assistant-only transcript. The read pass is never dispatched in that case.
var ErrNoTaggableTurns = errors.New("conversation has no taggable (human) turns to analyze")

// SingleSessionConfig is the single-session counterpart to the batch Config interface: a flat value
// with single-tenant, everything-on, unlimited defaults, so a caller never implements the 31-method
// per-org Config. DefaultSingleSessionConfig supplies the defaults; a caller overrides only what it
// needs. Version stamps are not exposed — a single standalone run has no prior generation or date
// peers for them to matter to, so they are fixed (see singleSessionVersion).
type SingleSessionConfig struct {
	// RunEvidence / RunDiscard gate the conditional E and D producers. Both default on; E still only
	// fires on an artifact ledger or a detector-scored round-trip pair, and D only on revert/abandon
	// evidence, exactly as the batch path gates them.
	RunEvidence bool
	RunDiscard  bool

	// UseSystemPromptCache opts every producer call into Gemini system-prompt caching.
	UseSystemPromptCache bool

	// DetectorTopN / DetectorPairCeiling bound the optional round-trip detector per session (a
	// non-positive value means no cap / no ceiling). Unused when no detector is supplied.
	DetectorTopN        int
	DetectorPairCeiling int
}

// DefaultSingleSessionConfig returns the single-tenant, everything-on, unlimited defaults.
func DefaultSingleSessionConfig() SingleSessionConfig {
	return SingleSessionConfig{
		RunEvidence:          true,
		RunDiscard:           true,
		UseSystemPromptCache: false,
		DetectorTopN:         20,
		DetectorPairCeiling:  1000,
	}
}

// SingleSessionInput carries the identifiers the built rows need. OrgID/PersonID/SessionID stamp the
// rows; SessionID also binds the per-trace Langfuse session grouping (it is the raw_record_id the
// batch path uses). ContentSha256/ConversationID/RepoName are optional provenance.
type SingleSessionInput struct {
	OrgID          string
	PersonID       string
	SessionID      string
	RepoName       string
	ConversationID string
	ContentSha256  string
}

// SingleSessionResult is the built, un-persisted analysis for one conversation: the session-grain
// row, the measured turn rows, the RECONCILED session-level security flags (same masking + dedup the
// batch path applies, built via security.BuildOpenFlags), and R's (E-enriched) ReadResult plus D's
// DiscardResult. The public analyze package translates these into its clean Analysis struct; nothing
// here is written to a DB.
type SingleSessionResult struct {
	Analysis      *model.SessionAnalysis
	MeasuredTurns []*model.SessionTurnAnalysis
	SecurityFlags []*model.SecurityFlag
	Read          *ReadResult
	Discard       *DiscardResult
}

// AnalyzeConversation runs the spine → R → E → D pipeline over one in-memory conversation and
// returns the built rows. producer is required (R always runs); detector is optional (nil disables
// the round-trip half's candidate scoring).
//
// It is all-or-nothing: an E or D pass error fails the whole call (R's completed verdict is not
// returned), because a single-session caller gets one Analysis or an error, not a partial persisted
// state. It rejects an empty/assistant-only transcript (ErrNoTaggableTurns) and a transcript larger
// than the single-call ceiling (ErrTranscriptTooLarge) BEFORE any provider call.
func AnalyzeConversation(
	ctx context.Context,
	input SingleSessionInput,
	conv *sessionintelligencepb.Conversation,
	producer LLMProducer,
	detector RoundTripDetector,
	cfg SingleSessionConfig,
) (*SingleSessionResult, error) {
	if conv == nil {
		return nil, fmt.Errorf("analyze conversation: conversation is nil")
	}
	if producer == nil {
		return nil, fmt.Errorf("analyze conversation (session %s): producer is nil", input.SessionID)
	}

	sp := buildSpineForConversation(ctx, input, conv)

	// Reject before dispatching any paid call: an empty/assistant-only transcript has no taggable
	// turn to judge (and the coverage check would otherwise fail opaquely after a wasted round trip).
	if len(sp.turnIndexMap) == 0 {
		return nil, fmt.Errorf("analyze conversation (session %s): %w", input.SessionID, ErrNoTaggableTurns)
	}
	// Reject a transcript larger than the single-call ceiling with a clear typed error rather than
	// dispatching it to fail opaquely — the single-session path does not window the read.
	if renderedChars := len([]rune(sp.llmContent)); renderedChars > SingleSessionCharLimit {
		return nil, fmt.Errorf("analyze conversation (session %s): %d turns / %d chars exceeds the %d-char single-call limit; the batch path chunks: %w",
			input.SessionID, sp.turnCount, renderedChars, SingleSessionCharLimit, ErrTranscriptTooLarge)
	}

	req := ProducerRequest{
		OrgID:                input.OrgID,
		PersonID:             input.PersonID,
		SessionID:            input.SessionID,
		Content:              sp.llmContent,
		UseSystemPromptCache: cfg.UseSystemPromptCache,
		Tags:                 producerTags("analyze_read", input.OrgID, sp.vocabularyVersion),
	}

	// R — the merged read pass. The single-session path sends the whole transcript in one call
	// (sp.chunked is forced false in the spine builder), so this is the batch path's single-chunk
	// branch via the shared helper: infer + rebase, then enforce the one-verdict-per-supplied-turn
	// contract via the same shared coverage check the batch path uses.
	read, err := runSingleChunkRead(ctx, producer, sp, req)
	if err != nil {
		// The producer already wraps with "analyze_read (session …)"; don't double-wrap.
		return nil, err
	}
	if err := enforceReadTurnCoverage(ctx, read, sp.turnIndexMap, input.OrgID, fmt.Sprintf("session %s", input.SessionID)); err != nil {
		return nil, fmt.Errorf("read pass coverage (session %s): %w", input.SessionID, err)
	}
	read.Phases = clampPhaseRangesToDomain(read.Phases, int32(len(sp.promptTokenCosts)))
	sp.setCallStatus(producerRead, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, singleSessionVersion)

	// The optional round-trip detector scores the spine's guard-valid pairs; its above-threshold
	// top-N feed E's round-trip half, exactly as in the batch path. Gated by RunEvidence since the
	// detector produces E's input.
	if cfg.RunEvidence && detector != nil {
		pairs, drops := enumerateGuardValidPairs(input.OrgID, input.SessionID, sp)
		recordGuardDrops(input.OrgID, drops)
		detected, detectErr := detector.Detect(ctx, input.OrgID, input.SessionID, toPublicRoundTripPairs(pairs), cfg.DetectorTopN, cfg.DetectorPairCeiling)
		if detectErr != nil {
			return nil, fmt.Errorf("detect round trips (session %s): %w", input.SessionID, detectErr)
		}
		if detected != nil {
			sp.roundTripCandidates = detected.Candidates
			sp.detectorTruncated = detected.Truncated
		}
	}

	// E — the evidence pass, conditional on a non-empty artifact ledger OR detector-scored round-trip
	// pairs, mirroring the batch path's non-pending branch. The single-session path has no harness
	// inventory to resolve artifact intent against, so E runs with intent unresolved (nil map): its
	// round-trip half and its artifact adjudication still produce, identified by slug rather than by
	// classified intent.
	if cfg.RunEvidence && (sp.ledgers.firesEvidence(read) || len(sp.roundTripCandidates) > 0) {
		evidenceReq := req
		evidenceReq.Tags = producerTags("analyze_evidence", input.OrgID, sp.vocabularyVersion)
		evidenceReq.Content = buildEvidenceContent(input.OrgID, input.SessionID, sp, false, nil)
		evidence, evidenceErr := producer.AnalyzeEvidence(ctx, evidenceReq)
		if evidenceErr != nil {
			return nil, evidenceErr // producer already wraps with "analyze_evidence (session …)"
		}
		applyEvidence(read, evidence)
		sp.setCallStatus(producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, singleSessionVersion)
	} else {
		sp.setCallStatus(producerEvidence, sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, singleSessionVersion)
	}

	// D — the discard tail, conditional on the spine's revert/abandon ledger. Its ranges are rebased
	// to raw whole-session indices for the code-summed waste ledger, exactly as the batch path does.
	var discard *DiscardResult
	if cfg.RunDiscard && sp.ledgers.firesDiscard() {
		discardReq := req
		discardReq.Tags = producerTags("analyze_discard", input.OrgID, sp.vocabularyVersion)
		discardReq.Content = buildDiscardContent(sp, read)
		discardResult, discardErr := producer.AnalyzeDiscard(ctx, discardReq)
		if discardErr != nil {
			return nil, discardErr // producer already wraps with "analyze_discard (session …)"
		}
		discard = discardResult
		rebaseDiscardToRawIndex(discard, sp.turnIndexMap)
		sp.setCallStatus(producerDiscard, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, singleSessionVersion)
	} else {
		sp.setCallStatus(producerDiscard, sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, singleSessionVersion)
	}

	sp.disposition = dispositionFor(sp.flagSkippedProducers | sp.pendingDataProducers | sp.failedProducers)

	// Build the rows with the same builders the batch commit uses. The record is a lightweight
	// in-memory stand-in — buildSessionAnalysisRow and buildMeasuredTurnRows read only its ID (used
	// as raw_record_id); everything else comes from the spine result.
	record := &model.RawRecordModel{ID: input.SessionID, OrganizationID: input.OrgID}
	var measuredTurns []*model.SessionTurnAnalysis
	if shouldBuildMeasuredTurns(sp) {
		measuredTurns = buildMeasuredTurnRows(input.OrgID, record.ID, sp)
	}
	analysis := buildSessionAnalysisRow(input.OrgID, record, input.PersonID, sp, read, discard, true, measuredTurns)

	// The session-level security flags go through the SAME reconcile the batch commit uses
	// (security.BuildOpenFlags): it masks each note (logredact.Redact), dedups to one flag per type
	// at the highest severity seen, defaults an unspecified severity to medium, and keeps only the
	// three reconciled slugs. publishEvidence is true for the single-session everything-on default.
	securityFlags := security.BuildOpenFlags(
		input.OrgID, input.PersonID, input.SessionID, time.Now().UTC(),
		read.SecurityIssues, read.EvidenceSecurityIssues, true,
	)

	return &SingleSessionResult{
		Analysis:      analysis,
		MeasuredTurns: measuredTurns,
		SecurityFlags: securityFlags,
		Read:          read,
		Discard:       discard,
	}, nil
}

// buildSpineForConversation builds the spine result for ONE in-memory conversation: the pure part of
// the batch spine() (parse is already done by the caller) with no S3 read, no prior generation (so
// never spine-only), and no delegated-child fold (own cost is anchored exactly as foldChildUsage
// does for a childless parent). The rendered transcript is always sent as one chunk.
func buildSpineForConversation(ctx context.Context, input SingleSessionInput, conv *sessionintelligencepb.Conversation) *spineResult {
	conversationStartedAt := conversationStart(conv, conv.GetConversationStartedAt().AsTime())
	ideType := conv.GetIdeType()
	metrics := sessionintelligence.ExtractMetrics(conv.GetPrompts(), ideType)
	usage := buildMeasuredUsage(ctx, input.OrgID, input.SessionID, conv, metrics, input.RepoName)
	// Anchor own cost as foldChildUsage does for a parent with no children: own == estimated, so a
	// re-fold would be a no-op. No child aggregation (no DAO) in the single-session path.
	if metrics != nil {
		metrics.OwnEstimatedCostUsd = metrics.GetEstimatedCostUsd()
	}

	lastActiveAt := conv.GetLastUpdatedAt().AsTime()
	sp := &spineResult{
		conversationStartedAt:        conversationStartedAt,
		lastActiveAt:                 lastActiveAt,
		conversationID:               input.ConversationID,
		contentSha256:                input.ContentSha256,
		sourceUpdatedAt:              lastActiveAt,
		spineVersion:                 singleSessionVersion,
		vocabularyVersion:            singleSessionVersion,
		prompts:                      conv.GetPrompts(),
		sessionEvents:                conv.GetSessionEvents(),
		disposition:                  sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED,
		metrics:                      metrics,
		usage:                        usage,
		ideType:                      ideType,
		turnCount:                    int32(len(conv.GetPrompts())),
		repoName:                     input.RepoName,
		promptTokenCosts:             perTurnTokenCosts(conv.GetPrompts()),
		perTurnHitlGate:              perTurnHitlGates(conv.GetPrompts()),
		ledgers:                      buildSpineLedgers(conv),
		dispatchToolUseToPromptIndex: deriveDispatchPromptIndex(conv),
	}
	content, turnIndexMap := buildSpineContent(input.OrgID, input.SessionID, sp)
	sp.llmContent = anonymizePaths(content)
	sp.turnIndexMap = turnIndexMap
	// Always one chunk on the single-session path: a caller analysing its own transcript sends the
	// whole rendered conversation in one call rather than windowing it (AnalyzeConversation bounds
	// the size with SingleSessionCharLimit before dispatching).
	sp.chunked = false
	if sp.contentSha256 == "" {
		sp.contentSha256 = ContentDigest([]byte(sp.llmContent))
	}
	sp.callState = &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{
		producerRead: {InputHash: ReadInputHash(sp.contentSha256, singleSessionVersion), ContractVersion: singleSessionVersion, Status: sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED},
	}}
	return sp
}

// runSingleChunkRead runs R over the whole rendered transcript in one call and rebases its turn/phase
// positions from R's rendered Sequence numbering to the raw whole-session Index. It is the shared
// single-chunk read both runReadPass (batch) and AnalyzeConversation (single-session) call, so the
// two cannot drift. The returned error is the producer's, already naming the operation and session;
// the batch caller adds raw-record context on top, the single-session caller returns it as-is.
func runSingleChunkRead(ctx context.Context, producer LLMProducer, sp *spineResult, req ProducerRequest) (*ReadResult, error) {
	read, err := producer.AnalyzeRead(ctx, req)
	if err != nil {
		return nil, err
	}
	rebaseToRawIndex(read, sp.turnIndexMap)
	return read, nil
}

// enforceReadTurnCoverage applies R's one-verdict-per-supplied-turn contract over the taggable turn
// set (turnIndexMap's values). It is the single implementation both the batch runProducers and the
// single-session path call, so the contract cannot drift between them. identifier labels the error and
// log line (the batch raw-record id, the single-session session id).
//
// An out-of-set (hallucinated) index is DROPPED — it recurs on an identical re-cut, so failing on it
// would loop a paid read to JUDGE_FAILED — while a duplicate verdict or an uncovered supplied turn is
// a hard failure. Never silently discard a security finding: when a dropped phantom carried
// security-layer issues, WARN so a systematic mis-index is visible rather than a silently-lost flag.
// On a drop the flattened security list is rebuilt from the survivors so the phantom's issue cannot
// open a flag (the reconcile reads SecurityIssues without an index check).
func enforceReadTurnCoverage(ctx context.Context, read *ReadResult, turnIndexMap []int32, orgID, identifier string) error {
	expected := make(map[int32]struct{}, len(turnIndexMap))
	for _, raw := range turnIndexMap {
		expected[raw] = struct{}{}
	}
	originalTurns := read.Turns
	validTurns := make([]*TurnJudgment, 0, len(originalTurns))
	seen := make(map[int32]struct{}, len(turnIndexMap))
	for _, turn := range originalTurns {
		if _, ok := expected[turn.PromptIndex]; !ok {
			if len(securityIssuesOf(turn.PromptIssues)) > 0 {
				logger.WarnWithCtx(ctx, "session analysis: dropping out-of-set read turn that carried security-layer issues",
					zap.String(logger.OrgID, orgID), zap.String("identifier", identifier), zap.Int32("prompt_index", turn.PromptIndex))
			}
			continue // phantom — drop it entirely, not just from the coverage tally
		}
		if _, dup := seen[turn.PromptIndex]; dup {
			return fmt.Errorf("read pass returned a duplicate turn verdict for prompt_index %d (%s): %w",
				turn.PromptIndex, identifier, errReadTurnCoverageMismatch)
		}
		seen[turn.PromptIndex] = struct{}{}
		validTurns = append(validTurns, turn)
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("read pass covered %d of %d supplied turns (%s): %w",
			len(seen), len(expected), identifier, errReadTurnCoverageMismatch)
	}
	// Replace R's turns with the phantom-free set so E, D, and the commit consume only supplied-turn
	// verdicts. Rebuild the flattened security list ONLY when a phantom was dropped (otherwise it
	// already reflects exactly these turns — a rebuild is a pointer-identity no-op); rebuilding from
	// the survivors is what stops a dropped phantom's security issue from opening a flag.
	read.Turns = validTurns
	if len(validTurns) != len(originalTurns) {
		rebuiltSecurity := make([]*sessionintelligencepb.PromptIssue, 0, len(read.SecurityIssues))
		for _, turn := range validTurns {
			rebuiltSecurity = append(rebuiltSecurity, securityIssuesOf(turn.PromptIssues)...)
		}
		read.SecurityIssues = rebuiltSecurity
	}
	return nil
}

// toPublicRoundTripPairs converts the spine's internal detector pairs to the public RoundTripPair so
// a caller's RoundTripDetector never imports the internal reference-llm client.
func toPublicRoundTripPairs(pairs []refllmclient.RoundTripPair) []RoundTripPair {
	out := make([]RoundTripPair, len(pairs))
	for i, pair := range pairs {
		out[i] = RoundTripPair{
			PromptIndex:   pair.PromptIndex,
			P:             pair.P,
			AgentResponse: pair.AgentResponse,
			Q:             pair.Q,
			Prior:         pair.Prior,
		}
	}
	return out
}
