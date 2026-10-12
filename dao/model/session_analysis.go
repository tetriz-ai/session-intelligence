// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

import (
	"time"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// The Session Analysis Engine GORM models. Field
// types track migrations/latest.sql exactly: a
// nullable column is a pointer, a NOT NULL column is a value. Enum columns store
// the proto enum NAME as text — the F1 gen_sql Value()/Scan() adapters
// are the driver.Valuer / sql.Scanner GORM binds, so no serializer tag is needed
// (same as model/loop.go). A nullable enum MUST be a pointer: the gen_sql Scan
// errors on a NULL value, so a value enum would fail to read a NULL row.
//
// Five tables in two roles: the four content tables (session_analyses,
// session_turn_analyses, session_round_trip_faults, session_phase_analyses) carry the
// data_window RLS policy; the single orchestration table (session_analysis_state)
// carries tenant_isolation only. Per-table scoping is documented on each struct below.

// SessionAnalysis is the session-grain content row for session_analyses
// (seat-scoped: has person_id). One live row per (organization_id,
// raw_record_id, vocabulary_version); SupersededAt marks a stale generation.
type SessionAnalysis struct {
	ID             string  `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string  `gorm:"column:organization_id;type:uuid;not null"`
	PersonID       string  `gorm:"column:person_id;type:uuid;not null"`
	RawRecordID    string  `gorm:"column:raw_record_id;type:uuid;not null"`
	ConversationID *string `gorm:"column:conversation_id"`
	SessionID      *string `gorm:"column:session_id"`
	// IdeType is the session's IDE, stored as the proto enum name. It is
	// nullable here — unlike session_loop_analyses.ide_type, which is NOT NULL —
	// so it is a pointer. The gen_sql Scan errors on a value outside the IdeType
	// vocabulary, so a writer must map an unrecognised vendor IDE to a known enum
	// value (or leave it NULL) rather than passing an arbitrary string through.
	IdeType               *commonv1.IdeType `gorm:"column:ide_type;type:text"`
	ConversationStartedAt time.Time         `gorm:"column:conversation_started_at;not null"`
	LastActiveAt          *time.Time        `gorm:"column:last_active_at"`
	VocabularyVersion     int32             `gorm:"column:vocabulary_version;not null"`

	// Capability flags: what the source transcript let the judge observe.
	CapTokenSplit     *sessionintelligencepb.TokenSplitCapability     `gorm:"column:cap_token_split;type:text"`
	CapToolOutcome    *sessionintelligencepb.ToolOutcomeCapability    `gorm:"column:cap_tool_outcome;type:text"`
	CapReasoningTrace *sessionintelligencepb.ReasoningTraceCapability `gorm:"column:cap_reasoning_trace;type:text"`
	CapAssistantText  *sessionintelligencepb.AssistantTextCapability  `gorm:"column:cap_assistant_text;type:text"`

	// Session verdict.
	SessionSteering  *sessionintelligencepb.SessionSteering `gorm:"column:session_steering;type:text"`
	GoalAchieved     *sessionintelligencepb.GoalAchieved    `gorm:"column:goal_achieved;type:text"`
	Smoothness       *sessionintelligencepb.Smoothness      `gorm:"column:smoothness;type:text"`
	FrictionCause    *sessionintelligencepb.FrictionCause   `gorm:"column:friction_cause;type:text"`
	Speed            *sessionintelligencepb.Speed           `gorm:"column:speed;type:text"`
	TokenEfficiency  *sessionintelligencepb.TokenEfficiency `gorm:"column:token_efficiency;type:text"`
	GoalIsCodingWork *bool                                  `gorm:"column:goal_is_coding_work"`
	UnderlyingGoal   *string                                `gorm:"column:underlying_goal"`

	// Input-quality scores (smallint).
	ScoreOverall          *int32   `gorm:"column:score_overall"`
	ScoreClarity          *int32   `gorm:"column:score_clarity"`
	ScoreSpecificity      *int32   `gorm:"column:score_specificity"`
	ScoreContextProvision *int32   `gorm:"column:score_context_provision"`
	ScoreActionability    *int32   `gorm:"column:score_actionability"`
	ScoreCompleteness     *int32   `gorm:"column:score_completeness"`
	ScoreEfficiency       *int32   `gorm:"column:score_efficiency"`
	WeightedScore         *float64 `gorm:"column:weighted_score"`

	// Cost and tokens.
	EstimatedCostUsd    *float64                                      `gorm:"column:estimated_cost_usd"`
	OwnEstimatedCostUsd *float64                                      `gorm:"column:own_estimated_cost_usd"`
	TotalPrompts        *int32                                        `gorm:"column:total_prompts"`
	TotalTokens         *int64                                        `gorm:"column:total_tokens"`
	WastedTokens        *int64                                        `gorm:"column:wasted_tokens"`
	TokenBreakdown      *sessionintelligencepb.AnalysisTokenBreakdown `gorm:"column:token_breakdown;type:jsonb"`
	SubagentUsage       *sessionintelligencepb.SubagentUsage          `gorm:"column:subagent_usage;type:jsonb"`

	// Session telemetry.
	IterationCount               *int32                                     `gorm:"column:iteration_count"`
	EffectiveActiveDurationS     *int32                                     `gorm:"column:effective_active_duration_s"`
	EffectiveAiCodePercent       *float64                                   `gorm:"column:effective_ai_code_percent"`
	EffectiveContextUsagePercent *float64                                   `gorm:"column:effective_context_usage_percent"`
	McpServerCount               *int32                                     `gorm:"column:mcp_server_count"`
	DelegationLevel              *sessionintelligencepb.DelegationLevel     `gorm:"column:delegation_level;type:text"`
	EffectivenessSource          *sessionintelligencepb.EffectivenessSource `gorm:"column:effectiveness_source;type:text"`
	IsSessionEffective           *bool                                      `gorm:"column:is_session_effective"`
	SessionOutcome               *sessionintelligencepb.SessionOutcome      `gorm:"column:session_outcome;type:text"`
	UserInterruptions            *int32                                     `gorm:"column:user_interruptions"`
	PlanTier                     *string                                    `gorm:"column:plan_tier"`
	// allow:string-enum — plan_tier_source is an open vendor passthrough text, not a
	// closed enum; the queryable closed dimension is PlanTierOrigin below.
	PlanTierSource         *string                               `gorm:"column:plan_tier_source"`
	PlanTierOrigin         *sessionintelligencepb.PlanTierOrigin `gorm:"column:plan_tier_origin;type:text"`
	SeatMonthlyPriceUsd    *float64                              `gorm:"column:seat_monthly_price_usd"`
	HasExtraUsageEnabled   *bool                                 `gorm:"column:has_extra_usage_enabled"`
	AccountUUID            *string                               `gorm:"column:account_uuid"`
	LoginEmail             *string                               `gorm:"column:login_email"`
	RepoName               *string                               `gorm:"column:repo_name"`
	ProjectName            *string                               `gorm:"column:project_name"`
	SessionDurationSeconds *int32                                `gorm:"column:session_duration_seconds"`
	CacheHitRatePercent    *float64                              `gorm:"column:cache_hit_rate_percent"`
	CompactionCount        *int32                                `gorm:"column:compaction_count"`
	FilesEditedCount       *int32                                `gorm:"column:files_edited_count"`
	FirstUserMsgTs         *time.Time                            `gorm:"column:first_user_msg_ts"`

	// Structural aggregates.
	TurnCount          *int32   `gorm:"column:turn_count"`
	HumanTurnCount     *int32   `gorm:"column:human_turn_count"`
	DelegatedTurnCount *int32   `gorm:"column:delegated_turn_count"`
	PhaseCount         *int32   `gorm:"column:phase_count"`
	ToolCallCount      *int32   `gorm:"column:tool_call_count"`
	ActiveShare        *float64 `gorm:"column:active_share"`

	// WorkType reuses PhaseTheme.
	WorkType            *sessionintelligencepb.PhaseTheme `gorm:"column:work_type;type:text"`
	SegmentationSuspect *bool                             `gorm:"column:segmentation_suspect"`

	// Surfaced sets.
	Languages      *sessionintelligencepb.StringList     `gorm:"column:languages;type:jsonb"`
	ModelsUsed     *sessionintelligencepb.StringList     `gorm:"column:models_used;type:jsonb"`
	FrictionCounts *sessionintelligencepb.FrictionCounts `gorm:"column:friction_counts;type:jsonb"`

	// Judged lists.
	InvokedSkills *sessionintelligencepb.InvokedSkillJudgmentList `gorm:"column:invoked_skills;type:jsonb"`
	// ReadSecurityIssues is R's flat security readout — the security-layer prompt issues R
	// judged across the session. It is R-owned and reconciled into security_flags at commit
	// time; the row keeps its own copy because a source whose turn grain is not stored
	// (supportsTurnGrouping == false) has no turn rows to reconstruct it from on a re-pick,
	// and without it the last-writer-wins security reconcile would delete R's flags
	//. Nil when R produced no security issues.
	ReadSecurityIssues *sessionintelligencepb.PromptIssueList `gorm:"column:read_security_issues;type:jsonb"`
	// SkillAssessments is E's per-invocation skill adjudication, keyed by invocation_ref to
	// the invoked_skill R named. It is an E-OWNED column — E's assessments are stored here,
	// never merged into R's invoked_skills, so R's row stays byte-immutable under E.
	// Nil when E did not run.
	SkillAssessments      *sessionintelligencepb.InvokedSkillJudgmentList `gorm:"column:skill_assessments;type:jsonb"`
	ArtifactAdjudications *sessionintelligencepb.ArtifactAdjudicationList `gorm:"column:artifact_adjudications;type:jsonb"`
	HarnessFindings       *sessionintelligencepb.HarnessFindingList       `gorm:"column:harness_findings;type:jsonb"`
	DiscardedRanges       *sessionintelligencepb.DiscardedRangeList       `gorm:"column:discarded_ranges;type:jsonb"`
	SignalCoverage        *sessionintelligencepb.SignalCoverageList       `gorm:"column:signal_coverage;type:jsonb"`

	// Serving-parity analysis columns (session-analyses-serving-parity design): the session's
	// taxonomy issues, per-category waste breakdown, and the scalar analysis outputs. Each is
	// nil when absent, so a partial row serves NULL-tolerantly.
	TaxonomyIssues  *sessionintelligencepb.TaxonomyIssueList  `gorm:"column:taxonomy_issues;type:jsonb"`
	WasteCategories *sessionintelligencepb.TokenBreakdownList `gorm:"column:waste_categories;type:jsonb"`
	Recommendations *sessionintelligencepb.StringList         `gorm:"column:recommendations;type:jsonb"`
	AiReviewed      *bool                                     `gorm:"column:ai_reviewed"`
	ImprovedPrompt  *string                                   `gorm:"column:improved_prompt"`
	TaskType        *string                                   `gorm:"column:task_type"`
	// Origin marks the row's provenance (see SessionAnalysisOrigin), the discriminator that
	// distinguishes live-engine rows from copy-migrated ones.
	Origin *sessionintelligencepb.SessionAnalysisOrigin `gorm:"column:origin;type:text"`

	// SessionMetaRaw is the vendor session-metadata pass-through, kept verbatim.
	SessionMetaRaw *sessionintelligencepb.UsageMetadata `gorm:"column:session_meta_raw;type:jsonb"` //allow:rawjson — vendor session metadata pass-through
	// McpServerNames and ToolErrors are read-only STORED generated columns hoisted off
	// session_meta_raw (migration 000151 carries the full rationale); the ;-> tag stops GORM
	// writing them, as Postgres rejects a write to a generated column. McpServerNames holds the
	// vendor mcpServerNames array in the StringList {"values":[...]} shape (nil when absent).
	// ToolErrors is the toolErrors scalar as text; nil (NULL) means EITHER a measured session
	// with 0 tool errors (proto3 omits a zero int) OR an unmeasured session — a per-session
	// average reader must COALESCE nil to 0 over measured rows, never a naive AVG (see the
	// tool_errors column comment in migration 000151).
	McpServerNames *sessionintelligencepb.StringList `gorm:"column:mcp_server_names;type:jsonb;->"`
	ToolErrors     *string                           `gorm:"column:tool_errors;->"`
	// SessionType, CostIsEstimated, CostIsInputOnly and UserMessageTimestamps are read-only STORED
	// generated columns hoisted off session_meta_raw (session-analyses-serving-parity design); the ;->
	// tag stops GORM writing them. SessionType is the session-facets task classification.
	// CostIsEstimated / CostIsInputOnly are the vendor cost flags; nil (NULL) collapses a genuinely-
	// false flag with an unmeasured session — readers COALESCE nil to false over measured rows (see
	// the cost_is_estimated column comment). UserMessageTimestamps holds the vendor
	// userMessageTimestamps array in the StringList {"values":[...]} shape (nil when absent/non-array).
	SessionType           *string                           `gorm:"column:session_type;->"`
	CostIsEstimated       *bool                             `gorm:"column:cost_is_estimated;->"`
	CostIsInputOnly       *bool                             `gorm:"column:cost_is_input_only;->"`
	UserMessageTimestamps *sessionintelligencepb.StringList `gorm:"column:user_message_timestamps;type:jsonb;->"`

	CreatedAt         time.Time                                `gorm:"column:created_at"`
	UpdatedAt         time.Time                                `gorm:"column:updated_at"`
	DeletedAt         gorm.DeletedAt                           `gorm:"column:deleted_at"`
	JudgedAt          *time.Time                               `gorm:"column:judged_at"`
	JudgedDisposition *sessionintelligencepb.JudgedDisposition `gorm:"column:judged_disposition;type:text"`
	SupersededAt      *time.Time                               `gorm:"column:superseded_at"`

	// Per-producer(-half) readiness — orthogonal to judged_disposition (which stays R's tier):
	// D and E each carry their own resolution state, and E carries one per half because its
	// round-trip half can commit independently of its artifact half. The commit stamps only
	// PRODUCER_READINESS_PRODUCED here (when the half ran this generation) or leaves it nil;
	// a half's PENDING / FAILED / NOT_APPLICABLE state is NOT written here — it is tracked
	// authoritatively in session_analysis_state.pending_data_producers and the JUDGE_FAILED
	// disposition, so read THOSE, not this column, for an unresolved half. The finer
	// ProducerReadiness values land here only once a serving reader needs them.
	DiscardDisposition           *sessionintelligencepb.ProducerReadiness `gorm:"column:discard_disposition;type:text"`
	EvidenceArtifactDisposition  *sessionintelligencepb.ProducerReadiness `gorm:"column:evidence_artifact_disposition;type:text"`
	EvidenceRoundtripDisposition *sessionintelligencepb.ProducerReadiness `gorm:"column:evidence_roundtrip_disposition;type:text"`
}

// TableName returns the database table name for GORM.
func (SessionAnalysis) TableName() string { return "session_analyses" }

// SessionAnalysisState is the orchestration row for session_analysis_state
// (tenant_isolation only — not plan-scoped). PersonID is nullable: a record may
// enter the pipeline before its person resolves.
type SessionAnalysisState struct { //allow:not-seat-scoped orchestration state; tenant_isolation only, actionable-work scan must see out-of-window rows
	ID                    string                                           `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID        string                                           `gorm:"column:organization_id;type:uuid;not null"`
	PersonID              *string                                          `gorm:"column:person_id;type:uuid"`
	RawRecordID           string                                           `gorm:"column:raw_record_id;type:uuid;not null"`
	ConversationStartedAt time.Time                                        `gorm:"column:conversation_started_at;not null"`
	Status                sessionintelligencepb.SessionAnalysisStatus      `gorm:"column:status;type:text;not null"`
	SkipReason            *sessionintelligencepb.SessionAnalysisSkipReason `gorm:"column:skip_reason;type:text"`
	RepoName              *string                                          `gorm:"column:repo_name"`
	SourceContentSha256   *string                                          `gorm:"column:source_content_sha256"`
	// SourceUpdatedAt is the raw_records.updated_at captured when this generation's spine
	// last ran — the legacy staleness baseline for a session row whose payload carries no
	// content_sha256. The category-3 legacy branch and the
	// commit re-read compare the raw record's current updated_at against this value; a row
	// with content_sha256 ignores it (it stays on the content-hash path). NULL until the
	// generation's first spine_done.
	SourceUpdatedAt         *time.Time                          `gorm:"column:source_updated_at"`
	SpineVersion            *int32                              `gorm:"column:spine_version"`
	VocabularyVersion       int32                               `gorm:"column:vocabulary_version;not null"`
	CallState               *sessionintelligencepb.CallStateMap `gorm:"column:call_state;type:jsonb"`
	ContractVersionRead     *int32                              `gorm:"column:contract_version_read"`
	ContractVersionEvidence *int32                              `gorm:"column:contract_version_evidence"`
	ContractVersionDiscard  *int32                              `gorm:"column:contract_version_discard"`
	AttemptCount            int32                               `gorm:"column:attempt_count;not null"`
	LastError               *string                             `gorm:"column:last_error"`
	LastAttemptedAt         *time.Time                          `gorm:"column:last_attempted_at"`
	LastSucceededAt         *time.Time                          `gorm:"column:last_succeeded_at"`
	ChildrenSeq             int32                               `gorm:"column:children_seq;not null"`
	// FoldedChildrenSeq is the children_seq the last successful judge folded; a later child
	// sighting bumps children_seq above it, which the child-refold scan leg detects to re-fold
	// the newly-arrived delegated children's cost.
	FoldedChildrenSeq    int32 `gorm:"column:folded_children_seq;not null"`
	FlagSkippedProducers int32 `gorm:"column:flag_skipped_producers;not null"`
	// PendingDataProducers is the flat bitmask of producers that went
	// CALL_STATUS_SKIPPED_PENDING_DATA — an input dependency not yet resolvable. It shares the bit
	// space of FlagSkippedProducers (see the producer bit constants in the session-analysis
	// processor) and is the scannable counterpart to the call_state status the scan cannot see,
	// driving the pending-data re-admission leg until the data resolves or the body changes
	//.
	PendingDataProducers int32 `gorm:"column:pending_data_producers;not null"`
	// FailedProducers is the flat bitmask of producers that went CALL_STATUS_FAILED while R
	// committed a valid verdict (so the record is JUDGED/SPINE_DONE, not wholesale FAILED). It shares
	// the bit space of FlagSkippedProducers/PendingDataProducers and is the scannable counterpart to
	// the call_state status the scan cannot see, driving the failed-producer re-admission leg that
	// retries ONLY the failed producer — carrying R — until it succeeds or the record's retry cap is
	// reached.
	FailedProducers int32          `gorm:"column:failed_producers;not null"`
	ClaimToken      *string        `gorm:"column:claim_token;type:uuid"`
	CreatedAt       time.Time      `gorm:"column:created_at"`
	UpdatedAt       time.Time      `gorm:"column:updated_at"`
	DeletedAt       gorm.DeletedAt `gorm:"column:deleted_at"`
	// Origin marks the row's provenance (see SessionAnalysisOrigin), the discriminator that
	// distinguishes live-engine rows from copy-migrated ones.
	Origin *sessionintelligencepb.SessionAnalysisOrigin `gorm:"column:origin;type:text"`
}

// TableName returns the database table name for GORM.
func (SessionAnalysisState) TableName() string { return "session_analysis_state" }

// SessionTurnAnalysis is the turn-grain content row for session_turn_analyses
// (data-window-only: no person_id; seat scoping composes through the parent
// session_analyses join). SessionAnalysisID is lazy-resolved, so it is nullable.
type SessionTurnAnalysis struct {
	ID                    string                                   `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID        string                                   `gorm:"column:organization_id;type:uuid;not null"`
	RawRecordID           string                                   `gorm:"column:raw_record_id;type:uuid;not null"`
	SessionAnalysisID     *string                                  `gorm:"column:session_analysis_id;type:uuid"`
	ConversationStartedAt time.Time                                `gorm:"column:conversation_started_at;not null"`
	PromptIndex           int32                                    `gorm:"column:prompt_index;not null"`
	PromptOrigination     *sessionintelligencepb.PromptOrigination `gorm:"column:prompt_origination;type:text"`
	Author                *string                                  `gorm:"column:author"`
	IsSubagent            *bool                                    `gorm:"column:is_subagent"`

	// Judged (peer) layer.
	PromptType                   *sessionintelligencepb.PromptType          `gorm:"column:prompt_type;type:text"`
	UserSteering                 *sessionintelligencepb.TurnUserSteering    `gorm:"column:user_steering;type:text"`
	PromptCompleteness           *sessionintelligencepb.PromptCompleteness  `gorm:"column:prompt_completeness;type:text"`
	PromptObjectiveMet           *sessionintelligencepb.PromptObjectiveMet  `gorm:"column:prompt_objective_met;type:text"`
	AgentHelpfulness             *sessionintelligencepb.AgentHelpfulness    `gorm:"column:agent_helpfulness;type:text"`
	AgentInitiative              *sessionintelligencepb.AgentInitiative     `gorm:"column:agent_initiative;type:text"`
	ToolUse                      *sessionintelligencepb.ToolUse             `gorm:"column:tool_use;type:text"`
	WasRetry                     *sessionintelligencepb.WasRetry            `gorm:"column:was_retry;type:text"`
	FailureClass                 *sessionintelligencepb.FailureClass        `gorm:"column:failure_class;type:text"`
	TokenLocus                   *sessionintelligencepb.TokenLocus          `gorm:"column:token_locus;type:text"`
	WasteCause                   *sessionintelligencepb.WasteCause          `gorm:"column:waste_cause;type:text"`
	DecisionType                 *sessionintelligencepb.DecisionType        `gorm:"column:decision_type;type:text"`
	DownstreamImpact             *sessionintelligencepb.DownstreamImpact    `gorm:"column:downstream_impact;type:text"`
	FeedbackTarget               *sessionintelligencepb.FeedbackTarget      `gorm:"column:feedback_target;type:text"`
	FeedbackDisposition          *sessionintelligencepb.FeedbackDisposition `gorm:"column:feedback_disposition;type:text"`
	ReferencedArtifact           *string                                    `gorm:"column:referenced_artifact"`
	ReferencedFindingIDs         *sessionintelligencepb.StringList          `gorm:"column:referenced_finding_ids;type:jsonb"`
	RoundTripFault               *sessionintelligencepb.RoundTripFault      `gorm:"column:round_trip_fault;type:text"`
	RoundTripEvidencePromptIndex *int32                                     `gorm:"column:round_trip_evidence_prompt_index"`
	PromptIssues                 *sessionintelligencepb.PromptIssueList     `gorm:"column:prompt_issues;type:jsonb"`
	JudgedContentSha256          *string                                    `gorm:"column:judged_content_sha256"`

	// Measured (peer) layer.
	// Models carries each model that served the turn with its per-model reasoning effort; effort
	// lives in this blob rather than a flat column so a multi-model turn keeps each model's effort.
	Models         *sessionintelligencepb.TurnModelList          `gorm:"column:models;type:jsonb"`
	TokenBreakdown *sessionintelligencepb.AnalysisTokenBreakdown `gorm:"column:token_breakdown;type:jsonb"`
	TokenFidelity  *sessionintelligencepb.TokenFidelity          `gorm:"column:token_fidelity;type:text"`
	// The four *_tokens columns are GENERATED ALWAYS AS STORED from token_breakdown;
	// the ;-> tag makes them read-only so GORM never writes them (Postgres rejects a
	// write to a generated column).
	InputTokens      *int64 `gorm:"column:input_tokens;->"`
	OutputTokens     *int64 `gorm:"column:output_tokens;->"`
	CacheReadTokens  *int64 `gorm:"column:cache_read_tokens;->"`
	CacheWriteTokens *int64 `gorm:"column:cache_write_tokens;->"`
	// NativeBillingUnits is the vendor billing units the turn consumed when the surface meters
	// in units other than tokens (Kiro meters in credits); nil when the surface reports none.
	NativeBillingUnits *float64                            `gorm:"column:native_billing_units"`
	LinesAdded         *int32                              `gorm:"column:lines_added"`
	LinesRemoved       *int32                              `gorm:"column:lines_removed"`
	ToolCallCount      *int32                              `gorm:"column:tool_call_count"`
	ToolErrorCount     *int32                              `gorm:"column:tool_error_count"`
	Tools              *sessionintelligencepb.ToolCallList `gorm:"column:tools;type:jsonb"`
	ToolRetryCount     *int32                              `gorm:"column:tool_retry_count"`
	APIRetryCount      *int32                              `gorm:"column:api_retry_count"`
	// CompactionCount is the context compactions attributed to this turn; nil when the surface
	// does not report compaction position (the session-grain total is SessionAnalysis.CompactionCount).
	CompactionCount *int32                                `gorm:"column:compaction_count"`
	FilesTouched    *sessionintelligencepb.FileTouchList  `gorm:"column:files_touched;type:jsonb"`
	Surface         *sessionintelligencepb.SessionSurface `gorm:"column:surface;type:text"`
	StartedAt       *time.Time                            `gorm:"column:started_at"`
	EndedAt         *time.Time                            `gorm:"column:ended_at"`
	RepoFullName    *string                               `gorm:"column:repo_full_name"`
	ContextTokens   *int64                                `gorm:"column:context_tokens"`
	ContextPct      *float64                              `gorm:"column:context_pct"`
	SubagentCount   *int32                                `gorm:"column:subagent_count"`
	// ParentConversationID locates the parent of a sub-agent turn stored as its own row: the
	// parent prompt index / tool-use id are meaningless without the conversation they index into,
	// so the child-to-parent rollup joins on (org, parent_conversation_id, parent_prompt_index).
	ParentConversationID *string `gorm:"column:parent_conversation_id"`
	ParentPromptIndex    *int32  `gorm:"column:parent_prompt_index"`
	ParentToolUseID      *string `gorm:"column:parent_tool_use_id"`
	SourceContentSha256  *string `gorm:"column:source_content_sha256"`

	// Linkage.
	PhaseID     *string `gorm:"column:phase_id;type:uuid"`
	EvidenceSeq *int32  `gorm:"column:evidence_seq"`
	ChunkIndex  *int32  `gorm:"column:chunk_index"`

	CreatedAt time.Time      `gorm:"column:created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (SessionTurnAnalysis) TableName() string { return "session_turn_analyses" }

// SessionRoundTripFault is E's per-pair round-trip fault, stored in its own E-owned table
// (data-window-only: no person_id; seat scoping composes through the parent session_analyses
// join, like session_turn_analyses). PromptIndex is P's index — the earlier prompt of the
// pair the fault lands on. E stops stamping fault onto R's turn rows and writes these rows
// instead, replace-set per E re-run, so R's turn rows stay immutable. Fault is
// nullable text (a pair E resolved as
// no_round_trip still records that verdict), so it is a pointer.
type SessionRoundTripFault struct {
	ID                    string                                `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID        string                                `gorm:"column:organization_id;type:uuid;not null"`
	SessionAnalysisID     string                                `gorm:"column:session_analysis_id;type:uuid;not null"`
	RawRecordID           string                                `gorm:"column:raw_record_id;type:uuid;not null"`
	ConversationStartedAt time.Time                             `gorm:"column:conversation_started_at;not null"`
	PromptIndex           int32                                 `gorm:"column:prompt_index;not null"`
	Fault                 *sessionintelligencepb.RoundTripFault `gorm:"column:fault;type:text"`
	EvidencePromptIndex   *int32                                `gorm:"column:evidence_prompt_index"`
	CreatedAt             time.Time                             `gorm:"column:created_at"`
	UpdatedAt             time.Time                             `gorm:"column:updated_at"`
	DeletedAt             gorm.DeletedAt                        `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (SessionRoundTripFault) TableName() string { return "session_round_trip_faults" }

// SessionPhaseAnalysis is the phase-grain content row for session_phase_analyses
// (data-window-only: no person_id; seat scoping via the parent join).
type SessionPhaseAnalysis struct {
	ID                    string                                   `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID        string                                   `gorm:"column:organization_id;type:uuid;not null"`
	SessionAnalysisID     *string                                  `gorm:"column:session_analysis_id;type:uuid"`
	ConversationStartedAt time.Time                                `gorm:"column:conversation_started_at;not null"`
	PhaseOrdinal          *int32                                   `gorm:"column:phase_ordinal"`
	RangeStart            *int32                                   `gorm:"column:range_start"`
	RangeEnd              *int32                                   `gorm:"column:range_end"`
	BoundaryOverridden    *bool                                    `gorm:"column:boundary_overridden"`
	OverrideReason        *string                                  `gorm:"column:override_reason"`
	PhaseTheme            *sessionintelligencepb.PhaseTheme        `gorm:"column:phase_theme;type:text"`
	SdlcStageSet          *sessionintelligencepb.SdlcStageSet      `gorm:"column:sdlc_stage_set;type:jsonb"`
	TaskComplexity        *sessionintelligencepb.TaskComplexity    `gorm:"column:task_complexity;type:text"`
	UserSteering          *sessionintelligencepb.PhaseUserSteering `gorm:"column:user_steering;type:text"`
	PhaseObjective        *string                                  `gorm:"column:phase_objective"`
	PhaseObjectiveMet     *sessionintelligencepb.PhaseObjectiveMet `gorm:"column:phase_objective_met;type:text"`
	AgentHelpfulness      *sessionintelligencepb.AgentHelpfulness  `gorm:"column:agent_helpfulness;type:text"`
	Smoothness            *sessionintelligencepb.Smoothness        `gorm:"column:smoothness;type:text"`
	FrictionCause         *sessionintelligencepb.FrictionCause     `gorm:"column:friction_cause;type:text"`
	Speed                 *sessionintelligencepb.Speed             `gorm:"column:speed;type:text"`
	HitlGate              *sessionintelligencepb.HitlGate          `gorm:"column:hitl_gate;type:text"`
	TurnCount             *int32                                   `gorm:"column:turn_count"`
	Tokens                *int64                                   `gorm:"column:tokens"`
	ToolCallCount         *int32                                   `gorm:"column:tool_call_count"`
	WentWell              *sessionintelligencepb.StringList        `gorm:"column:went_well;type:jsonb"`
	WentWrong             *sessionintelligencepb.StringList        `gorm:"column:went_wrong;type:jsonb"`
	Coaching              *sessionintelligencepb.PhaseCoachingList `gorm:"column:coaching;type:jsonb"`
	SegmentationSuspect   *bool                                    `gorm:"column:segmentation_suspect"`
	CreatedAt             time.Time                                `gorm:"column:created_at"`
	UpdatedAt             time.Time                                `gorm:"column:updated_at"`
	DeletedAt             gorm.DeletedAt                           `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (SessionPhaseAnalysis) TableName() string { return "session_phase_analyses" }
