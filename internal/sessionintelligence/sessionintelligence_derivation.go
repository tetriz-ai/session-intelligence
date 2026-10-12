// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"context"
	"math"
	"regexp"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

const (
	// effectivenessSourceCCFacets marks an effectiveness verdict derived from Claude Code's own outcome label.
	effectivenessSourceCCFacets = "cc_facets"
	// delegationLevelDelegated is the delegation tier for fully autonomous (background/subagent) sessions.
	delegationLevelDelegated = "delegated"
	// delegationLevelAssisted is the delegation tier for sessions that spawned sub-agents while staying interactive.
	delegationLevelAssisted = "assisted"
	// delegationLevelInteractive is the delegation tier for hands-on human-driven sessions.
	delegationLevelInteractive = "interactive"
	// delegationLevelUnknown marks a CC session whose run mode could not be
	// determined (no per-session session_kind — a pre-upgrade upload or a
	// historical row reprocessed after). It is deliberately NOT interactive:
	// counting an undetermined-mode session as hands-on would resurrect the very
	// mislabel fixed, in the opposite direction. Excluded from the
	// interactive / assisted / delegated known-mode buckets downstream.
	delegationLevelUnknown = "unknown"
)

// ExtractInstallationMetadata maps installation-level signals onto UsageMetadata.
// ctx carries the org, which the divergence warn line the Cursor derivation can
// emit is scoped to.
func ExtractInstallationMetadata(ctx context.Context, conv *sessionintelligencepb.Conversation, um *sessionintelligencepb.UsageMetadata) {
	im := conv.GetInstallationMetadata()
	if im == nil {
		return
	}
	um.PlanTier = im.GetPlanTier()
	um.PlanTierSource = im.GetPlanTierSource()
	um.LoginEmail = im.GetLoginEmail()
	um.DeviceId = im.GetDeviceId()
	um.Entrypoint = im.GetEntrypoint()
	um.CcVersion = im.GetCcVersion()
	um.IdePlatform = im.GetPlatform()
	um.IsOauthAuth = im.GetIsOauthAuth()
	um.OrganizationUuid = im.GetOrganizationUuid()
	um.AccountUuid = im.GetAccountUuid()
	// session_kind is set per-session from SessionMeta in ExtractSessionSignals.
	// The deprecated install-level InstallationMetadata.session_kind is
	// deliberately NOT read as a fallback: it described the machine, not the
	// session, so a conversation lacking the per-session value (a pre-upgrade
	// client, or a historical row reprocessed after this ships) resolves
	// session_kind to empty rather than that known-wrong value. computeDelegationLevel
	// then classifies such a CC session as delegation_level "unknown" (not
	// interactive), keeping an undetermined mode out of the known-mode buckets.
	um.CursorOnboardingDate = im.GetCursorOnboardingDate()
	um.CursorPrivacyMode = im.GetCursorPrivacyMode()
	um.CursorMemoriesEnabled = im.GetCursorMemoriesEnabled()
	um.CursorPersonalContextConfigured = im.GetCursorPersonalContextConfigured()
	um.CursorConversationScoring = im.GetCursorConversationScoring()
	// Cursor account facts (Statsig user.custom). Persisted verbatim; the
	// canonical Cursor plan tier is derived from them further below.
	um.IsEnterpriseUser = im.GetIsEnterpriseUser()
	um.IsEnterpriseTrialUser = im.GetIsEnterpriseTrialUser()
	um.CursorTeamId = im.GetCursorTeamId()
	um.CursorTeamRole = im.GetCursorTeamRole()
	um.CursorUserId = im.GetCursorUserId()
	// Cursor's own account statement, read over its API. Persisted verbatim within
	// a length bound so anything re-deriving a tier reads the vendor's words rather
	// than our conclusion. A refused value is counted on its own tripwire, not the
	// unmapped-spelling one: the drop persists neither the string nor its length, so
	// the two have opposite triage paths.
	cursorBillingTier, billingTierAccepted := acceptCursorVendorString(im.GetCursorBillingTier())
	if !billingTierAccepted {
		overlongCursorVendorStringTotal.Inc()
	}
	cursorTeamMembershipType, membershipTypeAccepted := acceptCursorVendorString(im.GetCursorTeamMembershipType())
	if !membershipTypeAccepted {
		overlongCursorVendorStringTotal.Inc()
	}
	um.CursorBillingTier = cursorBillingTier
	um.CursorTeamMembershipType = cursorTeamMembershipType
	// Copied as a pointer, not through the getter: unset and a reported false are
	// different billing periods, and nothing downstream may collapse them.
	um.CursorIsYearlyPlan = im.CursorIsYearlyPlan
	um.DefaultModel = im.GetDefaultModel()
	um.LastChatMode = im.GetLastChatMode()
	um.IdeVersion = im.GetIdeVersion()
	um.IdeMachineId = im.GetIdeMachineId()

	// Claude Code authoritative account record (~/.claude.json oauthAccount).
	// Persist the raw values verbatim, then derive the canonical plan_tier from
	// seat_tier/organization_type. The mapping overrides the desktop's
	// inferred telemetry/model plan_tier only when it confidently recognizes the
	// seat; an unrecognized seat leaves the inferred value untouched (no
	// regression) while still keeping the raw fields for observability.
	um.SeatTier = im.GetSeatTier()
	um.OrganizationType = im.GetOrganizationType()
	um.BillingType = im.GetBillingType()
	um.OrganizationRole = im.GetOrganizationRole()
	um.OrganizationRateLimitTier = im.GetOrganizationRateLimitTier()
	um.UserRateLimitTier = im.GetUserRateLimitTier()
	um.SubscriptionCreatedAt = im.GetSubscriptionCreatedAt()
	um.HasExtraUsageEnabled = im.GetHasExtraUsageEnabled()
	if mapped, ok := MapClaudeCodeSeatTier(im.GetSeatTier(), im.GetOrganizationType(), im.GetUserRateLimitTier()); ok {
		// Point-in-time guard: the seat tier reflects the user's CURRENT
		// subscription. A session that started before that subscription began was
		// on a different (usually lower) plan, so overriding it with today's tier
		// would skew date-filtered rollups. Keep the per-session inferred tier in
		// that case; the raw seat_tier above is still persisted either way.
		if sessionPredatesSubscription(conv, im) {
			return
		}
		um.PlanTier = mapped
		um.PlanTierSource = PlanTierSourceCCOauthAccount
		// Stamp the seat list price point-in-time (point-in-time pricing) so a later
		// price-map edit never re-prices this conversation. Only a concrete
		// non-zero price is stamped; free ($0) / unpriced (enterprise) tiers
		// leave it unset, and seat-allocation resolves those from plan_tier at
		// read time (deterministic either way).
		if price, ok := LookupSeatMonthlyPriceUSD(sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE.String(), mapped); ok && price > 0 {
			um.SeatMonthlyPriceUsd = price
		}
	} else if im.GetSeatTier() != "" {
		// Authoritative seat_tier present but unrecognized — surface it so a new
		// Anthropic tier is discoverable on a dashboard. The inferred plan_tier is
		// left intact (no regression); the raw seat_tier is still persisted.
		unmappedSeatTierTotal.Inc()
	}

	// Cursor: derive the canonical plan tier from the account facts before the
	// stamp below resolves a price from it (neither membership string names a tier
	// alone — see MapCursorMembershipTier). An unconfident mapping leaves the raw
	// value in place, which the price table's legacy aliases still price.
	// plan_tier_source moves only when a VENDOR-reported value decided the tier:
	// the read-side REPORTED/INFERRED gate keys on it, so provenance must follow
	// what actually won rather than the act of running this code.
	if um.GetIdeType() == sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String() {
		mapped, source, ok := MapCursorMembershipTier(ctx, CursorTierSignals{
			BillingTier:           cursorBillingTier,
			TeamMembershipType:    cursorTeamMembershipType,
			MembershipType:        um.GetPlanTier(),
			TeamID:                im.GetCursorTeamId(),
			IsEnterpriseUser:      im.GetIsEnterpriseUser(),
			IsEnterpriseTrialUser: im.GetIsEnterpriseTrialUser(),
			LocalPlanTierSource:   um.GetPlanTierSource(),
			TeamRole:              im.GetCursorTeamRole(),
		})
		if ok {
			um.PlanTier = mapped
			if source != "" {
				um.PlanTierSource = source
			}
		}
	}

	// Point-in-time seat-price stamp for non-Claude-Code IDEs (Cursor, Copilot, …):
	// the CC block above owns Claude Code's stamp. A REPORTED tier that resolves to a
	// concrete list price is frozen here so a later price-map edit can't re-price
	// closed history; free ($0) / unpriced (enterprise) / inferred tiers are left
	// unstamped and resolve from plan_tier at read time. Uses the canonical
	// ClassifyPlanTierOrigin (same one the read-side allocation gate uses) so a
	// desktop-guessed tier is never frozen as a billing fact — one definition of
	// "inferred".
	//
	// Every stamp resolves on the vendor's declared default basis, never on a
	// period read off this row. The period is a property of the SUBSCRIPTION, not
	// of a session: a per-row signal is present on some of a seat-month's rows and
	// absent on others, so deriving the rate from it would stamp the same tier at
	// two prices within one seat-month and let row selection — not the customer's
	// contract — decide the fee. The read-time fallback has no period either, so
	// the same seat would price differently through different windows.
	if um.GetIdeType() != sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE.String() &&
		um.GetSeatMonthlyPriceUsd() == 0 &&
		ClassifyPlanTierOrigin(um.GetPlanTierSource()) != PlanTierOriginInferred {
		if price, ok := LookupSeatMonthlyPriceUSD(um.GetIdeType(), um.GetPlanTier()); ok && price > 0 {
			um.SeatMonthlyPriceUsd = price
		}
	}
}

// sessionPredatesSubscription reports whether the conversation started before
// the account's current subscription began. Both timestamps must be present and
// valid to compare; when either is missing we cannot establish ordering and
// return false (apply the override — best effort, matches prior behavior).
func sessionPredatesSubscription(conv *sessionintelligencepb.Conversation, im *sessionintelligencepb.InstallationMetadata) bool {
	subscriptionStart := im.GetSubscriptionCreatedAt()
	sessionStart := conv.GetConversationStartedAt()
	if subscriptionStart == nil || !subscriptionStart.IsValid() ||
		sessionStart == nil || !sessionStart.IsValid() {
		return false
	}
	return sessionStart.AsTime().Before(subscriptionStart.AsTime())
}

// ExtractAiAttribution maps AI code attribution signals onto UsageMetadata.
func ExtractAiAttribution(conv *sessionintelligencepb.Conversation, um *sessionintelligencepb.UsageMetadata) {
	aa := conv.GetAiAttribution()
	if aa == nil {
		return
	}
	um.TabCompletionCount = aa.GetTabCompletionCount()
	um.AiCodePercent = aa.GetAiCodePercent()
	um.TabSuggestedLines = aa.GetTabSuggestedLines()
	um.TabAcceptedLines = aa.GetTabAcceptedLines()
	um.ComposerSuggestedLines = aa.GetComposerSuggestedLines()
	um.ComposerAcceptedLines = aa.GetComposerAcceptedLines()
	um.TabCompletionModelBreakdown = aa.GetTabCompletionModelBreakdown()
	um.AiCodePercentV2 = aa.GetAiCodePercentV2()
	um.DeletedFilesCount = aa.GetDeletedFilesCount()
	um.TrackingStartTime = aa.GetTrackingStartTime()
}

// ExtractStatsCache maps aggregate stats cache signals onto UsageMetadata.
func ExtractStatsCache(conv *sessionintelligencepb.Conversation, um *sessionintelligencepb.UsageMetadata) {
	sc := conv.GetStatsCache()
	if sc == nil {
		return
	}
	um.HourCounts = sc.GetHourCounts()
	um.TotalSessionsLifetime = sc.GetTotalSessions()
	um.TotalMessagesLifetime = sc.GetTotalMessages()
	um.LongestSessionDurationMs = sc.GetLongestSessionDurationMs()
	um.FirstSessionDate = sc.GetFirstSessionDate()
	um.StatsDailyActivity = sc.GetDailyActivity()
	um.StatsDailyModelTokens = sc.GetDailyModelTokens()
	um.StatsModelUsage = sc.GetModelUsage()
}

// discardThreshold is the minimum discard_probability at which an AI response
// is considered discarded. Must stay in sync with the threshold in the LLM system prompt.
const discardThreshold = 0.54

// segmentProvenanceUser is the per-prompt segment provenance marking a human-authored span.
// Mirrors the `user` value of the Provenance enum the LLM tags segments with.
const segmentProvenanceUser = "user"

// DeriveNoUserPrompt reports whether a conversation carried ZERO human-written prompts — only
// skill invocations and their injected bodies. It is computed from per-prompt segment provenance
// (reference-llm does not emit the flag): true only when the analysis has at least one segment and
// none is user-authored. No segments at all yields false — with no provenance signal we treat the
// session as an ordinary (counted) one rather than silently marking it skill-only.
func DeriveNoUserPrompt(analysis *sessionintelligencepb.AnalysisResult) bool {
	sawSegment := false
	for _, ppa := range analysis.GetPerPromptAnalysis() {
		for _, seg := range ppa.GetSegments() {
			sawSegment = true
			if seg.GetProvenance() == segmentProvenanceUser {
				return false
			}
		}
	}
	return sawSegment
}

// ApplyDiscardedPairWaste adds the full API token cost for discarded prompt–response
// pairs to analysis.TokenAnalysis.WastedTokens, then recomputes WastedPercentage
// relative to actual API token consumption (totalTokens from ExtractMetrics).
//
// For each discarded pair N the waste has two components:
// 1. Base cost: inputTokens + outputTokens for both the user turn and AI response
// (the direct API spend on the discarded exchange).
// 2. Re-sending cost: the discarded turn's inputTokens × remainingAPICalls ×
// max(1, totalDiscards). Because every subsequent API call re-sends the full
// context window (which still contains the discarded pair), the inputTokens
// of the discarded turn are effectively paid again on each subsequent call.
// Scaling by totalDiscards reflects that more discards compound the overhead.
func ApplyDiscardedPairWaste(conv *sessionintelligencepb.Conversation, analysis *sessionintelligencepb.AnalysisResult, totalTokens int64) {
	if analysis.GetTokenAnalysis() == nil || totalTokens == 0 {
		return
	}

	// Count re-sends per billed response, not per content-block line — an API
	// call is one message_id. totalTokens (the denominator and the cap below) is
	// already deduped, so the remainingAPICalls numerator must share that basis
	// or the waste ratio skews up for multi-line Claude Code responses.
	prompts := dedupPromptsByMessageID(conv.GetPrompts())

	type pairInfo struct {
		baseCost  int64
		aiTurnIdx int // index of the AI turn; used to count remaining API calls
	}

	// Single pass: collect per-user-turn inputTokens and build the pairs map.
	pairs := make(map[int32]*pairInfo, len(prompts)/2+1)
	userInputTokens := make([]int64, 0, len(prompts)/2+1)
	userNum := int32(0)
	for i, p := range prompts {
		if p.GetRole() != sessionintelligencepb.Role_ROLE_USER {
			continue
		}
		userNum++
		userInputTokens = append(userInputTokens, p.GetMetadata().GetInputTokens())
		if i+1 < len(prompts) && prompts[i+1].GetRole() == sessionintelligencepb.Role_ROLE_ASSISTANT {
			userTurnTokens := p.GetMetadata().GetInputTokens() + p.GetMetadata().GetOutputTokens()
			aiTurnTokens := prompts[i+1].GetMetadata().GetInputTokens() + prompts[i+1].GetMetadata().GetOutputTokens()
			pairs[userNum] = &pairInfo{
				baseCost:  userTurnTokens + aiTurnTokens,
				aiTurnIdx: i + 1,
			}
		}
	}

	// Count total discards so the re-sending penalty scales with conversation-level
	// discard frequency: more discards → more stale context carried per turn.
	totalDiscards := int64(0)
	for _, ppa := range analysis.GetPerPromptAnalysis() {
		if ppa.GetDiscardProbability() >= discardThreshold && ppa.GetPromptNumber() > 1 {
			totalDiscards++
		}
	}

	// For each discarded pair, add:
	// baseCost — direct API spend
	// contextDelta × remainingAPICalls × totalDiscards — re-sending cost
	for _, ppa := range analysis.GetPerPromptAnalysis() {
		if ppa.GetDiscardProbability() < discardThreshold || ppa.GetPromptNumber() <= 1 {
			continue
		}
		discardedNum := ppa.GetPromptNumber() - 1
		info, ok := pairs[discardedNum]
		if !ok {
			continue
		}

		idx := int(discardedNum) - 1 // 0-based index into userInputTokens
		inputTokens := userInputTokens[idx]

		remainingAPICalls := int64(len(prompts) - info.aiTurnIdx - 1)
		analysis.TokenAnalysis.WastedTokens += info.baseCost + inputTokens*remainingAPICalls*max(1, totalDiscards)
	}

	// Cap WastedTokens at totalTokens. The additive re-sending penalty can
	// compound past the session's own API spend on long high-discard
	// conversations, which would make waste_cost_usd > estimated_cost_usd
	// downstream — literally more waste than spend. The SQL aggregate also
	// carries a defensive LEAST(…) but capping at write time keeps legacy
	// rows from needing a backfill.
	if analysis.GetTokenAnalysis().GetWastedTokens() > totalTokens {
		analysis.TokenAnalysis.WastedTokens = totalTokens
	}

	// Recompute WastedPercentage against actual API token consumption.
	analysis.TokenAnalysis.WastedPercentage = float64(analysis.GetTokenAnalysis().GetWastedTokens()) / float64(totalTokens) * 100
}

// ComputeNormalizedFields populates the IDE-agnostic normalized fields on
// UsageMetadata so the frontend can query a single set of columns without
// branching on IDE type.
// setEffectiveChurn populates the normalized cross-IDE file/line churn counts on um
// from its own already-extracted source fields — Cursor sidebar counts where present,
// else session-meta files_modified / lines_added+removed. It is called both before the
// session context is forwarded to the LLM (so the analyzer sees churn) and inside
// ComputeNormalizedFields as part of the full normalization; reading only um's own
// fields makes the two calls idempotent.
func setEffectiveChurn(um *sessionintelligencepb.UsageMetadata) {
	if um == nil {
		return
	}
	isCursor := um.GetIdeType() == sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String()
	// Lines changed: Cursor sidebar > session-meta fallback.
	if isCursor && um.GetCursorLinesChanged() > 0 {
		um.EffectiveLinesChanged = um.GetCursorLinesChanged()
	} else {
		um.EffectiveLinesChanged = um.GetLinesAdded() + um.GetLinesRemoved()
	}
	// Files changed: Cursor sidebar > session-meta fallback.
	if isCursor && um.GetCursorFilesModified() > 0 {
		um.EffectiveFilesChanged = um.GetCursorFilesModified()
	} else {
		um.EffectiveFilesChanged = um.GetFilesModified()
	}
}

// ComputeNormalizedFields derives and sets the normalized usage fields for a conversation.
func ComputeNormalizedFields(conv *sessionintelligencepb.Conversation, um *sessionintelligencepb.UsageMetadata, _ float64) {
	if um == nil {
		return
	}

	isCursor := um.GetIdeType() == sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String()

	// Files/lines churn. Also computed earlier, before session context is forwarded to
	// the LLM (setEffectiveChurn), and recomputed here idempotently as part of the full
	// normalization. Kept a single shared call so the two sites cannot drift.
	setEffectiveChurn(um)

	// Effectiveness cascade (strongest signal first).
	um.IsSessionEffective, um.EffectivenessSource = computeEffectiveness(um)
	um.EffectivenessConfidence = computeEffectivenessConfidence(um.GetEffectivenessSource())

	// AI code %: prefer v2, fallback v1.
	if um.GetAiCodePercentV2() > 0 {
		um.EffectiveAiCodePercent = um.GetAiCodePercentV2()
	} else {
		um.EffectiveAiCodePercent = um.GetAiCodePercent()
	}

	// Context usage: IDE-specific signals where available, server-computed fallback.
	// BOTH vendor fields are 0–100 and both are divided here. Kiro was previously
	// passed through as if it were already a fraction, which made every Kiro
	// session read ~100x more context-hungry than an equivalent Cursor one.
	// Ground truth: Kiro's own context_snapshots.json records pct 8.1 against a
	// 1,000,000-token window as 80,700 tokens used — i.e. a percentage.
	// Fixed in the derivation rather than the extractor deliberately: this value is
	// recomputed on every (re)process, so historical rows heal with no migration
	// and no 100x discontinuity between old and new uploads.
	isKiro := um.GetIdeType() == "IDE_TYPE_KIRO"
	if isKiro && um.GetIdeContextUsagePercent() > 0 {
		um.EffectiveContextUsagePercent = um.GetIdeContextUsagePercent() / 100
	} else if isCursor && um.GetCursorContextUsagePercent() > 0 {
		um.EffectiveContextUsagePercent = um.GetCursorContextUsagePercent() / 100 // sidebar is 0–100, normalize to 0.0–1.0
	} else {
		um.EffectiveContextUsagePercent = um.GetContextUsagePercent()
	}

	// PR attribution: true if a direct PR link or measurable AI code. Reads
	// the scalar pr_url for back-compat across the transition window — new
	// uploads populate it alongside pr_references.
	um.HasPrAttribution = um.GetPrUrl() != "" || um.GetEffectiveAiCodePercent() > 0 //nolint:staticcheck // back-compat read

	um.AiAttributionConfidence = computeAttributionConfidence(um)
	um.DelegationLevel = computeDelegationLevel(um)
	if n := len(um.GetMcpServerNames()); n <= math.MaxInt32 {
		um.McpServerCount = int32(n)
	}

	// Active-duration cascade — tried in trust order Tier 2 → Tier 3 → Tier 1
	// (idle-excluded measures first, IDE wall-clock last);
	// EffectiveActiveDurationSource records which tier won. See the constants
	// idleGapThreshold, minActiveFloorSeconds, and estimateActiveSecondsPerMessage
	// for parameter rationale.
	//
	// Tier 2 — gap-filtered (primary): sum over the merged user + assistant
	// prompt timestamps of gaps below idleGapThreshold. Falls back to
	// user_message_timestamps when conv.Prompts[] is absent (older uploads,
	// Cursor sessions where session-meta carries the timestamps). A value
	// that clears minActiveFloorSeconds is trusted directly; a sub-floor
	// value is still preferred over the Tier 1 figure because it is measured
	// (idle-excluded), not wall-clock.
	// Tier 3 — message heuristic: per-user-message estimate capped by span,
	// min(span, user_message_count * estimateActiveSecondsPerMessage). Count
	// and span come from user_message_timestamps when present, otherwise from
	// role=USER prompts in conv.Prompts[]. Used when Tier 2 is too sparse to
	// clear the floor.
	// Tier 1 — IDE duration (last resort): duration_minutes * 60. Wall-clock,
	// so it includes idle and over-reports sessions left open across long idle
	// periods; used only when no message timestamps exist to measure active
	// time. sessionDurationSeconds (raw conv span) is never used — same idle
	// inflation, plus the post-last-message tail.

	// IdeReportedDurationS stores the Tier 1 wall-clock unconditionally so a
	// caller can compare it against the idle-excluded EffectiveActiveDurationS.
	// Convert only when the result is representable as int64: a duration_minutes
	// a corrupt upload could carry — non-positive, Inf, NaN, or a finite value
	// whose seconds exceed int64 — is rejected, because float→int64 overflow is
	// platform-defined (MinInt64 on amd64) and would poison the stored value.
	// The bound is int64's own range, not an arbitrary cap.
	var tier1 int64
	if dm := um.GetDurationMinutes(); dm > 0 && dm*60 < math.MaxInt64 {
		tier1 = int64(dm * 60)
	}
	um.IdeReportedDurationS = tier1

	userTS := um.GetUserMessageTimestamps()
	gapTS := collectPromptTimestamps(conv)
	if len(gapTS) < 2 {
		gapTS = userTS
	}

	var tier2 int64
	if len(gapTS) >= 2 {
		tier2 = computeActiveTimeFromTimestamps(gapTS)
	}
	if tier2 >= minActiveFloorSeconds {
		um.EffectiveActiveDurationS = tier2
		um.EffectiveActiveDurationSource = sessionintelligencepb.ActiveDurationSource_ACTIVE_DURATION_SOURCE_TIER2_GAP_FILTERED
	} else {
		tier3 := computeTier3Estimate(conv, userTS)
		switch {
		case tier3 > 0:
			um.EffectiveActiveDurationS = tier3
			um.EffectiveActiveDurationSource = sessionintelligencepb.ActiveDurationSource_ACTIVE_DURATION_SOURCE_TIER3_MESSAGE_HEURISTIC
		case tier2 > 0:
			um.EffectiveActiveDurationS = tier2
			um.EffectiveActiveDurationSource = sessionintelligencepb.ActiveDurationSource_ACTIVE_DURATION_SOURCE_TIER2_GAP_FILTERED
		case tier1 > 0:
			um.EffectiveActiveDurationS = tier1
			um.EffectiveActiveDurationSource = sessionintelligencepb.ActiveDurationSource_ACTIVE_DURATION_SOURCE_TIER1_IDE_DURATION
		}
	}

	um.IsActiveSeat = true
}

// activeDayPrefixPattern is the literal twin of the DAO's activeDayCountExtractedExpr
// regex: a userMessageTimestamps[0] value must match this ISO-8601 date-time
// prefix before the read path will bucket it into distinct_active_days. Both
// sides use the same pattern string so the processor-side exclusion signal and
// the SQL guard cannot drift in meaning.
var activeDayPrefixPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T`)

// ActiveSessionMissingTimestamp reports whether um is an active seat (effective
// active duration > 0) whose userMessageTimestamps[0] is absent or not a
// bucketable ISO-8601 date-time. Such a session counts as active but is
// silently dropped from the distinct_active_days AI-leverage metric by the
// DAO's activeDayCountExtractedExpr. The processor emits a counter + log on this
// condition so the exclusion rate stays observable per IDE rather than implicit
// (today ~0.9% Cursor, 0% other IDEs).
func ActiveSessionMissingTimestamp(um *sessionintelligencepb.UsageMetadata) bool {
	if um == nil || um.GetEffectiveActiveDurationS() <= 0 {
		return false
	}
	ts := um.GetUserMessageTimestamps()
	return len(ts) == 0 || !activeDayPrefixPattern.MatchString(ts[0])
}

// computeEffectiveness decides whether a session was effective and which
// signal was decisive. Cascade order:
// - CC facets outcome (fully_achieved / mostly_achieved)
// - Cursor conversation_scoring non-empty
// - ai_code_percent > 0 (code was committed with AI attribution)
// - default: not effective, "unknown"
//
// PR-merge is a separate signal evaluated at read time against the
// pr_attribution column — see effectiveSessionPredicateSQL in the DAO.
//
// Migration note: the previous cascade included a "score_threshold" fallback
// (overallScore >= 7.0). That has been removed because LLM-assigned scores
// proved unreliable as a standalone effectiveness signal. Historical sessions
// that were marked effective via score_threshold will now show as "unknown" /
// not-effective at read time — the DAO predicate explicitly excludes
// score_threshold rows. No backfill is needed; the old data degrades
// gracefully to the "unknown" bucket.
func computeEffectiveness(um *sessionintelligencepb.UsageMetadata) (bool, string) {
	isCC := um.GetIdeType() == "IDE_TYPE_CLAUDE_CODE"

	// CC: facets outcome.
	if isCC {
		switch um.GetSessionOutcome() {
		case "fully_achieved", "mostly_achieved":
			return true, effectivenessSourceCCFacets
		case "partially_achieved", "not_achieved", "abandoned":
			return false, effectivenessSourceCCFacets
		}
	}

	// Cursor: conversation_scoring non-empty.
	if um.GetCursorConversationScoring() != "" {
		return true, "cursor_classification"
	}

	// Both: ai_code_percent > 0 implies code was committed.
	if um.GetAiCodePercent() > 0 || um.GetAiCodePercentV2() > 0 {
		return true, "code_committed"
	}

	return false, toolModeUnknown
}

// computeEffectivenessConfidence ranks how much we should trust the
// effectiveness verdict given the source that produced it:
// - cc_facets → high (Claude Code's own outcome label)
// - cursor_classification → medium (Cursor's internal classifier fired,
// but the stored payload is ID-presence,
// not a scored verdict — see the
// ide-signal-comparison design)
// - code_committed → low (VCS AI-attribution fallback; proxies
// outcome via ai_code_percent > 0)
// - unknown / "" → low (no signal)
//
// All known sentinels from computeEffectiveness are enumerated explicitly so
// adding a new effectiveness source on the producer side without updating
// this mapping trips the default branch — catchable in tests and code review
// instead of silently downgrading to "low".
//
// Kept separate from computeEffectiveness so the source string remains the
// single observable audit trail; downstream consumers that don't care about
// confidence can ignore the new field.
func computeEffectivenessConfidence(source string) string {
	switch source {
	case effectivenessSourceCCFacets:
		return "high"
	case "cursor_classification":
		return "medium"
	case "code_committed", toolModeUnknown, "":
		return "low"
	default:
		// New effectiveness source introduced by computeEffectiveness without
		// a confidence mapping here — default to "low" but return a distinct
		// marker in logs/test output so drift surfaces. Safe because the
		// field's only consumer is UI confidence rendering, which treats any
		// value other than "high"/"medium" as "low".
		return "low"
	}
}

// computeAttributionConfidence returns the confidence tier for AI attribution.
// "high" = direct session→PR link or measurable AI code percent.
// "none" = no attribution data available.
func computeAttributionConfidence(um *sessionintelligencepb.UsageMetadata) string {
	if um.GetPrUrl() != "" || um.GetEffectiveAiCodePercent() > 0 { //nolint:staticcheck // back-compat read
		return "high"
	}
	return "none"
}

// SessionSourceKiroCrew is the usage_metadata.session_source value the desktop extractor
// stamps on Kiro's autonomous Crew surface. Crew shares IDE_TYPE_KIRO with interactive Kiro
// and is distinguishable only by this value, so every classifier that must tell Crew apart
// keys on this one constant rather than repeating the literal.
const SessionSourceKiroCrew = "kiro-crew"

// computeDelegationLevel classifies the agent delegation mode.
// CC background sessions are "delegated", CC sessions using the task agent
// are "assisted", Codex subagent sessions are "delegated",
// everything else is "interactive". A CC session with no per-session
// session_kind (a pre-upgrade upload, or a historical row reprocessed after
// ) is "unknown" — undetermined mode is not counted as hands-on
// interactive, and is excluded from the known-mode buckets downstream.
func computeDelegationLevel(um *sessionintelligencepb.UsageMetadata) string {
	isCC := um.GetIdeType() == "IDE_TYPE_CLAUDE_CODE"
	if isCC && um.GetSessionKind() == "background" {
		return delegationLevelDelegated
	}
	if isCC && um.GetUsesTaskAgent() {
		return delegationLevelAssisted
	}
	// A CC session whose run mode we could not determine is undetermined, not
	// interactive. Sits after the background/task-agent checks (those signals are
	// independent of session_kind) and before the other-IDE branches, which never
	// match a CC session anyway.
	if isCC && um.GetSessionKind() == "" {
		return delegationLevelUnknown
	}
	if um.GetIdeType() == "IDE_TYPE_CODEX" && um.GetSessionSource() == "subagent" {
		return delegationLevelDelegated
	}
	// Cursor — IDE composer, Agent window (both write composerData), and the
	// terminal CLI all share IDE_TYPE_CURSOR. A session that spawned sub-agents is
	// "assisted". Gate on the sub-agent signals (sub_agent_count from
	// composerData.subagentComposerIds / the CLI Task tool, spawned_agent_names
	// from the CLI Task subagent_type) — NOT uses_task_agent, which for Cursor is
	// composerData.isAgentic (agent mode on) and would mark almost every session.
	if um.GetIdeType() == sessionintelligencepb.IdeType_IDE_TYPE_CURSOR.String() {
		if um.GetSubAgentCount() > 0 || len(um.GetSpawnedAgentNames()) > 0 {
			return delegationLevelAssisted
		}
	}
	if um.GetIdeType() == "IDE_TYPE_KIRO" {
		if um.GetAutonomyMode() == "autopilot" {
			return delegationLevelDelegated
		}
		// Crew is Kiro's autonomous surface: the human posts a request from Slack or
		// the dashboard and the agent works unattended. That is delegated by
		// construction, and no per-session field says so — crew sets neither
		// autopilot nor spawned agent names, so without this every crew row lands on
		// "interactive" and autonomous usage reads as hands-on work.
		if um.GetSessionSource() == SessionSourceKiroCrew {
			return delegationLevelDelegated
		}
		if len(um.GetSpawnedAgentNames()) > 0 {
			return delegationLevelAssisted
		}
		// Safe here in a way it is not for Cursor: Kiro sets uses_task_agent only
		// when sub-agents actually ran (named agents, or a sub-execution transcript
		// on disk), whereas Cursor's is composerData.isAgentic — agent mode merely
		// being on. Sessions predating Kiro's sub_agent_start events have the
		// transcripts but no names, and are exactly the ones the check above misses.
		if um.GetUsesTaskAgent() {
			return delegationLevelAssisted
		}
	}
	return delegationLevelInteractive
}
