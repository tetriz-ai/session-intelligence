// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
)

// LLMClient is the concrete LLMProducer over the shared reference-llm transport
// (internal/refllmclient). It sets each producer's task type, binds
// the Langfuse session identity + per-trace tags, and opts into Gemini system-prompt
// caching; result parsing into the typed producer outputs stays here (level 2), the
// transport in the shared client.
type LLMClient struct {
	client refllmclient.ContentGenerator
}

// NewLLMClient builds the session-analysis producer client over the shared
// reference-llm client.
func NewLLMClient(client refllmclient.ContentGenerator) *LLMClient {
	return &LLMClient{client: client}
}

// buildRequest assembles the shared generate-content request, binding the Langfuse
// session id (the raw_record_id, so every R/E/D call for one session groups under
// one trace session) and the per-trace tags.
func buildRequest(taskType string, req ProducerRequest) refllmclient.GenerateContentRequest {
	sessionCtx := map[string]any{
		"langfuse_session_id": req.SessionID,
		"chunk_index":         req.ChunkIndex,
	}
	if len(req.Tags) > 0 {
		sessionCtx["langfuse_tags"] = req.Tags
	}
	return refllmclient.GenerateContentRequest{
		Content:              req.Content,
		TaskType:             taskType,
		OrgID:                req.OrgID,
		SourceID:             req.SessionID,
		SessionContext:       sessionCtx,
		UseSystemPromptCache: req.UseSystemPromptCache,
	}
}

func usageFrom(resp *refllmclient.GenerateContentResponse) ProducerUsage {
	if resp == nil || resp.Usage == nil {
		return ProducerUsage{}
	}
	return ProducerUsage{
		InputTokens:     int64(resp.Usage.PromptTokens),
		OutputTokens:    int64(resp.Usage.CompletionTokens),
		CacheReadTokens: int64(resp.Usage.CacheReadTokens),
	}
}

// AnalyzeRead runs the merged read pass and decodes the session verdict, the
// per-turn issue lists, and the security-layer prompt issues.
func (c *LLMClient) AnalyzeRead(ctx context.Context, req ProducerRequest) (*ReadResult, error) {
	resp, err := c.client.GenerateContent(ctx, buildRequest(refllmclient.TaskTypeAnalyzeRead, req))
	if err != nil {
		return nil, fmt.Errorf("analyze_read (session %s): %w", req.SessionID, err)
	}
	parsed, err := parseReadResponse(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("parsing analyze_read result (session %s): %w", req.SessionID, err)
	}
	parsed.Usage = usageFrom(resp)
	parsed.ScorerModel = resp.Model
	return parsed, nil
}

// AnalyzeEvidence runs the evidence pass and decodes the three keys — artifact
// adjudications, skill assessments, and round-trip fault attributions.
func (c *LLMClient) AnalyzeEvidence(ctx context.Context, req ProducerRequest) (*EvidenceResult, error) {
	resp, err := c.client.GenerateContent(ctx, buildRequest(refllmclient.TaskTypeAnalyzeEvidence, req))
	if err != nil {
		return nil, fmt.Errorf("analyze_evidence (session %s): %w", req.SessionID, err)
	}
	parsed, err := parseEvidenceResponse(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("parsing analyze_evidence result (session %s): %w", req.SessionID, err)
	}
	parsed.Usage = usageFrom(resp)
	return parsed, nil
}

// AnalyzeDiscard runs the discard tail and decodes the discarded work ranges. The
// per-range token_sum and the session-grain wasted_tokens are code-summed at commit
// time from the spine's measured per-turn token costs (the waste ledger is a MEASURED
// column over D's judged ranges — the LLM output contract, Call 5), not by
// the model, so they are left zero here.
func (c *LLMClient) AnalyzeDiscard(ctx context.Context, req ProducerRequest) (*DiscardResult, error) {
	resp, err := c.client.GenerateContent(ctx, buildRequest(refllmclient.TaskTypeAnalyzeDiscard, req))
	if err != nil {
		return nil, fmt.Errorf("analyze_discard (session %s): %w", req.SessionID, err)
	}
	parsed, err := parseDiscardResponse(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("parsing analyze_discard result (session %s): %w", req.SessionID, err)
	}
	parsed.Usage = usageFrom(resp)
	return parsed, nil
}

// The analyze_read response wire shape — the deployed reference-llm AnalyzeReadResponse
// (app/llm/schemas/session_analysis.py): one object with `turns`, `phases`, and
// `session_analysis`, every enum a LOWERCASE StrEnum value. This must track that schema
// exactly — a field the wire omits is silently dropped by json.Unmarshal and commits
// NULL to its column, so every field the schema defines is mirrored here.
type readWire struct {
	Turns           []readTurnWire  `json:"turns"`
	Phases          []readPhaseWire `json:"phases"`
	SessionAnalysis readVerdictWire `json:"session_analysis"`
}

// readTurnWire mirrors the Python TurnAnalysis: every human- and agent-behaviour
// signal, its nullable fields as pointers so a JSON null decodes to nil (abstention),
// not a zero enum.
type readTurnWire struct {
	PromptIndex          int32             `json:"prompt_index"`
	PromptType           string            `json:"prompt_type"`
	UserSteering         *string           `json:"user_steering"`
	PromptCompleteness   *string           `json:"prompt_completeness"`
	PromptIssues         []promptIssueWire `json:"prompt_issues"`
	PromptObjectiveMet   string            `json:"prompt_objective_met"`
	AgentHelpfulness     string            `json:"agent_helpfulness"`
	AgentInitiative      string            `json:"agent_initiative"`
	ToolUse              string            `json:"tool_use"`
	WasRetry             *string           `json:"was_retry"`
	FailureClass         *string           `json:"failure_class"`
	TokenLocus           string            `json:"token_locus"`
	WasteCause           *string           `json:"waste_cause"`
	DecisionType         *string           `json:"decision_type"`
	DownstreamImpact     *string           `json:"downstream_impact"`
	FeedbackTarget       *string           `json:"feedback_target"`
	FeedbackDisposition  *string           `json:"feedback_disposition"`
	ReferencedArtifact   *string           `json:"referenced_artifact"`
	ReferencedFindingIDs []string          `json:"referenced_finding_ids"`
	EvidenceSeq          *int32            `json:"evidence_seq"`
}

type promptIssueWire struct {
	Slug     string  `json:"slug"`
	Severity string  `json:"severity"`
	Note     *string `json:"note"`
}

// readPhaseWire mirrors the Python PhaseAnalysis.
type readPhaseWire struct {
	PromptRange struct {
		StartIndex int32 `json:"start_index"`
		EndIndex   int32 `json:"end_index"`
	} `json:"prompt_range"`
	BoundaryOverridden bool                `json:"boundary_overridden"`
	OverrideReason     *string             `json:"override_reason"`
	PhaseTheme         string              `json:"phase_theme"`
	SdlcStage          []string            `json:"sdlc_stage"`
	TaskComplexity     string              `json:"task_complexity"`
	UserSteering       string              `json:"user_steering"`
	PhaseObjective     *string             `json:"phase_objective"`
	PhaseObjectiveMet  string              `json:"phase_objective_met"`
	AgentHelpfulness   string              `json:"agent_helpfulness"`
	Smoothness         string              `json:"smoothness"`
	FrictionCause      string              `json:"friction_cause"`
	Speed              string              `json:"speed"`
	WentWell           []string            `json:"went_well"`
	WentWrong          []string            `json:"went_wrong"`
	CouldBeBetter      []couldBeBetterWire `json:"could_be_better"`
}

// couldBeBetterWire mirrors the Python CouldBeBetter — one phase coaching note.
type couldBeBetterWire struct {
	RootCause           string   `json:"root_cause"`
	Suggestion          string   `json:"suggestion"`
	Lever               string   `json:"lever"`
	Axes                []string `json:"axes"`
	Target              *string  `json:"target"`
	EvidencePromptIndex int32    `json:"evidence_prompt_index"`
}

// readVerdictWire mirrors the Python SessionVerdict, including the R-emitted judged
// lists (invoked_skills, harness_findings).
type readVerdictWire struct {
	UnderlyingGoal   string `json:"underlying_goal"`
	SessionSteering  string `json:"session_steering"`
	GoalAchieved     string `json:"goal_achieved"`
	Smoothness       string `json:"smoothness"`
	FrictionCause    string `json:"friction_cause"`
	Speed            string `json:"speed"`
	TokenEfficiency  string `json:"token_efficiency"`
	GoalIsCodingWork *bool  `json:"goal_is_coding_work"`
	Scores           struct {
		Overall          *int32 `json:"overall"`
		Clarity          *int32 `json:"clarity"`
		Specificity      *int32 `json:"specificity"`
		ContextProvision *int32 `json:"context_provision"`
		Actionability    *int32 `json:"actionability"`
		Completeness     *int32 `json:"completeness"`
		Efficiency       *int32 `json:"efficiency"`
	} `json:"scores"`
	InvokedSkills   []invokedSkillWire   `json:"invoked_skills"`
	HarnessFindings []harnessFindingWire `json:"harness_findings"`
}

// invokedSkillWire mirrors the R half of the Python InvokedSkillJudgment (the six
// fields R emits; the E producer fills the assessment fields on the same message).
type invokedSkillWire struct {
	InvocationRef       string `json:"invocation_ref"`
	BaselineIntent      string `json:"baseline_intent"`
	InputContract       string `json:"input_contract"`
	ContractSatisfied   *bool  `json:"contract_satisfied"`
	Contribution        string `json:"contribution"`
	EvidencePromptIndex int32  `json:"evidence_prompt_index"`
}

// harnessFindingWire mirrors the Python HarnessFinding.
type harnessFindingWire struct {
	PreferenceSignal    string  `json:"preference_signal"`
	AgentDefault        string  `json:"agent_default"`
	AuthorPreference    string  `json:"author_preference"`
	BothValid           bool    `json:"both_valid"`
	Attribution         string  `json:"attribution"`
	InvokedSkill        *string `json:"invoked_skill"`
	ArtifactType        string  `json:"artifact_type"`
	ArtifactRelation    string  `json:"artifact_relation"`
	PhaseRef            int32   `json:"phase_ref"`
	PhaseImpact         string  `json:"phase_impact"`
	VisibleVia          string  `json:"visible_via"`
	EvidencePromptIndex int32   `json:"evidence_prompt_index"`
}

// securitySlugsKebab is the set of prompt-issue slugs that reconcile into
// security_flags — the security-layer readout W emits inside turn prompt_issues (there
// is no top-level security block).
var securitySlugsKebab = map[string]bool{
	"api-key-leak": true, "db-credential-leak": true, "pii-injection": true,
}

// sdlcStageByLabel maps the Python SdlcStage StrEnum VALUES (Title-case labels with
// symbols, e.g. "Product Spec", "Test & QA", "CI/CD") to the proto enum. The generic
// vendorEnum uppercase-and-prefix path cannot reach these — the labels carry spaces and
// slashes — so this one enum needs an explicit table (the LLM output contract,
// Producer R phase contract).
var sdlcStageByLabel = map[string]sessionintelligencepb.SdlcStage{
	"Product Spec": sessionintelligencepb.SdlcStage_SDLC_STAGE_PRODUCT_SPEC,
	"Tech Spec":    sessionintelligencepb.SdlcStage_SDLC_STAGE_TECH_SPEC,
	"Plan":         sessionintelligencepb.SdlcStage_SDLC_STAGE_PLAN,
	"Code":         sessionintelligencepb.SdlcStage_SDLC_STAGE_CODE,
	"Review":       sessionintelligencepb.SdlcStage_SDLC_STAGE_REVIEW,
	"Test & QA":    sessionintelligencepb.SdlcStage_SDLC_STAGE_TEST_AND_QA,
	"CI/CD":        sessionintelligencepb.SdlcStage_SDLC_STAGE_CI_CD,
	"Deploy":       sessionintelligencepb.SdlcStage_SDLC_STAGE_DEPLOY,
	"Operate":      sessionintelligencepb.SdlcStage_SDLC_STAGE_OPERATE,
}

// parseReadResponse decodes W's analyze_read response into the typed ReadResult: the
// session verdict, the judged turns (each linked to its phase by prompt-range), the
// phases, and the security-layer prompt issues extracted from the turns.
func parseReadResponse(result string) (*ReadResult, error) {
	if result == "" {
		return &ReadResult{}, nil
	}
	var wire readWire
	if err := json.Unmarshal([]byte(result), &wire); err != nil {
		return nil, fmt.Errorf("unmarshal analyze_read result: %w", err)
	}

	verdict := SessionVerdict{
		SessionSteering:       vendorEnum("SESSION_STEERING_", wire.SessionAnalysis.SessionSteering, sessionintelligencepb.SessionSteering_value, func(v int32) sessionintelligencepb.SessionSteering { return sessionintelligencepb.SessionSteering(v) }),
		GoalAchieved:          vendorEnum("GOAL_ACHIEVED_", wire.SessionAnalysis.GoalAchieved, sessionintelligencepb.GoalAchieved_value, func(v int32) sessionintelligencepb.GoalAchieved { return sessionintelligencepb.GoalAchieved(v) }),
		Smoothness:            vendorEnum("SMOOTHNESS_", wire.SessionAnalysis.Smoothness, sessionintelligencepb.Smoothness_value, func(v int32) sessionintelligencepb.Smoothness { return sessionintelligencepb.Smoothness(v) }),
		FrictionCause:         vendorEnum("FRICTION_CAUSE_", wire.SessionAnalysis.FrictionCause, sessionintelligencepb.FrictionCause_value, func(v int32) sessionintelligencepb.FrictionCause { return sessionintelligencepb.FrictionCause(v) }),
		Speed:                 vendorEnum("SPEED_", wire.SessionAnalysis.Speed, sessionintelligencepb.Speed_value, func(v int32) sessionintelligencepb.Speed { return sessionintelligencepb.Speed(v) }),
		TokenEfficiency:       vendorEnum("TOKEN_EFFICIENCY_", wire.SessionAnalysis.TokenEfficiency, sessionintelligencepb.TokenEfficiency_value, func(v int32) sessionintelligencepb.TokenEfficiency { return sessionintelligencepb.TokenEfficiency(v) }),
		GoalIsCodingWork:      wire.SessionAnalysis.GoalIsCodingWork,
		ScoreOverall:          wire.SessionAnalysis.Scores.Overall,
		ScoreClarity:          wire.SessionAnalysis.Scores.Clarity,
		ScoreSpecificity:      wire.SessionAnalysis.Scores.Specificity,
		ScoreContextProvision: wire.SessionAnalysis.Scores.ContextProvision,
		ScoreActionability:    wire.SessionAnalysis.Scores.Actionability,
		ScoreCompleteness:     wire.SessionAnalysis.Scores.Completeness,
		ScoreEfficiency:       wire.SessionAnalysis.Scores.Efficiency,
	}
	if wire.SessionAnalysis.UnderlyingGoal != "" {
		verdict.UnderlyingGoal = &wire.SessionAnalysis.UnderlyingGoal
	}
	verdict.InvokedSkills = buildInvokedSkills(wire.SessionAnalysis.InvokedSkills)
	verdict.HarnessFindings = buildHarnessFindings(wire.SessionAnalysis.HarnessFindings)

	phases := make([]*PhaseJudgment, 0, len(wire.Phases))
	for ordinal, ph := range wire.Phases {
		start, end := ph.PromptRange.StartIndex, ph.PromptRange.EndIndex
		boundaryOverridden := ph.BoundaryOverridden
		phase := &PhaseJudgment{
			PhaseOrdinal: int32(ordinal),
			RangeStart:   &start,
			RangeEnd:     &end,
			// turn_count is the phase's inclusive prompt-index span — derived here (not emitted by
			// the LLM) so it is R-owned and round-trips a re-pick unchanged.
			TurnCount:          phaseTurnCountFromRange(start, end),
			BoundaryOverridden: &boundaryOverridden,
			OverrideReason:     ph.OverrideReason,
			PhaseTheme:         vendorEnum("PHASE_THEME_", ph.PhaseTheme, sessionintelligencepb.PhaseTheme_value, func(v int32) sessionintelligencepb.PhaseTheme { return sessionintelligencepb.PhaseTheme(v) }),
			SdlcStageSet:       buildSdlcStageSet(ph.SdlcStage),
			TaskComplexity:     vendorEnum("TASK_COMPLEXITY_", ph.TaskComplexity, sessionintelligencepb.TaskComplexity_value, func(v int32) sessionintelligencepb.TaskComplexity { return sessionintelligencepb.TaskComplexity(v) }),
			UserSteering: vendorEnum("PHASE_USER_STEERING_", ph.UserSteering, sessionintelligencepb.PhaseUserSteering_value, func(v int32) sessionintelligencepb.PhaseUserSteering {
				return sessionintelligencepb.PhaseUserSteering(v)
			}),
			PhaseObjective: ph.PhaseObjective,
			PhaseObjectiveMet: vendorEnum("PHASE_OBJECTIVE_MET_", ph.PhaseObjectiveMet, sessionintelligencepb.PhaseObjectiveMet_value, func(v int32) sessionintelligencepb.PhaseObjectiveMet {
				return sessionintelligencepb.PhaseObjectiveMet(v)
			}),
			AgentHelpfulness: vendorEnum("AGENT_HELPFULNESS_", ph.AgentHelpfulness, sessionintelligencepb.AgentHelpfulness_value, func(v int32) sessionintelligencepb.AgentHelpfulness { return sessionintelligencepb.AgentHelpfulness(v) }),
			Smoothness:       vendorEnum("SMOOTHNESS_", ph.Smoothness, sessionintelligencepb.Smoothness_value, func(v int32) sessionintelligencepb.Smoothness { return sessionintelligencepb.Smoothness(v) }),
			FrictionCause:    vendorEnum("FRICTION_CAUSE_", ph.FrictionCause, sessionintelligencepb.FrictionCause_value, func(v int32) sessionintelligencepb.FrictionCause { return sessionintelligencepb.FrictionCause(v) }),
			Speed:            vendorEnum("SPEED_", ph.Speed, sessionintelligencepb.Speed_value, func(v int32) sessionintelligencepb.Speed { return sessionintelligencepb.Speed(v) }),
			WentWell:         stringList(ph.WentWell),
			WentWrong:        stringList(ph.WentWrong),
			Coaching:         buildPhaseCoaching(ph.CouldBeBetter),
		}
		phases = append(phases, phase)
	}
	// work_type is the dominant phase's theme (the phase spanning the most prompts),
	// materialized onto the session row.
	verdict.WorkType = dominantPhaseTheme(wire.Phases)

	turns := make([]*TurnJudgment, 0, len(wire.Turns))
	var security []*sessionintelligencepb.PromptIssue
	for _, tw := range wire.Turns {
		issues := make([]*sessionintelligencepb.PromptIssue, 0, len(tw.PromptIssues))
		for _, iw := range tw.PromptIssues {
			issue := &sessionintelligencepb.PromptIssue{
				Slug:     slugToEnum(iw.Slug),
				Severity: vendorEnumValue("ISSUE_SEVERITY_", iw.Severity, sessionintelligencepb.IssueSeverity_value, func(v int32) sessionintelligencepb.IssueSeverity { return sessionintelligencepb.IssueSeverity(v) }),
			}
			if iw.Note != nil {
				issue.Note = *iw.Note
			}
			if securitySlugsKebab[iw.Slug] {
				issue.Layer = "security"
				security = append(security, issue)
			}
			issues = append(issues, issue)
		}
		turn := &TurnJudgment{
			PromptIndex:  tw.PromptIndex,
			PhaseOrdinal: phaseOrdinalFor(tw.PromptIndex, wire.Phases),
			PromptType:   vendorEnum("PROMPT_TYPE_", tw.PromptType, sessionintelligencepb.PromptType_value, func(v int32) sessionintelligencepb.PromptType { return sessionintelligencepb.PromptType(v) }),
			UserSteering: ptrVendorEnum("TURN_USER_STEERING_", tw.UserSteering, sessionintelligencepb.TurnUserSteering_value, func(v int32) sessionintelligencepb.TurnUserSteering { return sessionintelligencepb.TurnUserSteering(v) }),
			PromptCompleteness: ptrVendorEnum("PROMPT_COMPLETENESS_", tw.PromptCompleteness, sessionintelligencepb.PromptCompleteness_value, func(v int32) sessionintelligencepb.PromptCompleteness {
				return sessionintelligencepb.PromptCompleteness(v)
			}),
			PromptObjectiveMet: vendorEnum("PROMPT_OBJECTIVE_MET_", tw.PromptObjectiveMet, sessionintelligencepb.PromptObjectiveMet_value, func(v int32) sessionintelligencepb.PromptObjectiveMet {
				return sessionintelligencepb.PromptObjectiveMet(v)
			}),
			AgentHelpfulness: vendorEnum("AGENT_HELPFULNESS_", tw.AgentHelpfulness, sessionintelligencepb.AgentHelpfulness_value, func(v int32) sessionintelligencepb.AgentHelpfulness { return sessionintelligencepb.AgentHelpfulness(v) }),
			AgentInitiative:  vendorEnum("AGENT_INITIATIVE_", tw.AgentInitiative, sessionintelligencepb.AgentInitiative_value, func(v int32) sessionintelligencepb.AgentInitiative { return sessionintelligencepb.AgentInitiative(v) }),
			ToolUse:          vendorEnum("TOOL_USE_", tw.ToolUse, sessionintelligencepb.ToolUse_value, func(v int32) sessionintelligencepb.ToolUse { return sessionintelligencepb.ToolUse(v) }),
			WasRetry:         ptrVendorEnum("WAS_RETRY_", tw.WasRetry, sessionintelligencepb.WasRetry_value, func(v int32) sessionintelligencepb.WasRetry { return sessionintelligencepb.WasRetry(v) }),
			FailureClass:     ptrVendorEnum("FAILURE_CLASS_", tw.FailureClass, sessionintelligencepb.FailureClass_value, func(v int32) sessionintelligencepb.FailureClass { return sessionintelligencepb.FailureClass(v) }),
			TokenLocus:       vendorEnum("TOKEN_LOCUS_", tw.TokenLocus, sessionintelligencepb.TokenLocus_value, func(v int32) sessionintelligencepb.TokenLocus { return sessionintelligencepb.TokenLocus(v) }),
			WasteCause:       ptrVendorEnum("WASTE_CAUSE_", tw.WasteCause, sessionintelligencepb.WasteCause_value, func(v int32) sessionintelligencepb.WasteCause { return sessionintelligencepb.WasteCause(v) }),
			DecisionType:     ptrVendorEnum("DECISION_TYPE_", tw.DecisionType, sessionintelligencepb.DecisionType_value, func(v int32) sessionintelligencepb.DecisionType { return sessionintelligencepb.DecisionType(v) }),
			DownstreamImpact: ptrVendorEnum("DOWNSTREAM_IMPACT_", tw.DownstreamImpact, sessionintelligencepb.DownstreamImpact_value, func(v int32) sessionintelligencepb.DownstreamImpact { return sessionintelligencepb.DownstreamImpact(v) }),
			FeedbackTarget:   ptrVendorEnum("FEEDBACK_TARGET_", tw.FeedbackTarget, sessionintelligencepb.FeedbackTarget_value, func(v int32) sessionintelligencepb.FeedbackTarget { return sessionintelligencepb.FeedbackTarget(v) }),
			FeedbackDisposition: ptrVendorEnum("FEEDBACK_DISPOSITION_", tw.FeedbackDisposition, sessionintelligencepb.FeedbackDisposition_value, func(v int32) sessionintelligencepb.FeedbackDisposition {
				return sessionintelligencepb.FeedbackDisposition(v)
			}),
			ReferencedArtifact:   tw.ReferencedArtifact,
			ReferencedFindingIDs: stringList(tw.ReferencedFindingIDs),
			EvidenceSeq:          tw.EvidenceSeq,
		}
		if len(issues) > 0 {
			turn.PromptIssues = &sessionintelligencepb.PromptIssueList{Items: issues}
		}
		turns = append(turns, turn)
	}

	return &ReadResult{Verdict: verdict, Turns: turns, Phases: phases, SecurityIssues: security}, nil
}

// discardWire is the analyze_discard response — one object, one key. An empty array is
// the common, valid answer (the LLM output contract).
type discardWire struct {
	DiscardedRanges []discardedRangeWire `json:"discarded_ranges"`
}

type discardedRangeWire struct {
	PromptRange struct {
		StartIndex int32 `json:"start_index"`
		EndIndex   int32 `json:"end_index"`
	} `json:"prompt_range"`
	Reason              string `json:"reason"`
	EvidencePromptIndex int32  `json:"evidence_prompt_index"`
}

// parseDiscardResponse decodes D's analyze_discard response into the discarded ranges
// (both prompt_range bounds inclusive). token_sum is left zero — code sums the measured
// per-turn tokens over each range at commit time, never the model.
func parseDiscardResponse(result string) (*DiscardResult, error) {
	if result == "" {
		return &DiscardResult{}, nil
	}
	var wire discardWire
	if err := json.Unmarshal([]byte(result), &wire); err != nil {
		return nil, fmt.Errorf("unmarshal analyze_discard result: %w", err)
	}
	ranges := make([]*sessionintelligencepb.DiscardedRange, 0, len(wire.DiscardedRanges))
	for _, dr := range wire.DiscardedRanges {
		ranges = append(ranges, &sessionintelligencepb.DiscardedRange{
			RangeStart:          dr.PromptRange.StartIndex,
			RangeEnd:            dr.PromptRange.EndIndex,
			EvidencePromptIndex: dr.EvidencePromptIndex,
			Reason:              dr.Reason,
		})
	}
	out := &DiscardResult{}
	if len(ranges) > 0 {
		out.DiscardedRanges = &sessionintelligencepb.DiscardedRangeList{Items: ranges}
	}
	return out, nil
}

// The analyze_evidence response wire shape — the deployed reference-llm
// AnalyzeEvidenceResponse (app/llm/schemas/session_analysis.py): one object, three keys
// (artifacts, skill_assessments, round_trips), every enum a LOWERCASE StrEnum value. Every
// field the schema defines is mirrored here so a wire field is never silently dropped.
type evidenceWire struct {
	Artifacts        []artifactAdjudicationWire `json:"artifacts"`
	SkillAssessments []skillAssessmentWire      `json:"skill_assessments"`
	RoundTrips       []roundTripWire            `json:"round_trips"`
}

// artifactAdjudicationWire mirrors the Python ArtifactAdjudication. findings[] is a
// WORKING list the model emits and code aggregates into disposition counts (the rows are
// discarded); contract_checks[] is carried through per gate.
type artifactAdjudicationWire struct {
	ArtifactRef        string                `json:"artifact_ref"`
	ContractChecks     []contractCheckWire   `json:"contract_checks"`
	Findings           []artifactFindingWire `json:"findings"`
	HumanSignalPresent bool                  `json:"human_signal_present"`
	OverallAttribution string                `json:"overall_attribution"`
}

type contractCheckWire struct {
	GateRef                  string `json:"gate_ref"`
	ClauseIsARealRequirement bool   `json:"clause_is_a_real_requirement"`
	EvidenceSeq              int32  `json:"evidence_seq"`
}

type artifactFindingWire struct {
	FindingID           string `json:"finding_id"`
	Severity            string `json:"severity"`
	Disposition         string `json:"disposition"`
	Attribution         string `json:"attribution"`
	EvidencePromptIndex int32  `json:"evidence_prompt_index"`
}

// skillAssessmentWire mirrors the Python SkillAssessment — E's six adjudicated dimensions
// on one invocation, keyed by invocation_ref to the invoked_skill R emitted.
type skillAssessmentWire struct {
	InvocationRef        string `json:"invocation_ref"`
	Role                 string `json:"role"`
	ContractCompliance   string `json:"contract_compliance"`
	OutputQuality        string `json:"output_quality"`
	ExecutionReliability string `json:"execution_reliability"`
	HumanAlignment       string `json:"human_alignment"`
	Verdict              string `json:"verdict"`
	EvidenceSeq          *int32 `json:"evidence_seq"`
}

// roundTripWire mirrors the Python RoundTrip — one per supplied pair. prompt_index is P's
// index (always P, never Q); evidence_prompt_index may be either. prompt_completeness and
// issue are mirrored so the wire contract is complete, but the v1 turn grain carries only
// round_trip_fault / round_trip_evidence_prompt_index — the two the data model defines — so
// those two are decoded and the rest are not stored.
type roundTripWire struct {
	PromptIndex         int32               `json:"prompt_index"`
	Fault               string              `json:"fault"`
	PromptCompleteness  string              `json:"prompt_completeness"`
	Issue               *roundTripIssueWire `json:"issue"`
	EvidencePromptIndex *int32              `json:"evidence_prompt_index"`
}

// roundTripIssueWire mirrors the Python RoundTripIssue (non-null only on prompt/mixed
// faults). A security-layer slug here (an api-key / credential leak or pii injection E alone
// caught on the round trip) routes into security_flags via RoundTripJudgment.SecurityIssue
// ; a non-security issue is parsed for contract
// completeness but not stored (the v1 turn grain carries no round-trip issue column).
type roundTripIssueWire struct {
	Slug     string  `json:"slug"`
	Layer    *string `json:"layer"`
	Severity string  `json:"severity"`
	Note     *string `json:"note"`
}

// parseEvidenceResponse decodes E's analyze_evidence response into the typed
// EvidenceResult: the artifact adjudications (findings aggregated into disposition counts,
// gate clause-not-real counted from the contract checks), the per-invocation skill
// assessments, and the per-pair round-trip fault attributions. An empty result envelope
// is valid (E fired but adjudicated nothing).
func parseEvidenceResponse(result string) (*EvidenceResult, error) {
	if result == "" {
		return &EvidenceResult{}, nil
	}
	var wire evidenceWire
	if err := json.Unmarshal([]byte(result), &wire); err != nil {
		return nil, fmt.Errorf("unmarshal analyze_evidence result: %w", err)
	}

	adjudications := make([]*sessionintelligencepb.ArtifactAdjudication, 0, len(wire.Artifacts))
	for _, aw := range wire.Artifacts {
		adjudications = append(adjudications, buildArtifactAdjudication(aw))
	}

	skills := make([]*SkillAssessment, 0, len(wire.SkillAssessments))
	for _, sw := range wire.SkillAssessments {
		skills = append(skills, &SkillAssessment{
			InvocationRef: sw.InvocationRef,
			Role:          vendorEnum("SKILL_ROLE_", sw.Role, sessionintelligencepb.SkillRole_value, func(v int32) sessionintelligencepb.SkillRole { return sessionintelligencepb.SkillRole(v) }),
			ContractCompliance: vendorEnum("CONTRACT_COMPLIANCE_", sw.ContractCompliance, sessionintelligencepb.ContractCompliance_value, func(v int32) sessionintelligencepb.ContractCompliance {
				return sessionintelligencepb.ContractCompliance(v)
			}),
			OutputQuality: vendorEnum("OUTPUT_QUALITY_", sw.OutputQuality, sessionintelligencepb.OutputQuality_value, func(v int32) sessionintelligencepb.OutputQuality { return sessionintelligencepb.OutputQuality(v) }),
			ExecutionReliability: vendorEnum("EXECUTION_RELIABILITY_", sw.ExecutionReliability, sessionintelligencepb.ExecutionReliability_value, func(v int32) sessionintelligencepb.ExecutionReliability {
				return sessionintelligencepb.ExecutionReliability(v)
			}),
			HumanAlignment: vendorEnum("HUMAN_ALIGNMENT_", sw.HumanAlignment, sessionintelligencepb.HumanAlignment_value, func(v int32) sessionintelligencepb.HumanAlignment { return sessionintelligencepb.HumanAlignment(v) }),
			SkillVerdict:   vendorEnum("SKILL_VERDICT_", sw.Verdict, sessionintelligencepb.SkillVerdict_value, func(v int32) sessionintelligencepb.SkillVerdict { return sessionintelligencepb.SkillVerdict(v) }),
			EvidenceSeq:    sw.EvidenceSeq,
		})
	}

	roundTrips := make([]*RoundTripJudgment, 0, len(wire.RoundTrips))
	for _, rw := range wire.RoundTrips {
		roundTrips = append(roundTrips, &RoundTripJudgment{
			PromptIndex:         rw.PromptIndex,
			Fault:               vendorEnum("ROUND_TRIP_FAULT_", rw.Fault, sessionintelligencepb.RoundTripFault_value, func(v int32) sessionintelligencepb.RoundTripFault { return sessionintelligencepb.RoundTripFault(v) }),
			EvidencePromptIndex: rw.EvidencePromptIndex,
			SecurityIssue:       buildRoundTripSecurityIssue(rw.Issue),
		})
	}

	out := &EvidenceResult{SkillAssessments: skills, RoundTrips: roundTrips}
	if len(adjudications) > 0 {
		out.Adjudications = &sessionintelligencepb.ArtifactAdjudicationList{Items: adjudications}
	}
	return out, nil
}

// buildArtifactAdjudication maps one wire adjudication to the proto message, aggregating
// the WORKING findings[] into per-disposition counts and the contract checks into the gate
// tallies. gates_violated is left zero: the gate pass/fail status comes from the spine's
// contract-gate-check ledger, which the model does not carry — the model supplies the
// clause-is-a-real-requirement judgment, code carries the status.
func buildArtifactAdjudication(aw artifactAdjudicationWire) *sessionintelligencepb.ArtifactAdjudication {
	adj := &sessionintelligencepb.ArtifactAdjudication{
		ArtifactRef:        aw.ArtifactRef,
		HumanSignalPresent: aw.HumanSignalPresent,
		GatesTotal:         int32(len(aw.ContractChecks)),
	}
	if oa := vendorEnum("OVERALL_ATTRIBUTION_", aw.OverallAttribution, sessionintelligencepb.OverallAttribution_value, func(v int32) sessionintelligencepb.OverallAttribution {
		return sessionintelligencepb.OverallAttribution(v)
	}); oa != nil {
		adj.OverallAttribution = *oa
	}
	for _, cw := range aw.ContractChecks {
		adj.ContractChecks = append(adj.ContractChecks, &sessionintelligencepb.ContractCheck{
			GateRef:                  cw.GateRef,
			ClauseIsARealRequirement: cw.ClauseIsARealRequirement,
			EvidenceSeq:              cw.EvidenceSeq,
		})
		if !cw.ClauseIsARealRequirement {
			adj.GatesClauseNotReal++
		}
	}
	for _, fw := range aw.Findings {
		switch fw.Disposition {
		case "accepted":
			adj.FindingsAccepted++
		case "rejected":
			adj.FindingsRejected++
		case "modified":
			adj.FindingsModified++
		case "deferred":
			adj.FindingsDeferred++
		case "unresolved":
			adj.FindingsUnresolved++
		}
	}
	return adj
}

// buildRoundTripSecurityIssue maps a round trip's issue to a security-layer PromptIssue when
// its slug is one of the security taxonomy slugs, else nil (a prompt/taste issue is not stored).
// The Layer is stamped "security" so the commit reconcile treats it exactly like R's security
// readout, differing only in the E source bit + fingerprint it carries.
func buildRoundTripSecurityIssue(issue *roundTripIssueWire) *sessionintelligencepb.PromptIssue {
	if issue == nil || !securitySlugsKebab[issue.Slug] {
		return nil
	}
	pi := &sessionintelligencepb.PromptIssue{
		Slug:     slugToEnum(issue.Slug),
		Severity: vendorEnumValue("ISSUE_SEVERITY_", issue.Severity, sessionintelligencepb.IssueSeverity_value, func(v int32) sessionintelligencepb.IssueSeverity { return sessionintelligencepb.IssueSeverity(v) }),
		Layer:    "security",
	}
	if issue.Note != nil {
		pi.Note = *issue.Note
	}
	return pi
}

// buildSdlcStageSet maps the phase's Title-case SDLC labels to the proto set, dropping
// any label outside the closed vocabulary. Returns nil for an empty/all-unknown set.
func buildSdlcStageSet(labels []string) *sessionintelligencepb.SdlcStageSet {
	stages := make([]sessionintelligencepb.SdlcStage, 0, len(labels))
	for _, label := range labels {
		if stage, ok := sdlcStageByLabel[label]; ok {
			stages = append(stages, stage)
		}
	}
	if len(stages) == 0 {
		return nil
	}
	return &sessionintelligencepb.SdlcStageSet{Stages: stages}
}

// buildPhaseCoaching maps R's could_be_better notes to the PhaseCoaching JSONB list.
func buildPhaseCoaching(notes []couldBeBetterWire) *sessionintelligencepb.PhaseCoachingList {
	if len(notes) == 0 {
		return nil
	}
	items := make([]*sessionintelligencepb.PhaseCoaching, 0, len(notes))
	for _, note := range notes {
		coaching := &sessionintelligencepb.PhaseCoaching{
			RootCause:           note.RootCause,
			Suggestion:          note.Suggestion,
			EvidencePromptIndex: note.EvidencePromptIndex,
		}
		if lever := vendorEnum("COACHING_LEVER_", note.Lever, sessionintelligencepb.CoachingLever_value, func(v int32) sessionintelligencepb.CoachingLever { return sessionintelligencepb.CoachingLever(v) }); lever != nil {
			coaching.Lever = *lever
		}
		if note.Target != nil {
			coaching.Target = *note.Target
		}
		for _, axis := range note.Axes {
			if a := vendorEnum("COACHING_AXIS_", axis, sessionintelligencepb.CoachingAxis_value, func(v int32) sessionintelligencepb.CoachingAxis { return sessionintelligencepb.CoachingAxis(v) }); a != nil {
				coaching.Axes = append(coaching.Axes, *a)
			}
		}
		items = append(items, coaching)
	}
	return &sessionintelligencepb.PhaseCoachingList{Items: items}
}

// buildInvokedSkills maps R's invoked-skill judgments to the JSONB list — only the six
// fields R emits; the E producer fills the assessment fields on the same message later.
func buildInvokedSkills(skills []invokedSkillWire) *sessionintelligencepb.InvokedSkillJudgmentList {
	if len(skills) == 0 {
		return nil
	}
	items := make([]*sessionintelligencepb.InvokedSkillJudgment, 0, len(skills))
	for _, skill := range skills {
		item := &sessionintelligencepb.InvokedSkillJudgment{
			InvocationRef:       skill.InvocationRef,
			BaselineIntent:      skill.BaselineIntent,
			Contribution:        skill.Contribution,
			EvidencePromptIndex: skill.EvidencePromptIndex,
		}
		if ic := vendorEnum("INPUT_CONTRACT_", skill.InputContract, sessionintelligencepb.InputContract_value, func(v int32) sessionintelligencepb.InputContract { return sessionintelligencepb.InputContract(v) }); ic != nil {
			item.InputContract = *ic
		}
		item.ContractSatisfied = skill.ContractSatisfied
		items = append(items, item)
	}
	return &sessionintelligencepb.InvokedSkillJudgmentList{Items: items}
}

// buildHarnessFindings maps R's harness findings to the JSONB list.
func buildHarnessFindings(findings []harnessFindingWire) *sessionintelligencepb.HarnessFindingList {
	if len(findings) == 0 {
		return nil
	}
	items := make([]*sessionintelligencepb.HarnessFinding, 0, len(findings))
	for _, finding := range findings {
		item := &sessionintelligencepb.HarnessFinding{
			PreferenceSignal:    finding.PreferenceSignal,
			AgentDefault:        finding.AgentDefault,
			AuthorPreference:    finding.AuthorPreference,
			BothValid:           finding.BothValid,
			PhaseRef:            finding.PhaseRef,
			EvidencePromptIndex: finding.EvidencePromptIndex,
		}
		if finding.InvokedSkill != nil {
			item.InvokedSkill = *finding.InvokedSkill
		}
		if attr := vendorEnum("FINDING_ATTRIBUTION_KIND_", finding.Attribution, sessionintelligencepb.FindingAttributionKind_value, func(v int32) sessionintelligencepb.FindingAttributionKind {
			return sessionintelligencepb.FindingAttributionKind(v)
		}); attr != nil {
			item.Attribution = *attr
		}
		if at := vendorEnum("HARNESS_ARTIFACT_TYPE_", finding.ArtifactType, sessionintelligencepb.HarnessArtifactType_value, func(v int32) sessionintelligencepb.HarnessArtifactType {
			return sessionintelligencepb.HarnessArtifactType(v)
		}); at != nil {
			item.ArtifactType = *at
		}
		if ar := vendorEnum("ARTIFACT_RELATION_", finding.ArtifactRelation, sessionintelligencepb.ArtifactRelation_value, func(v int32) sessionintelligencepb.ArtifactRelation { return sessionintelligencepb.ArtifactRelation(v) }); ar != nil {
			item.ArtifactRelation = *ar
		}
		if pi := vendorEnum("PHASE_IMPACT_", finding.PhaseImpact, sessionintelligencepb.PhaseImpact_value, func(v int32) sessionintelligencepb.PhaseImpact { return sessionintelligencepb.PhaseImpact(v) }); pi != nil {
			item.PhaseImpact = *pi
		}
		if vv := vendorEnum("VISIBLE_VIA_", finding.VisibleVia, sessionintelligencepb.VisibleVia_value, func(v int32) sessionintelligencepb.VisibleVia { return sessionintelligencepb.VisibleVia(v) }); vv != nil {
			item.VisibleVia = *vv
		}
		items = append(items, item)
	}
	return &sessionintelligencepb.HarnessFindingList{Items: items}
}

// stringList wraps a non-empty string slice as a JSONB StringList, returning nil for an
// empty slice so the column stays NULL rather than storing an empty list.
func stringList(values []string) *sessionintelligencepb.StringList {
	if len(values) == 0 {
		return nil
	}
	return &sessionintelligencepb.StringList{Values: values}
}

// slugToEnum maps a kebab-case prompt-issue slug to its proto enum, returning
// UNSPECIFIED for a slug outside the closed vocabulary (W's slug is a free string).
func slugToEnum(kebab string) sessionintelligencepb.PromptIssueSlug {
	name := "PROMPT_ISSUE_SLUG_" + strings.ToUpper(strings.ReplaceAll(kebab, "-", "_"))
	return sessionintelligencepb.PromptIssueSlug(sessionintelligencepb.PromptIssueSlug_value[name])
}

// ptrVendorEnum is vendorEnum for a nullable wire field: nil in → nil out.
func ptrVendorEnum[T ~int32](prefix string, value *string, valueMap map[string]int32, mk func(int32) T) *T {
	if value == nil {
		return nil
	}
	return vendorEnum(prefix, *value, valueMap, mk)
}

// phaseOrdinalFor returns the ordinal of the phase whose prompt range contains the
// given prompt index, or nil when no phase claims it.
func phaseOrdinalFor(promptIndex int32, phases []readPhaseWire) *int32 {
	for ordinal, ph := range phases {
		if promptIndex >= ph.PromptRange.StartIndex && promptIndex <= ph.PromptRange.EndIndex {
			result := int32(ordinal)
			return &result
		}
	}
	return nil
}

// dominantPhaseTheme returns the theme of the phase spanning the most prompts (the
// materialized session work_type), or nil when there are no phases.
func dominantPhaseTheme(phases []readPhaseWire) *sessionintelligencepb.PhaseTheme {
	var best *readPhaseWire
	var bestSpan int32 = -1
	for i := range phases {
		span := phases[i].PromptRange.EndIndex - phases[i].PromptRange.StartIndex
		if span > bestSpan {
			bestSpan = span
			best = &phases[i]
		}
	}
	if best == nil {
		return nil
	}
	return vendorEnum("PHASE_THEME_", best.PhaseTheme, sessionintelligencepb.PhaseTheme_value, func(v int32) sessionintelligencepb.PhaseTheme { return sessionintelligencepb.PhaseTheme(v) })
}
