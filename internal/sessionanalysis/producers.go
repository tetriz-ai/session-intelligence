// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

//go:generate go tool mockgen -source=producers.go -destination=mocks/mock_producers.go -package=mocks

// LLMProducer is the seam for the three authored LLM producers of the session-analysis engine
// : R (the merged read pass), E (artifact / round-trip
// evidence), and D (the discard tail). Each is one reference-llm inference — one
// assembled system prompt and one output contract. The runtime order per session is
// R, then a code reconcile, then E (conditional), then a monotone downgrade, then D
// (conditional). LLMProducer is the seam the processor calls; the concrete client
// (llm_client.go) calls the reference LLM service, and a mock stands in for the orchestration
// tests.
type LLMProducer interface {
	// AnalyzeRead runs R, the merged read pass, once per chunk window. It emits the
	// session verdict, the judged turn tags (carrying prompt_issues, the security
	// readout), and the phase rows.
	AnalyzeRead(ctx context.Context, req ProducerRequest) (*ReadResult, error)
	// AnalyzeEvidence runs E, conditional on the spine's artifact ledger or a
	// confirmed round-trip pair. Its round-trip fault feeds R's verdict as a code
	// downgrade, never a mid-R gap.
	AnalyzeEvidence(ctx context.Context, req ProducerRequest) (*EvidenceResult, error)
	// AnalyzeDiscard runs D, conditional on revert/abandon evidence, last. It applies
	// a monotone token_efficiency downgrade.
	AnalyzeDiscard(ctx context.Context, req ProducerRequest) (*DiscardResult, error)
}

// ProducerRequest is the assembled input for one producer inference. The processor
// sets Langfuse trace grouping (SessionID = the engine's session identity, the raw
// record id) and per-trace tags so the fan-out from one to N calls groups under one
// session view.
type ProducerRequest struct {
	OrgID    string
	PersonID string
	// SessionID is the Langfuse session identity — the raw_record_id — so every R/E/D
	// call for one session groups under one trace session.
	SessionID string
	// Content is the LLM-input payload the spine built: the rendered conversation transcript
	// (sessionintelligence.ConversationToText) with file paths hashed as a send-transform.
	// Read once, by R only.
	Content string
	// ChunkIndex tags a chunked read window (0 for the single-chunk common case).
	ChunkIndex int32
	// UseSystemPromptCache opts the call into Gemini system-prompt caching.
	UseSystemPromptCache bool
	// Tags are the per-trace Langfuse tags (task_type, version stamps, org/person).
	Tags map[string]string
}

// ReadResult is R's output: the session verdict, the judged turns, the phases, and
// the security-layer prompt issues the commit reconciles into security_flags. E runs after
// R and — since the ownership fix — writes its output into its OWN fields here rather
// than mutating R's verdict/turns (by design): applyEvidence fills
// SkillAssessments, RoundTripFaults, and EvidenceSecurityIssues, and the commit persists
// each into its E-owned home.
type ReadResult struct {
	Verdict        SessionVerdict
	Turns          []*TurnJudgment
	Phases         []*PhaseJudgment
	SecurityIssues []*sessionintelligencepb.PromptIssue // R's api-key-leak / db-credential-leak / pii-injection readout
	Usage          ProducerUsage
	ScorerModel    string

	// E-owned outputs, filled by applyEvidence (nil/empty when E did not run):
	// SkillAssessments is E's per-invocation adjudication, stored in the E-owned
	// session_analyses.skill_assessments column (never merged into R's invoked_skills).
	SkillAssessments *sessionintelligencepb.InvokedSkillJudgmentList
	// RoundTripFaults are E's per-pair faults, written replace-set to session_round_trip_faults
	// (never stamped onto R's turn rows).
	RoundTripFaults []*RoundTripJudgment
	// EvidenceSecurityIssues are the security-layer slugs E alone caught on a round trip,
	// reconciled into security_flags on the same path as R's SecurityIssues.
	EvidenceSecurityIssues []*sessionintelligencepb.PromptIssue
	// EvidenceRan is true when E fired this pass (set by applyEvidence), false when E did
	// not run (an R-only re-pass, or a session with no artifact/round-trip trigger). The
	// commit's writeRoundTripFaults reads this to decide whether to touch
	// session_round_trip_faults at all: an R-only pass must preserve E's prior fault set
	// rather than replacing it with empty.
	EvidenceRan bool

	// CarriedTaxonomyIssues / CarriedWasteCategories carry the stored session-grain rollups
	// forward on a carried re-pick. Set ONLY by rehydrateReadResult when no turn rows exist
	// (a turn-grain-unsupported source), so applyNewVocabRollup re-writes the stored rollups
	// unchanged instead of NULLing them via an empty turn-flatten — the analog of the
	// read_security_issues fallback. Nil when turn rows exist (the rollups are recomputed).
	CarriedTaxonomyIssues  *sessionintelligencepb.TaxonomyIssueList
	CarriedWasteCategories *sessionintelligencepb.TokenBreakdownList
}

// TurnJudgment is R's judged half of one turn, written column-scoped (never
// clobbering the peer measured grouper's columns). PhaseOrdinal links the turn to its
// phase so the commit can resolve phase_id from the freshly-upserted phase set. The
// nullable fields carry R's abstention (a JSON null / out-of-vocabulary value) through
// as nil, so an unjudged signal stays NULL rather than a bogus zero enum.
// RoundTripFault / RoundTripEvidencePromptIndex are the E producer's per-pair fault
// attribution, applied onto the P turn of each round-trip pair after E runs; ChunkIndex
// tags which windowed read produced the turn (nil on the single-chunk fast path).
type TurnJudgment struct {
	PromptIndex                  int32
	PhaseOrdinal                 *int32
	PromptType                   *sessionintelligencepb.PromptType
	UserSteering                 *sessionintelligencepb.TurnUserSteering
	PromptCompleteness           *sessionintelligencepb.PromptCompleteness
	PromptObjectiveMet           *sessionintelligencepb.PromptObjectiveMet
	AgentHelpfulness             *sessionintelligencepb.AgentHelpfulness
	AgentInitiative              *sessionintelligencepb.AgentInitiative
	ToolUse                      *sessionintelligencepb.ToolUse
	WasRetry                     *sessionintelligencepb.WasRetry
	FailureClass                 *sessionintelligencepb.FailureClass
	TokenLocus                   *sessionintelligencepb.TokenLocus
	WasteCause                   *sessionintelligencepb.WasteCause
	DecisionType                 *sessionintelligencepb.DecisionType
	DownstreamImpact             *sessionintelligencepb.DownstreamImpact
	FeedbackTarget               *sessionintelligencepb.FeedbackTarget
	FeedbackDisposition          *sessionintelligencepb.FeedbackDisposition
	ReferencedArtifact           *string
	ReferencedFindingIDs         *sessionintelligencepb.StringList
	EvidenceSeq                  *int32
	PromptIssues                 *sessionintelligencepb.PromptIssueList
	RoundTripFault               *sessionintelligencepb.RoundTripFault
	RoundTripEvidencePromptIndex *int32
	ChunkIndex                   *int32
}

// PhaseJudgment is R's judged phase — the read pass owns the full phase set, upserted
// by ordinal on reanalysis.
type PhaseJudgment struct {
	PhaseOrdinal       int32
	RangeStart         *int32
	RangeEnd           *int32
	BoundaryOverridden *bool
	OverrideReason     *string
	PhaseTheme         *sessionintelligencepb.PhaseTheme
	SdlcStageSet       *sessionintelligencepb.SdlcStageSet
	TaskComplexity     *sessionintelligencepb.TaskComplexity
	UserSteering       *sessionintelligencepb.PhaseUserSteering
	PhaseObjective     *string
	PhaseObjectiveMet  *sessionintelligencepb.PhaseObjectiveMet
	AgentHelpfulness   *sessionintelligencepb.AgentHelpfulness
	Smoothness         *sessionintelligencepb.Smoothness
	FrictionCause      *sessionintelligencepb.FrictionCause
	Speed              *sessionintelligencepb.Speed
	WentWell           *sessionintelligencepb.StringList
	WentWrong          *sessionintelligencepb.StringList
	Coaching           *sessionintelligencepb.PhaseCoachingList
	TurnCount          *int32
}

// SessionVerdict is R's session-grain judged output the commit maps onto
// session_analyses. WorkType is derived (the dominant phase's theme), not a verdict
// field W emits.
type SessionVerdict struct {
	SessionSteering  *sessionintelligencepb.SessionSteering
	GoalAchieved     *sessionintelligencepb.GoalAchieved
	Smoothness       *sessionintelligencepb.Smoothness
	FrictionCause    *sessionintelligencepb.FrictionCause
	Speed            *sessionintelligencepb.Speed
	TokenEfficiency  *sessionintelligencepb.TokenEfficiency
	GoalIsCodingWork *bool
	UnderlyingGoal   *string
	WorkType         *sessionintelligencepb.PhaseTheme

	// Input-quality scores (0-10), from session_analysis.scores.
	ScoreOverall          *int32
	ScoreClarity          *int32
	ScoreSpecificity      *int32
	ScoreContextProvision *int32
	ScoreActionability    *int32
	ScoreCompleteness     *int32
	ScoreEfficiency       *int32

	// Judged JSONB lists R emits at session grain. ArtifactAdjudications is filled by
	// the E producer (not R) — it lives on the verdict so the commit maps it onto the
	// session_analyses.artifact_adjudications column alongside R's own lists.
	InvokedSkills         *sessionintelligencepb.InvokedSkillJudgmentList
	HarnessFindings       *sessionintelligencepb.HarnessFindingList
	ArtifactAdjudications *sessionintelligencepb.ArtifactAdjudicationList
}

// EvidenceResult is E's output — its one object, three keys: the artifact adjudications
// (session_analyses.artifact_adjudications), the per-invocation skill assessments,
// and the per-pair round-trip fault attributions. E runs after R and owns its own
// storage: applyEvidence routes each key into an E-owned field on ReadResult (the
// skill_assessments column, the session_round_trip_faults rows) rather than mutating
// R's invoked_skills or turn rows, so R stays byte-immutable under E.
type EvidenceResult struct {
	Adjudications    *sessionintelligencepb.ArtifactAdjudicationList
	SkillAssessments []*SkillAssessment
	RoundTrips       []*RoundTripJudgment
	Usage            ProducerUsage
}

// SkillAssessment is E's adjudication of one skill/command invocation, keyed by
// invocation_ref to the invoked_skill R already emitted. Every field is E's — the six
// assessment dimensions plus the evidence locator — carried as pointers so an
// unresolved dimension stays NULL rather than a zero enum.
type SkillAssessment struct {
	InvocationRef        string
	Role                 *sessionintelligencepb.SkillRole
	ContractCompliance   *sessionintelligencepb.ContractCompliance
	OutputQuality        *sessionintelligencepb.OutputQuality
	ExecutionReliability *sessionintelligencepb.ExecutionReliability
	HumanAlignment       *sessionintelligencepb.HumanAlignment
	SkillVerdict         *sessionintelligencepb.SkillVerdict
	// EvidenceSeq is decoded for contract completeness; InvokedSkillJudgment carries no
	// evidence_seq column (its locator is R's evidence_prompt_index), so it is not stored.
	EvidenceSeq *int32
}

// RoundTripJudgment is E's fault attribution for one supplied round-trip pair. PromptIndex
// is P's index (the earlier prompt of the pair — always P, never Q), which is the turn the
// fault lands on; EvidencePromptIndex may be either P or Q.
type RoundTripJudgment struct {
	PromptIndex         int32
	Fault               *sessionintelligencepb.RoundTripFault
	EvidencePromptIndex *int32
	// SecurityIssue is the security-layer slug E caught on this round trip (an api-key /
	// credential leak or a pii injection), nil unless the pair carried one. It routes into
	// security_flags with the E source bit; today the round-trip issue is otherwise not stored.
	SecurityIssue *sessionintelligencepb.PromptIssue
}

// DiscardResult is D's output: the discarded ranges and the monotone
// token_efficiency downgrade.
type DiscardResult struct {
	DiscardedRanges *sessionintelligencepb.DiscardedRangeList
	// TokenEfficiencyDowngrade, when set, can only lower the verdict (efficient →
	// wasteful), never raise it (the monotone-downgrade invariant).
	TokenEfficiencyDowngrade *sessionintelligencepb.TokenEfficiency
	Usage                    ProducerUsage
}

// ProducerUsage carries the token accounting one inference reported, including the
// Gemini system-prompt cache-read hits.
type ProducerUsage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}
