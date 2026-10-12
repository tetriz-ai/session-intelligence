// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

// DAO interfaces the engine depends on.
// The three input-side interfaces (raw records, harness artifacts) are trimmed to
// the methods the engine calls; a host supplies a reference reader for them. The
// session and security interfaces are the full contracts the shipped repos implement.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	coreaidlcv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/aidlc/v1"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// IRawRecordDAO is the input source of session raw records (trimmed to engine use).
type IRawRecordDAO interface {
	StampProcessingStatusOnly(ctx context.Context, id, orgID string, dataDate time.Time, status model.ProcessingStatus) error
	GetByIDForDate(ctx context.Context, id string, dataDate time.Time) (*model.RawRecordModel, error)
	CurrentContentDigest(ctx context.Context, id string, dataDate time.Time) (string, error)
	CurrentSourceUpdatedAt(ctx context.Context, id string, dataDate time.Time) (time.Time, error)
	GetByRecordKey(ctx context.Context, orgID, connectorID, recordKey string) (*model.RawRecordModel, error)
	UpdateMetadata(ctx context.Context, id string, metadata json.RawMessage) error
	AggregateSubagentUsageByParent(ctx context.Context, orgID, personID, parentRecordKey string, parentDataDate time.Time) ([]SubagentUsageRow, error)
}

// IHarnessArtifactDAO resolves invoked-artifact slugs to live harness artifacts (trimmed).
type IHarnessArtifactDAO interface {
	ListLiveForSource(ctx context.Context, orgID string, ideType commonv1.IdeType, artifactType commonv1.HarnessArtifactType, artifactSource commonv1.HarnessArtifactSource, personID *string) ([]*model.HarnessArtifact, error)
}

// IHarnessArtifactClassificationDAO resolves an artifact's intent classification (trimmed).
type IHarnessArtifactClassificationDAO interface {
	GetByArtifact(ctx context.Context, orgID string, task coreaidlcv1.HarnessClassificationTask, artifactID string) (*model.HarnessArtifactClassification, error)
}

// SubagentUsageRow is one parent record's aggregated delegated sub-agent usage.
type SubagentUsageRow struct {
	AgentType       string
	PluginName      string
	Tokens          int64
	CostUsdMicros   int64
	Count           int32
	LatestUpdatedAt time.Time
}

// ISessionAnalysisDAO persists and reads session analyses.
type ISessionAnalysisDAO interface {
	// Create inserts one session-analysis row.
	Create(ctx context.Context, analysis *model.SessionAnalysis) error
	// UpsertJudged writes the session content row on the live-generation partial
	// unique (org, raw_record_id) WHERE superseded_at IS NULL — updating a re-judge
	// in place. evidenceRan is true iff E (the evidence pass) fired on this pass; when
	// false, the prior live row's E-owned skill_assessments/artifact_adjudications are
	// carried forward onto analysis before the write, so an R-only re-judge (E did not
	// re-run) never NULLs out a prior generation's valid E output — analysis is expected
	// nil in those columns in that case. Runs inside the caller's commit transaction.
	UpsertJudged(ctx context.Context, analysis *model.SessionAnalysis, evidenceRan bool) error
	// UpsertMeasured refreshes ONLY the measured columns of the live generation (a
	// spine-only reprocess), leaving the judged verdict intact.
	UpsertMeasured(ctx context.Context, analysis *model.SessionAnalysis) error
	UpsertMeasuredReaffirmingJudged(ctx context.Context, analysis *model.SessionAnalysis) error
	// SupersedeContent marks the live content generation SUPERSEDED so a fresh
	// generation at a new vocabulary_version can coexist (a vocabulary bump).
	SupersedeContent(ctx context.Context, rawRecordID string) (bool, error)
	// ClearServedVerdict clears judged_at on the live content row so a content-change
	// reopen drops the stale verdict out of serving until the re-run re-judges it (a
	// failed re-run then serves nothing rather than the stale verdict). Returns whether
	// a live judged row was cleared.
	ClearServedVerdict(ctx context.Context, rawRecordID string) (bool, error)
	// GetByRawRecordID returns the live (non-superseded) analysis, or ErrNotFound.
	GetByRawRecordID(ctx context.Context, rawRecordID string) (*model.SessionAnalysis, error)
	// MarkJudgeFailed stamps judged_at + JUDGED_DISPOSITION_JUDGE_FAILED on the live
	// generation, but ONLY when judged_at is still NULL — a natural no-op before
	// spine_done (no measured row exists yet) and for a row already carrying a valid
	// prior judged verdict (PRODUCED/PARTIAL/NOT_APPLICABLE/JUDGE_FAILED all set
	// judged_at, so a later terminal R failure never overwrites an earlier judged
	// generation's verdict). The measured columns already written at spine_done then
	// serve through the ordinary NULL-tolerant path; every judged column stays NULL.
	// Returns whether a row was stamped.
	MarkJudgeFailed(ctx context.Context, rawRecordID string) (bool, error)
}

// ISessionAnalysisStateDAO persists and reads the per-record processing state.
type ISessionAnalysisStateDAO interface {
	// Create inserts one state row.
	Create(ctx context.Context, state *model.SessionAnalysisState) error
	// GetByRawRecordID returns the state row for a raw record, or ErrNotFound.
	GetByRawRecordID(ctx context.Context, rawRecordID string) (*model.SessionAnalysisState, error)
	// ScanLiveActionable returns the LIVE-lane actionable candidates (categories
	// 1/2/3/5/6), freshest first.
	ScanLiveActionable(ctx context.Context, params SessionAnalysisScanParams, limit int) ([]*ActionableCandidate, error)
	// ScanBackfillActionable returns the BACKFILL-lane candidates (categories
	// 4/7/8/9), freshest first, always yielding to LIVE.
	ScanBackfillActionable(ctx context.Context, params SessionAnalysisScanParams, limit int) ([]*ActionableCandidate, error)
	// ClaimForProcessing inserts a fresh PENDING row for a never-analysed raw record
	// via INSERT ... ON CONFLICT DO NOTHING, returning whether this call won the claim
	// (the double-claim backstop).
	ClaimForProcessing(ctx context.Context, state *model.SessionAnalysisState) (bool, error)
	// Reclaim re-takes the lease on an existing row (retryable failure / content
	// change / lease-expired), rotating claim_token; CAS-gated on expectedClaimToken
	// (NULL-safe) so two ticks racing one row cannot both win. Returns whether it was
	// re-claimed.
	Reclaim(ctx context.Context, rawRecordID string, expectedClaimToken *string, resetAttempts bool) (bool, error)
	// TransitionToSpineDone stamps the measured-pass anchors and moves PENDING →
	// SPINE_DONE, CAS-gated on claim_token.
	TransitionToSpineDone(ctx context.Context, rawRecordID, claimToken, sourceContentSha256 string, sourceUpdatedAt time.Time, spineVersion int32, conversationStartedAt time.Time) (bool, error)
	// TransitionToJudged is the state half of the judged commit — CAS-gated on claim_token (the
	// lease) ONLY, never on children_seq (the durable staged commit — the producers
	// design): a valid R verdict always reaches JUDGED. foldedChildrenSeq is written to
	// folded_children_seq (what this pass folded); a later child (children_seq > folded_children_seq)
	// re-admits the JUDGED parent via the child-refold leg to fold the delta, carrying R.
	TransitionToJudged(ctx context.Context, rawRecordID, claimToken string, foldedChildrenSeq int32, commit JudgedCommit) (bool, error)
	// TransitionToFailed records a failed attempt; incrementAttempt is false under an
	// active provider-wide outage. It also advances the staleness anchor to the attempted body
	// (sourceContentDigest = the current payload content_sha256, NULL when empty; sourceUpdatedAt =
	// its current updated_at) so a persistently-failing changed body is bounded by the retry cap
	// rather than re-admitted forever by the content-changed scan leg. Returns the row's
	// attempt_count after the transition so a caller can gate terminal-only work on
	// attempt_count >= RetryCap.
	TransitionToFailed(ctx context.Context, rawRecordID, claimToken, lastError string, incrementAttempt bool, sourceContentDigest string, sourceUpdatedAt time.Time) (bool, int32, error)
	// MarkSkipped writes a conditional SKIPPED row stamped with the firing gate and its
	// re-admission key, returning whether it INSERTED a new row (vs re-marking an existing skip) so
	// the caller bumps the parent child-fence exactly once per genuine first child sighting.
	MarkSkipped(ctx context.Context, state *model.SessionAnalysisState, reason sessionintelligencepb.SessionAnalysisSkipReason) (bool, error)
	RefreshChildContentAnchor(ctx context.Context, rawRecordID string, sourceContentSha256 *string, sourceUpdatedAt time.Time) error
	// BumpChildrenSeq increments the parent's child fence on first child sighting.
	BumpChildrenSeq(ctx context.Context, parentRawRecordID string) (bool, error)
	// Supersede marks a specific old generation SUPERSEDED, CAS-targeting its exact
	// (raw_record_id, vocabulary_version).
	Supersede(ctx context.Context, rawRecordID string, oldVocabularyVersion int32) (bool, error)
}

// ISessionTurnAnalysisDAO persists and reads per-turn analyses.
type ISessionTurnAnalysisDAO interface {
	// Create inserts one turn row.
	Create(ctx context.Context, turn *model.SessionTurnAnalysis) error
	// ListByRawRecordID returns a raw record's turns, ordered by prompt index. Keyed
	// on the seat-scoped parent's key, never a bare organization_id.
	ListByRawRecordID(ctx context.Context, rawRecordID string) ([]*model.SessionTurnAnalysis, error)
	// UpdateJudged overlays this engine's judged half onto the live measured turn row
	// the grouper already created, keyed on (org, raw_record_id, prompt_index) and
	// touching only the judged columns. UPDATE-only — never inserts; returns the rows
	// affected, 0 meaning no live measured row matched (the caller requeues rather than
	// land a judged-only row). Runs inside the caller's commit transaction.
	UpdateJudged(ctx context.Context, turn *model.SessionTurnAnalysis) (int64, error)
	// SoftDeleteJudgedBeyond retires the trailing turns a shorter re-analysis dropped
	// (prompt_index > maxPromptIndex).
	SoftDeleteJudgedBeyond(ctx context.Context, orgID, rawRecordID string, maxPromptIndex int32) (int64, error)
	// UpsertMeasured writes the measured half of one turn as a column-scoped upsert on
	// (org, raw_record_id, prompt_index), preserving the peer judged writer's columns.
	// deleted_at is in the set, so a conflict onto a retired index revives the row.
	UpsertMeasured(ctx context.Context, turn *model.SessionTurnAnalysis) error
	// SoftDeleteMeasuredBeyond retires the trailing turns a shorter re-grouping dropped
	// (prompt_index > maxPromptIndex). The measured grouper owns tail management.
	SoftDeleteMeasuredBeyond(ctx context.Context, orgID, rawRecordID string, maxPromptIndex int32) (int64, error)
	// BackfillChildParentPromptIndex stamps parent_prompt_index on a parent conversation's
	// sub-agent child rows, keyed by (org, parent_conversation_id, parent_tool_use_id).
	BackfillChildParentPromptIndex(ctx context.Context, orgID, parentConversationID string, toolUseIDToPromptIndex map[string]int32) (int64, error)
}

// ISessionPhaseAnalysisDAO persists and reads per-phase analyses.
type ISessionPhaseAnalysisDAO interface {
	// Create inserts one phase row.
	Create(ctx context.Context, phase *model.SessionPhaseAnalysis) error
	// ListBySessionAnalysisID returns an analysis's phases, ordered by phase ordinal.
	ListBySessionAnalysisID(ctx context.Context, sessionAnalysisID string) ([]*model.SessionPhaseAnalysis, error)
	// ReplacePhaseSet upserts a session's full phase set by phase_ordinal (preserving
	// each row's id) and soft-deletes the trailing phases dropped, returning the
	// phase_ordinal → phase id map for the caller's turn re-point. FK-safe. Runs inside
	// the caller's commit transaction.
	ReplacePhaseSet(ctx context.Context, orgID, sessionAnalysisID string, phases []*model.SessionPhaseAnalysis) (map[int32]string, error)
}

// ISessionRoundTripFaultDAO persists and reads round-trip fault rows.
type ISessionRoundTripFaultDAO interface {
	// ReplaceSet soft-deletes every live fault for the session_analysis_id, then inserts
	// the given set — an E re-run replaces the fault set, never appends. An empty set clears
	// the session's faults. Runs inside the caller's commit transaction.
	ReplaceSet(ctx context.Context, orgID, sessionAnalysisID string, faults []*model.SessionRoundTripFault) error
	// ListBySessionAnalysisID returns a session's round-trip faults, ordered by prompt index.
	// Keyed on the seat-scoped parent's id, never a bare organization_id.
	ListBySessionAnalysisID(ctx context.Context, sessionAnalysisID string) ([]*model.SessionRoundTripFault, error)
}

// ISecurityFlagDAO persists and reads security flags.
type ISecurityFlagDAO interface {
	// CreateBatch inserts a batch of security flags.
	CreateBatch(ctx context.Context, flags []*model.SecurityFlag) error

	// ListByOrg returns the most recent security flags for an org.
	ListByOrg(ctx context.Context, orgID string, limit int) ([]*model.SecurityFlag, error)

	// ListByPerson returns security flags for a specific person in an org.
	ListByPerson(ctx context.Context, orgID, personID string, limit int) ([]*model.SecurityFlag, error)

	// CountOpenByOrg returns the count of non-resolved flags for an org.
	CountOpenByOrg(ctx context.Context, orgID string) (int64, error)

	// GetByID returns a single security flag by ID, or a wrapped gorm.ErrRecordNotFound
	// (errors.Is-matchable) when no row exists.
	GetByID(ctx context.Context, id string) (*model.SecurityFlag, error)

	// DeleteByRawRecordIDAndStatus soft-deletes all non-deleted flags for the given
	// raw_record_id that match the given status.
	DeleteByRawRecordIDAndStatus(ctx context.Context, rawRecordID string, status string) error

	// ListOpenByRawRecordID returns the raw record's live open flags. ReconcileOpenFlags
	// is the sole caller today; exported for direct inspection in tests.
	ListOpenByRawRecordID(ctx context.Context, rawRecordID string) ([]*model.SecurityFlag, error)

	// ReconcileOpenFlags atomically replaces a raw record's OPEN security flags with
	// freshFlags — see the method doc on SecurityFlagRepo for the replace-set contract;
	// the last writer for a raw_record wins, with no cross-writer flag preservation.
	ReconcileOpenFlags(ctx context.Context, rawRecordID string, freshFlags []*model.SecurityFlag) error
}
