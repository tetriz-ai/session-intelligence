// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package analyze is the single-session public front door to the session-intelligence engine: bring
// one coding-agent transcript and your own LLM provider, and get the same structured analysis the
// batch pipeline produces — a session verdict, judged turns, phases, outcome signals and security
// flags — with no database, no S3 and no per-org configuration.
//
// AnalyzeSession(ctx, *Conversation, Options) over ONE conversation is the primary entry. An
// internal, Postgres-backed batch path drains unprocessed records over the same authored pipeline;
// this package is its single-tenant, in-memory sibling.
package analyze

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// Options configures a single AnalyzeSession run. Only Provider is required; the rest have
// single-tenant, everything-on defaults.
type Options struct {
	// Provider runs the read/evidence/discard LLM passes. Required.
	Provider Provider
	// Detector optionally scores round-trip pairs for E's round-trip half. Leave it the zero
	// interface value to disable it. A typed nil (e.g. `var d *myDetector; opts.Detector = d`) is
	// also treated as disabled: it is normalized to off rather than panicking when scored.
	Detector RoundTripDetector

	// Identifiers stamped onto the analysis. All optional — sensible defaults are filled in
	// (SessionID falls back to the conversation's own session id, then "session").
	//
	// OrgID becomes a Prometheus metric label on the internal pipeline counters, so keep it
	// LOW-CARDINALITY (a tenant/org identifier, not a per-request or per-user unique value) to avoid
	// unbounded metric series.
	OrgID     string
	PersonID  string
	SessionID string
	RepoName  string

	// DisableEvidence / DisableDiscard turn off the conditional E and D passes (both run by
	// default; each still only fires on its own spine signal).
	DisableEvidence bool
	DisableDiscard  bool
	// UseSystemPromptCache opts every provider call into system-prompt caching where the provider
	// supports it (currently the Gemini provider honors it; other providers ignore it).
	UseSystemPromptCache bool
}

// Analysis is the clean, public result of analysing one session — translated from the engine's
// internal row model so no raw proto leaks as the primary surface. Enum-valued fields are rendered as
// their short upper-snake names (e.g. "ACHIEVED"); an unresolved signal is the empty string.
type Analysis struct {
	SessionID     string         `json:"session_id"`
	IdeType       string         `json:"ide_type"`
	Disposition   string         `json:"disposition"` // PRODUCED or PARTIAL
	Verdict       Verdict        `json:"verdict"`
	Measured      Measured       `json:"measured"`
	Turns         []Turn         `json:"turns"`
	Phases        []Phase        `json:"phases"`
	SecurityFlags []SecurityFlag `json:"security_flags"`
}

// Verdict is R's session-grain judgment.
type Verdict struct {
	UnderlyingGoal   string `json:"underlying_goal"`
	GoalIsCodingWork *bool  `json:"goal_is_coding_work"`
	WorkType         string `json:"work_type"`
	SessionSteering  string `json:"session_steering"`
	GoalAchieved     string `json:"goal_achieved"`
	Smoothness       string `json:"smoothness"`
	FrictionCause    string `json:"friction_cause"`
	Speed            string `json:"speed"`
	TokenEfficiency  string `json:"token_efficiency"`

	ScoreOverall          *int32 `json:"score_overall"`
	ScoreClarity          *int32 `json:"score_clarity"`
	ScoreSpecificity      *int32 `json:"score_specificity"`
	ScoreContextProvision *int32 `json:"score_context_provision"`
	ScoreActionability    *int32 `json:"score_actionability"`
	ScoreCompleteness     *int32 `json:"score_completeness"`
	ScoreEfficiency       *int32 `json:"score_efficiency"`
}

// Measured is the code-derived (no-LLM) accounting for the session.
type Measured struct {
	TotalPrompts     *int32   `json:"total_prompts"`
	TurnCount        *int32   `json:"turn_count"`
	HumanTurnCount   *int32   `json:"human_turn_count"`
	DelegatedTurns   *int32   `json:"delegated_turns"`
	PhaseCount       *int32   `json:"phase_count"`
	ToolCallCount    *int32   `json:"tool_call_count"`
	TotalTokens      *int64   `json:"total_tokens"`
	WastedTokens     *int64   `json:"wasted_tokens"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd"`
	DurationSeconds  *int32   `json:"duration_seconds"`
}

// Turn is R's judgment of one human turn (a useful subset of the full turn grain).
type Turn struct {
	PromptIndex        int32           `json:"prompt_index"`
	PhaseOrdinal       *int32          `json:"phase_ordinal"`
	PromptType         string          `json:"prompt_type"`
	UserSteering       string          `json:"user_steering"`
	PromptCompleteness string          `json:"prompt_completeness"`
	PromptObjectiveMet string          `json:"prompt_objective_met"`
	AgentHelpfulness   string          `json:"agent_helpfulness"`
	AgentInitiative    string          `json:"agent_initiative"`
	ToolUse            string          `json:"tool_use"`
	FailureClass       string          `json:"failure_class"`
	WasteCause         string          `json:"waste_cause"`
	DownstreamImpact   string          `json:"downstream_impact"`
	RoundTripFault     string          `json:"round_trip_fault"`
	SecurityIssues     []SecurityIssue `json:"security_issues"`
}

// Phase is R's judgment of one session phase.
type Phase struct {
	Ordinal        int32    `json:"ordinal"`
	RangeStart     *int32   `json:"range_start"`
	RangeEnd       *int32   `json:"range_end"`
	Theme          string   `json:"theme"`
	TaskComplexity string   `json:"task_complexity"`
	UserSteering   string   `json:"user_steering"`
	Objective      string   `json:"objective"`
	ObjectiveMet   string   `json:"objective_met"`
	Smoothness     string   `json:"smoothness"`
	FrictionCause  string   `json:"friction_cause"`
	Speed          string   `json:"speed"`
	WentWell       []string `json:"went_well"`
	WentWrong      []string `json:"went_wrong"`
}

// SecurityIssue is one RAW-but-redacted per-turn security-layer finding. Its Note is passed through
// the defensive secret redactor (masking known token/email/path shapes), but it is NOT reconciled or
// deduped — it is the per-turn readout. For the session-level, deduped set use Analysis.SecurityFlags.
type SecurityIssue struct {
	Type     string `json:"type"` // kebab slug, e.g. "api-key-leak"
	Severity string `json:"severity"`
	Note     string `json:"note"` // redacted
}

// SecurityFlag is one RECONCILED session-level security finding — produced by the same reconcile the
// batch path uses (security.BuildOpenFlags): one flag per type, promoted to the highest severity seen,
// with every occurrence's excerpt masked by the defensive secret redactor. Only the three reconciled
// slugs cross the seam (api-key-leak / db-credential-leak / pii-injection); an unspecified severity is
// reported as "medium". Read and evidence findings are merged by type (there is no per-source split).
type SecurityFlag struct {
	Type        string   `json:"type"`        // kebab slug, e.g. "api-key-leak"
	Severity    string   `json:"severity"`    // low / medium / high / critical
	Occurrences []string `json:"occurrences"` // one redacted excerpt per contributing finding
}

// AnalyzeSession runs the spine → read → evidence → discard pipeline over one in-memory conversation
// and returns the structured analysis. It performs no database or S3 access; the only I/O is the
// provider's (and optional detector's) calls. Provider is required.
//
// It is all-or-nothing: if the evidence or discard pass errors, the whole call fails and R's
// completed verdict is discarded (a single-session caller gets one Analysis or an error, never a
// partial result). It returns an error WITHOUT any provider call when the conversation has no
// taggable (human) turns (ErrNoTaggableTurns), and when the rendered transcript exceeds the
// single-call size ceiling (ErrTranscriptTooLarge) — the single-session path does not window the
// read the way the batch path does, so a larger transcript must go through the batch pipeline.
func AnalyzeSession(ctx context.Context, conversation *Conversation, opts Options) (*Analysis, error) {
	if conversation == nil {
		return nil, fmt.Errorf("analyze session: conversation is nil")
	}
	if opts.Provider == nil {
		return nil, fmt.Errorf("analyze session: Options.Provider is required")
	}

	input := sessionanalysis.SingleSessionInput{
		OrgID:     defaultString(opts.OrgID, "default-org"),
		PersonID:  defaultString(opts.PersonID, "default-person"),
		SessionID: defaultString(opts.SessionID, defaultString(conversation.GetSessionId(), "session")),
		RepoName:  defaultString(opts.RepoName, conversation.GetProjectName()),
	}

	cfg := sessionanalysis.DefaultSingleSessionConfig()
	cfg.RunEvidence = !opts.DisableEvidence
	cfg.RunDiscard = !opts.DisableDiscard
	cfg.UseSystemPromptCache = opts.UseSystemPromptCache

	producer := sessionanalysis.NewContentGeneratorProducer(opts.Provider)

	// Normalize a typed-nil Detector (a non-nil interface wrapping a nil pointer) to a true nil:
	// it would otherwise pass the engine's != nil guard and panic when scored. A caller who wrote
	// `var d *myDetector; opts.Detector = d` then gets the detector-off path rather than a crash.
	detector := opts.Detector
	if isNilDetector(detector) {
		detector = nil
	}

	result, err := sessionanalysis.AnalyzeConversation(ctx, input, conversation, producer, detector, cfg)
	if err != nil {
		return nil, fmt.Errorf("analyze session %q: %w", input.SessionID, err)
	}
	return translate(input.SessionID, result), nil
}

// isNilDetector reports whether d is nil or a typed nil (a non-nil interface wrapping a nil
// pointer), so AnalyzeSession can treat either as "no detector" rather than call into it.
func isNilDetector(d RoundTripDetector) bool {
	if d == nil {
		return true
	}
	value := reflect.ValueOf(d)
	switch value.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
		return value.IsNil()
	default:
		return false
	}
}

// translate maps the engine's internal rows onto the public Analysis.
func translate(sessionID string, result *sessionanalysis.SingleSessionResult) *Analysis {
	analysisRow := result.Analysis
	analysis := &Analysis{
		SessionID:     sessionID,
		IdeType:       shortEnum(analysisRow.IdeType, "IDE_TYPE_"),
		Disposition:   shortEnum(analysisRow.JudgedDisposition, "JUDGED_DISPOSITION_"),
		Verdict:       translateVerdict(analysisRow),
		Measured:      translateMeasured(analysisRow),
		Turns:         translateTurns(result.Read),
		Phases:        translatePhases(result.Read),
		SecurityFlags: translateSecurityFlags(result.SecurityFlags),
	}
	return analysis
}

func translateVerdict(row *model.SessionAnalysis) Verdict {
	verdict := Verdict{
		WorkType:              shortEnum(row.WorkType, "PHASE_THEME_"),
		SessionSteering:       shortEnum(row.SessionSteering, "SESSION_STEERING_"),
		GoalAchieved:          shortEnum(row.GoalAchieved, "GOAL_ACHIEVED_"),
		Smoothness:            shortEnum(row.Smoothness, "SMOOTHNESS_"),
		FrictionCause:         shortEnum(row.FrictionCause, "FRICTION_CAUSE_"),
		Speed:                 shortEnum(row.Speed, "SPEED_"),
		TokenEfficiency:       shortEnum(row.TokenEfficiency, "TOKEN_EFFICIENCY_"),
		GoalIsCodingWork:      row.GoalIsCodingWork,
		ScoreOverall:          row.ScoreOverall,
		ScoreClarity:          row.ScoreClarity,
		ScoreSpecificity:      row.ScoreSpecificity,
		ScoreContextProvision: row.ScoreContextProvision,
		ScoreActionability:    row.ScoreActionability,
		ScoreCompleteness:     row.ScoreCompleteness,
		ScoreEfficiency:       row.ScoreEfficiency,
	}
	if row.UnderlyingGoal != nil {
		verdict.UnderlyingGoal = *row.UnderlyingGoal
	}
	return verdict
}

func translateMeasured(row *model.SessionAnalysis) Measured {
	return Measured{
		TotalPrompts:     row.TotalPrompts,
		TurnCount:        row.TurnCount,
		HumanTurnCount:   row.HumanTurnCount,
		DelegatedTurns:   row.DelegatedTurnCount,
		PhaseCount:       row.PhaseCount,
		ToolCallCount:    row.ToolCallCount,
		TotalTokens:      row.TotalTokens,
		WastedTokens:     row.WastedTokens,
		EstimatedCostUSD: row.EstimatedCostUsd,
		DurationSeconds:  row.SessionDurationSeconds,
	}
}

func translateTurns(read *sessionanalysis.ReadResult) []Turn {
	if read == nil {
		return nil
	}
	turns := make([]Turn, 0, len(read.Turns))
	for _, judged := range read.Turns {
		turn := Turn{
			PromptIndex:        judged.PromptIndex,
			PhaseOrdinal:       judged.PhaseOrdinal,
			PromptType:         shortEnum(judged.PromptType, "PROMPT_TYPE_"),
			UserSteering:       shortEnum(judged.UserSteering, "TURN_USER_STEERING_"),
			PromptCompleteness: shortEnum(judged.PromptCompleteness, "PROMPT_COMPLETENESS_"),
			PromptObjectiveMet: shortEnum(judged.PromptObjectiveMet, "PROMPT_OBJECTIVE_MET_"),
			AgentHelpfulness:   shortEnum(judged.AgentHelpfulness, "AGENT_HELPFULNESS_"),
			AgentInitiative:    shortEnum(judged.AgentInitiative, "AGENT_INITIATIVE_"),
			ToolUse:            shortEnum(judged.ToolUse, "TOOL_USE_"),
			FailureClass:       shortEnum(judged.FailureClass, "FAILURE_CLASS_"),
			WasteCause:         shortEnum(judged.WasteCause, "WASTE_CAUSE_"),
			DownstreamImpact:   shortEnum(judged.DownstreamImpact, "DOWNSTREAM_IMPACT_"),
			RoundTripFault:     shortEnum(judged.RoundTripFault, "ROUND_TRIP_FAULT_"),
			SecurityIssues:     translateIssues(judged.PromptIssues),
		}
		turns = append(turns, turn)
	}
	return turns
}

func translatePhases(read *sessionanalysis.ReadResult) []Phase {
	if read == nil {
		return nil
	}
	phases := make([]Phase, 0, len(read.Phases))
	for _, judged := range read.Phases {
		phase := Phase{
			Ordinal:        judged.PhaseOrdinal,
			RangeStart:     judged.RangeStart,
			RangeEnd:       judged.RangeEnd,
			Theme:          shortEnum(judged.PhaseTheme, "PHASE_THEME_"),
			TaskComplexity: shortEnum(judged.TaskComplexity, "TASK_COMPLEXITY_"),
			UserSteering:   shortEnum(judged.UserSteering, "PHASE_USER_STEERING_"),
			ObjectiveMet:   shortEnum(judged.PhaseObjectiveMet, "PHASE_OBJECTIVE_MET_"),
			Smoothness:     shortEnum(judged.Smoothness, "SMOOTHNESS_"),
			FrictionCause:  shortEnum(judged.FrictionCause, "FRICTION_CAUSE_"),
			Speed:          shortEnum(judged.Speed, "SPEED_"),
			WentWell:       judged.WentWell.GetValues(),
			WentWrong:      judged.WentWrong.GetValues(),
		}
		if judged.PhaseObjective != nil {
			phase.Objective = *judged.PhaseObjective
		}
		phases = append(phases, phase)
	}
	return phases
}

// translateSecurityFlags maps the engine's RECONCILED security flags (built via
// security.BuildOpenFlags — already masked, deduped per type at the highest severity, and filtered to
// the three reconciled slugs) onto the public SecurityFlag.
func translateSecurityFlags(flags []*model.SecurityFlag) []SecurityFlag {
	if len(flags) == 0 {
		return nil
	}
	out := make([]SecurityFlag, 0, len(flags))
	for _, flag := range flags {
		occurrences := flag.FlagDetails.GetOccurrences()
		excerpts := make([]string, 0, len(occurrences))
		for _, occurrence := range occurrences {
			excerpts = append(excerpts, occurrence.GetSanitizedExcerpt())
		}
		out = append(out, SecurityFlag{
			Type:        flag.FlagType,
			Severity:    flag.Severity,
			Occurrences: excerpts,
		})
	}
	return out
}

// translateIssues maps a turn's security-layer prompt issues to the public per-turn SecurityIssue,
// running each note through the defensive secret redactor (the per-turn list is raw, unlike the
// reconciled SecurityFlags, so it is masked here so a raw secret never surfaces).
func translateIssues(list *sessionintelligencepb.PromptIssueList) []SecurityIssue {
	items := list.GetItems()
	if len(items) == 0 {
		return nil
	}
	issues := make([]SecurityIssue, 0, len(items))
	for _, issue := range items {
		if issue.GetLayer() != "security" {
			continue
		}
		issues = append(issues, SecurityIssue{
			Type:     slugKebab(issue.GetSlug()),
			Severity: shortEnumValue(issue.GetSeverity(), "ISSUE_SEVERITY_"),
			Note:     sessionanalysis.RedactSecret(issue.GetNote()),
		})
	}
	return issues
}

// shortEnum renders a nullable proto-enum column as its short upper-snake name, or "" when nil or an
// UNSPECIFIED value.
func shortEnum[T interface{ String() string }](value *T, prefix string) string {
	if value == nil {
		return ""
	}
	return trimEnum((*value).String(), prefix)
}

// shortEnumValue renders a value proto enum as its short upper-snake name, or "" when it is an
// UNSPECIFIED value.
func shortEnumValue[T interface{ String() string }](value T, prefix string) string {
	return trimEnum(value.String(), prefix)
}

// trimEnum strips the enum prefix, rendering an UNSPECIFIED value (nil-equivalent) as "".
func trimEnum(name, prefix string) string {
	if strings.HasSuffix(name, "UNSPECIFIED") {
		return ""
	}
	return strings.TrimPrefix(name, prefix)
}

// slugKebab renders a PromptIssueSlug enum as its kebab-case form (e.g. "api-key-leak"), matching the
// flag_type the security tables store. Returns "" for the unspecified slug.
func slugKebab(slug sessionintelligencepb.PromptIssueSlug) string {
	name := strings.TrimPrefix(slug.String(), "PROMPT_ISSUE_SLUG_")
	if name == "" || name == "UNSPECIFIED" {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
