# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic response schemas for the three session-analysis producers.

R (`analyze_read`, Calls 1+2), E (`analyze_evidence`, Calls 3+6), and D
(`analyze_discard`, Call 5). Every enum is bound with a `StrEnum` so
`model_json_schema()` emits an `enum` constraint into Gemini's `response_schema`:
guided decoding then makes an out-of-contract enum value structurally impossible to
emit, which the contract model-portability rule requires ("a cap can live in prose because
exceeding one is visible; an invented enum value is not"). The exact value sets mirror the
producer output shapes (Producer R / E and Call 5).

List CAPS (went_well <= 4, sdlc_stage 1-5, discarded_ranges <= 5, ...) are deliberately
NOT bound as `maxItems`: the contract enforces them in the prompt and Go re-validates, and a
maxItems on a nested enum array inflates the guided-decoding state count (the same limit
that pushed generate_insights off gemini-2.5-flash). The schema binds enum vocabulary and
structural shape only; the caps stay where the contract puts them.
"""

from __future__ import annotations

from enum import StrEnum

from pydantic import BaseModel, Field

# ─────────────────────────── shared enums ───────────────────────────
# Reused across grains within R (and, where noted, E). Kept as single definitions so a
# vocabulary can't drift between the turn and phase views of the same concept.


class Severity(StrEnum):
    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"
    CRITICAL = "critical"


class AgentHelpfulness(StrEnum):
    HELPFUL = "helpful"
    PARTIALLY_HELPFUL = "partially_helpful"
    UNHELPFUL = "unhelpful"
    BLOCKED_BY_LIMITATION = "blocked_by_limitation"
    UNCLEAR = "unclear"


class Smoothness(StrEnum):
    SMOOTH = "smooth"
    SOME_FRICTION = "some_friction"
    ROUGH = "rough"
    UNCLEAR = "unclear"


class FrictionCause(StrEnum):
    NONE = "none"
    HARNESS = "harness"
    MODEL = "model"
    PROMPT = "prompt"
    APPROACH = "approach"
    UNKNOWN = "unknown"


class Speed(StrEnum):
    FAST = "fast"
    SLOW = "slow"
    UNKNOWN = "unknown"


# ═══════════════════════ R — analyze_read ═══════════════════════════


class PromptType(StrEnum):
    EXPLORATION_ASK = "exploration_ask"
    PLANNING = "planning"
    DECISION_MAKING = "decision_making"
    CREATION = "creation"
    REVIEW = "review"
    OPERATIONAL_ASK = "operational_ask"
    CONTEXT_ENRICHMENT = "context_enrichment"
    DATA_QUERY = "data_query"
    STATUS_UPDATE = "status_update"
    TRIVIAL = "trivial"


class TurnUserSteering(StrEnum):
    ACTIVE = "active"
    CORRECTING = "correcting"
    PASSIVE = "passive"


class PromptCompleteness(StrEnum):
    COMPLETE = "complete"
    MISSING_INFO = "missing_info"
    AMBIGUOUS = "ambiguous"
    NOT_APPLICABLE = "not_applicable"


class ObjectiveMet(StrEnum):
    MET = "met"
    PARTIALLY_MET = "partially_met"
    NOT_MET = "not_met"
    UNCLEAR = "unclear"
    NOT_APPLICABLE = "not_applicable"


class AgentInitiative(StrEnum):
    FOLLOWED_SPEC = "followed_spec"
    ASKED_TO_CONFIRM = "asked_to_confirm"
    REFUSED_TO_PROCEED = "refused_to_proceed"
    ASSUMED_AND_PROCEEDED = "assumed_and_proceeded"
    UNCLEAR = "unclear"


class ToolUse(StrEnum):
    APPROPRIATE = "appropriate"
    SKILL_SHOULD_HAVE_BEEN_USED = "skill_should_have_been_used"
    INADEQUATE = "inadequate"
    OVERUSED = "overused"
    UNCLEAR = "unclear"


class WasRetry(StrEnum):
    FRESH = "fresh"
    RETRY_SAME = "retry_same"
    RETRY_REFINED = "retry_refined"


class FailureClass(StrEnum):
    NONE = "none"
    TRANSIENT = "transient"
    HARD = "hard"


class TokenLocus(StrEnum):
    """The MODEL-FACING five values only. Code stamps `human_input` / `tool_results`
    (arithmetic on the measured token split), so the response schema binds the subset the
    model is allowed to emit (PRD Producer R output contract)."""

    AGENT_REASONING = "agent_reasoning"
    CONTEXT_REESTABLISHMENT = "context_reestablishment"
    SUBAGENT_FANOUT = "subagent_fanout"
    RETRY = "retry"
    UNKNOWN = "unknown"


class WasteCause(StrEnum):
    NONE = "none"
    REDUNDANT_REREAD = "redundant_reread"
    OVER_FETCH = "over_fetch"
    WRONG_MODEL_TIER = "wrong_model_tier"
    POOR_DECOMPOSITION = "poor_decomposition"
    MISSING_MEMORY = "missing_memory"


class DecisionType(StrEnum):
    APPROACH = "approach"
    API_OR_LIBRARY = "api_or_library"
    DATA_SHAPE = "data_shape"
    SCOPE_BOUNDARY = "scope_boundary"
    INTENT_ASSUMPTION = "intent_assumption"


class DownstreamImpact(StrEnum):
    BUILT_ON = "built_on"
    BENIGN = "benign"
    CORRECTED = "corrected"
    DRIFTED = "drifted"
    UNKNOWN = "unknown"


class FeedbackTarget(StrEnum):
    ARTIFACT = "artifact"
    REVIEW_FINDING = "review_finding"
    BOTH = "both"
    PROCESS = "process"


class FeedbackDisposition(StrEnum):
    ACCEPT = "accept"
    REJECT = "reject"
    REFINE = "refine"
    DEFER = "defer"
    REQUEST_RETRY = "request_retry"
    UNCLEAR = "unclear"


class PhaseTheme(StrEnum):
    EXPLORATION = "exploration"
    PLANNING = "planning"
    CREATION = "creation"
    CORRECTION = "correction"
    REFACTOR = "refactor"
    REVIEW = "review"
    OPERATIONS = "operations"


class SdlcStage(StrEnum):
    PRODUCT_SPEC = "Product Spec"
    TECH_SPEC = "Tech Spec"
    PLAN = "Plan"
    CODE = "Code"
    REVIEW = "Review"
    TEST_AND_QA = "Test & QA"
    CI_CD = "CI/CD"
    DEPLOY = "Deploy"
    OPERATE = "Operate"


class TaskComplexity(StrEnum):
    TRIVIAL = "trivial"
    MODERATE = "moderate"
    COMPLEX = "complex"


class Lever(StrEnum):
    PROMPTING = "prompting"
    TOOL_USE = "tool_use"
    PLANNING = "planning"
    DECOMPOSITION = "decomposition"
    SUBAGENTS = "subagents"
    SKILL_OR_COMMAND = "skill_or_command"
    STEERING = "steering"
    VERIFICATION = "verification"


class ImprovementAxis(StrEnum):
    OUTPUT_QUALITY = "output_quality"
    SPEED = "speed"
    TOKENS = "tokens"


class SessionSteering(StrEnum):
    MOSTLY_ACTIVE = "mostly_active"
    ACTIVE = "active"
    MIDDLING = "middling"
    SLIGHTLY_PASSIVE = "slightly_passive"
    MOSTLY_PASSIVE = "mostly_passive"


class GoalAchieved(StrEnum):
    FULLY = "fully"
    SOMEWHAT = "somewhat"
    PARTIALLY = "partially"
    BARELY = "barely"
    NO = "no"


class TokenEfficiency(StrEnum):
    EFFICIENT = "efficient"
    WASTEFUL = "wasteful"
    UNKNOWN = "unknown"


class InputContract(StrEnum):
    NONE = "none"
    TARGET = "target"
    TASK = "task"


class HarnessAttribution(StrEnum):
    HARNESS = "harness"
    PROMPT = "prompt"
    MODEL = "model"


class ArtifactType(StrEnum):
    SKILL = "skill"
    COMMAND = "command"
    AGENT = "agent"
    HOOK = "hook"
    RULE = "rule"


class ArtifactRelation(StrEnum):
    CAUSED = "caused"
    SHOULD_HAVE_BEEN_INVOKED = "should_have_been_invoked"
    MISSING = "missing"


class PhaseImpact(StrEnum):
    FASTER = "faster"
    FRICTION_FREE = "friction_free"
    BOTH = "both"


class VisibleVia(StrEnum):
    INVOKED_BODY = "invoked_body"
    INFERRED = "inferred"


class PromptIssue(BaseModel):
    slug: str
    severity: Severity
    note: str | None = None


class TurnAnalysis(BaseModel):
    prompt_index: int
    prompt_type: PromptType
    # Human-behaviour signals — null when the turn is a subagent turn.
    user_steering: TurnUserSteering | None = None
    prompt_completeness: PromptCompleteness | None = None
    prompt_issues: list[PromptIssue] = Field(default_factory=list)
    # Agent-behaviour signals — always judged.
    prompt_objective_met: ObjectiveMet
    agent_helpfulness: AgentHelpfulness
    agent_initiative: AgentInitiative
    tool_use: ToolUse
    was_retry: WasRetry | None = None
    failure_class: FailureClass | None = None
    token_locus: TokenLocus
    waste_cause: WasteCause | None = None
    # Both null unless agent_initiative == assumed_and_proceeded.
    decision_type: DecisionType | None = None
    downstream_impact: DownstreamImpact | None = None
    feedback_target: FeedbackTarget | None = None
    feedback_disposition: FeedbackDisposition | None = None
    referenced_artifact: str | None = None
    referenced_finding_ids: list[str] = Field(default_factory=list)
    # Required key; null only on a turn with no citable event.
    evidence_seq: int | None = Field(...)


class PromptRange(BaseModel):
    start_index: int
    end_index: int


class CouldBeBetter(BaseModel):
    root_cause: str
    suggestion: str
    lever: Lever
    axes: list[ImprovementAxis]
    target: str | None = None
    evidence_prompt_index: int


class PhaseAnalysis(BaseModel):
    prompt_range: PromptRange
    boundary_overridden: bool
    # Required key; non-null iff boundary_overridden is true.
    override_reason: str | None = Field(...)
    phase_theme: PhaseTheme
    # 1-5 stages; cap in prompt.
    sdlc_stage: list[SdlcStage]
    task_complexity: TaskComplexity
    user_steering: TurnUserSteering
    phase_objective: str
    phase_objective_met: ObjectiveMet
    agent_helpfulness: AgentHelpfulness
    smoothness: Smoothness
    friction_cause: FrictionCause
    speed: Speed
    went_well: list[str] = Field(default_factory=list)
    went_wrong: list[str] = Field(default_factory=list)
    could_be_better: list[CouldBeBetter] = Field(default_factory=list)


class QualityScores(BaseModel):
    overall: int
    clarity: int
    specificity: int
    context_provision: int
    actionability: int
    completeness: int
    efficiency: int


class InvokedSkillJudgment(BaseModel):
    invocation_ref: str
    baseline_intent: str
    input_contract: InputContract
    contract_satisfied: bool | None = None
    contribution: str
    evidence_prompt_index: int


class HarnessFinding(BaseModel):
    preference_signal: str
    agent_default: str
    author_preference: str
    both_valid: bool
    attribution: HarnessAttribution
    invoked_skill: str | None = None
    artifact_type: ArtifactType
    artifact_relation: ArtifactRelation
    phase_ref: int
    phase_impact: PhaseImpact
    visible_via: VisibleVia
    evidence_prompt_index: int


class SessionVerdict(BaseModel):
    underlying_goal: str
    session_steering: SessionSteering
    goal_achieved: GoalAchieved
    smoothness: Smoothness
    friction_cause: FrictionCause
    speed: Speed
    token_efficiency: TokenEfficiency
    goal_is_coding_work: bool
    scores: QualityScores
    invoked_skills: list[InvokedSkillJudgment] = Field(default_factory=list)
    harness_findings: list[HarnessFinding] = Field(default_factory=list)


class AnalyzeReadResponse(BaseModel):
    """R's one object, three keys. The response schema binds this and nothing else."""

    turns: list[TurnAnalysis]
    phases: list[PhaseAnalysis]
    session_analysis: SessionVerdict


# ═══════════════════════ E — analyze_evidence ═══════════════════════


class ArtifactDisposition(StrEnum):
    ACCEPTED = "accepted"
    REJECTED = "rejected"
    MODIFIED = "modified"
    DEFERRED = "deferred"
    UNRESOLVED = "unresolved"


class Attribution(StrEnum):
    ARTIFACT_SKILL = "artifact_skill"
    REVIEW_SKILL = "review_skill"
    BOTH = "both"
    PROMPT = "prompt"
    UNRESOLVED = "unresolved"


class SkillRole(StrEnum):
    PRODUCER = "producer"
    REVIEWER = "reviewer"
    BOTH = "both"


class ContractCompliance(StrEnum):
    ADEQUATE = "adequate"
    INADEQUATE = "inadequate"
    UNRESOLVED = "unresolved"


class OutputQuality(StrEnum):
    ADEQUATE = "adequate"
    INADEQUATE = "inadequate"
    UNRESOLVED = "unresolved"


class ExecutionReliability(StrEnum):
    CLEAN = "clean"
    RECOVERED_WITH_FRICTION = "recovered_with_friction"
    FAILED = "failed"
    UNRESOLVED = "unresolved"


class HumanAlignment(StrEnum):
    ALIGNED = "aligned"
    CORRECTED = "corrected"
    DISPUTED = "disputed"
    UNRESOLVED = "unresolved"


class SkillVerdict(StrEnum):
    ADEQUATE = "adequate"
    INADEQUATE = "inadequate"
    UNRESOLVED = "unresolved"


class RoundTripFault(StrEnum):
    PROMPT = "prompt"
    AGENT = "agent"
    TASTE = "taste"
    MIXED = "mixed"
    UNCLEAR = "unclear"
    NO_ROUND_TRIP = "no_round_trip"


class RoundTripPromptCompleteness(StrEnum):
    COMPLETE = "complete"
    MISSING_INFO = "missing_info"
    AMBIGUOUS = "ambiguous"
    UNRESOLVED = "unresolved"


class ContractCheck(BaseModel):
    gate_ref: str
    clause_is_a_real_requirement: bool
    evidence_seq: int


class ArtifactFinding(BaseModel):
    finding_id: str
    severity: Severity
    disposition: ArtifactDisposition
    attribution: Attribution
    evidence_prompt_index: int


class ArtifactAdjudication(BaseModel):
    artifact_ref: str
    contract_checks: list[ContractCheck] = Field(default_factory=list)
    findings: list[ArtifactFinding] = Field(default_factory=list)
    human_signal_present: bool
    overall_attribution: Attribution


class SkillAssessment(BaseModel):
    invocation_ref: str
    role: SkillRole
    contract_compliance: ContractCompliance
    output_quality: OutputQuality
    execution_reliability: ExecutionReliability
    human_alignment: HumanAlignment
    verdict: SkillVerdict
    # Required key; null only on a required row with no citable event.
    evidence_seq: int | None = Field(...)


class RoundTripIssue(BaseModel):
    slug: str
    layer: str | None = None
    severity: Severity
    note: str | None = None


class RoundTrip(BaseModel):
    prompt_index: int
    fault: RoundTripFault
    prompt_completeness: RoundTripPromptCompleteness
    # Non-null only on prompt/mixed faults.
    issue: RoundTripIssue | None = None
    # Required key; P's or Q's index.
    evidence_prompt_index: int | None = Field(...)


class AnalyzeEvidenceResponse(BaseModel):
    """E's one object, three keys — two from artifact/review adjudication, one from
    round-trip fault."""

    artifacts: list[ArtifactAdjudication]
    skill_assessments: list[SkillAssessment]
    round_trips: list[RoundTrip]


# ═══════════════════════ D — analyze_discard ════════════════════════


class DiscardedRange(BaseModel):
    prompt_range: PromptRange
    # One line, <= 25 words (cap in prompt).
    reason: str
    evidence_prompt_index: int


class AnalyzeDiscardResponse(BaseModel):
    """D's one object, one key. An empty array is valid and is the common answer."""

    discarded_ranges: list[DiscardedRange] = Field(default_factory=list)
