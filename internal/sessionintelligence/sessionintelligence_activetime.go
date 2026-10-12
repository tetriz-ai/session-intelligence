// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"sort"
	"time"

	"go.uber.org/zap"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
)

// idleGapThreshold is the maximum gap between consecutive messages that still
// counts as "active" time. Gaps exceeding this are treated as idle (user stepped away).
// Definition: sum of inter-message durations, excluding idle gaps > 5 min.
const idleGapThreshold = 5 * 60 // 5 minutes in seconds

// minActiveFloorSeconds is the smallest Tier-2 result trusted as
// representative. A Tier-2 value below this floor, combined with a
// non-trivial message count, indicates that every real inter-message gap
// exceeded idleGapThreshold and only an incidental short gap (e.g. two
// near-simultaneous prompts) contributed. The caller falls through to Tier 3
// in that case so the heuristic estimate can produce a bounded value instead
// of locking in the incidental sub-floor reading.
const minActiveFloorSeconds = 5 * 60

// estimateActiveSecondsPerMessage is the Tier-3 fallback coefficient used when
// gap-filtered active time is zero (Cursor sessions with sparse synthetic
// timestamps where every inter-message gap exceeds idleGapThreshold).
//
// Source: a typical Cursor session with sparse timestamps — roughly 9 messages
// over a ~27-min wall-clock span — averages about 3 min of real interaction
// per prompt-response cycle (prompt composition + response review). The
// estimate is capped at wall-clock span so short sessions aren't inflated.
//
// This Tier-3 fallback applies to most Cursor sessions whose timestamps are too
// sparse to gap-filter; revisit if session-meta coverage rises enough to make
// Tier-3 rare.
const estimateActiveSecondsPerMessage = 180

// computeActiveTimeFromTimestamps sums inter-message gaps shorter than idleGapThreshold.
// Timestamps are ISO-8601 strings ("2026-04-10T14:30:00Z"). Returns total active seconds.
//
// Sorts input defensively so callers can pass merged streams (user + assistant
// timestamps) without pre-sorting. Denser streams capture more sub-threshold
// gaps; a user-only stream over a long session with sparse human input drops
// every inter-prompt gap and collapses toward zero.
//
// Returns 0 for sessions where ALL gaps exceed idleGapThreshold — the caller
// falls back to the per-message heuristic (see Tier 3 in ComputeNormalizedFields).
func computeActiveTimeFromTimestamps(timestamps []string) int64 {
	if len(timestamps) < 2 {
		return 0
	}
	parsed := make([]time.Time, 0, len(timestamps))
	for _, ts := range timestamps {
		t, err := parseFlexibleTimestamp(ts)
		if err != nil {
			logger.Warn("dropping unparseable message timestamp from active-time computation", zap.Error(err))
			continue
		}
		parsed = append(parsed, t)
	}
	if len(parsed) < 2 {
		return 0
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].Before(parsed[j]) })
	var activeS int64
	for i := 1; i < len(parsed); i++ {
		gap := parsed[i].Sub(parsed[i-1]).Seconds()
		if gap > 0 && gap < float64(idleGapThreshold) {
			activeS += int64(gap)
		}
	}
	return activeS
}

// computeTier3Estimate produces the per-user-message active-time estimate
// capped at wall-clock span. Returns 0 when insufficient input is available.
//
// Inputs are derived in priority order:
// - user_message_timestamps when length >= 2 (covers IDEs that supply it
// directly, including older uploads).
// - role=USER prompts in conv.Prompts[] when at least two carry timestamps
// (covers uploads where session-meta omits user_message_timestamps but
// prompts carry per-message timestamps).
//
// Count drives the multiplier so assistant-only padding cannot inflate the
// estimate; span bounds the result so a session left open across long idle
// periods is not over-reported.
func computeTier3Estimate(conv *sessionintelligencepb.Conversation, userTS []string) int64 {
	if len(userTS) < 2 {
		userTS = collectUserPromptTimestamps(conv)
	}
	if len(userTS) < 2 {
		return 0
	}
	first, err := parseFlexibleTimestamp(userTS[0])
	if err != nil {
		logger.Warn("dropping Tier-3 active-time estimate: unparseable first user timestamp", zap.Error(err))
		return 0
	}
	last, err := parseFlexibleTimestamp(userTS[len(userTS)-1])
	if err != nil {
		logger.Warn("dropping Tier-3 active-time estimate: unparseable last user timestamp", zap.Error(err))
		return 0
	}
	spanS := int64(last.Sub(first).Seconds())
	estimateS := int64(len(userTS)) * estimateActiveSecondsPerMessage
	if estimateS > spanS {
		estimateS = spanS
	}
	if estimateS < 0 {
		return 0
	}
	return estimateS
}

// IsHumanPrompt reports whether a prompt is the human speaking, as opposed to a
// delegated agent's turn folded into the same conversation.
//
// A surface whose sub-agent never becomes its own session (Kiro) folds those
// turns in here, so role alone no longer separates the human from the machine.
// Metrics about the person — how often they prompted, when, how many rounds it
// took — must use this; metrics about API structure or work performed read the
// role directly, since a delegated turn is a real turn. Exported so the
// session-analysis engine's machine-turn filter
// reuses this exact test rather than re-deriving it.
func IsHumanPrompt(p *sessionintelligencepb.Prompt) bool {
	return p.GetRole() == sessionintelligencepb.Role_ROLE_USER && !p.GetMetadata().GetIsSubAgent()
}

// collectUserPromptTimestamps extracts timestamps of user-role prompts from
// conv.Prompts[] as RFC3339Nano strings, sorted ascending. Returns an empty
// slice when no user prompts carry timestamps.
func collectUserPromptTimestamps(conv *sessionintelligencepb.Conversation) []string {
	if conv == nil {
		return nil
	}
	prompts := conv.GetPrompts()
	out := make([]string, 0, len(prompts))
	for _, p := range prompts {
		if !IsHumanPrompt(p) {
			continue
		}
		ts := p.GetTimestamp()
		if ts == nil || !ts.IsValid() {
			continue
		}
		out = append(out, ts.AsTime().UTC().Format(time.RFC3339Nano))
	}
	sort.Strings(out)
	return out
}

// collectPromptTimestamps returns the timestamp of every prompt in the
// conversation (both user and assistant) as ISO-8601 strings. Empty
// timestamps are skipped. Used to feed Tier 2 with a denser stream than
// user-only timestamps: assistant cadence within an extended turn typically
// falls under idleGapThreshold and contributes active-time signal that a
// user-only stream would miss.
func collectPromptTimestamps(conv *sessionintelligencepb.Conversation) []string {
	if conv == nil {
		return nil
	}
	prompts := conv.GetPrompts()
	out := make([]string, 0, len(prompts))
	for _, p := range prompts {
		ts := p.GetTimestamp()
		if ts == nil || !ts.IsValid() {
			continue
		}
		out = append(out, ts.AsTime().UTC().Format(time.RFC3339Nano))
	}
	return out
}

// computeContextUsagePercent returns the session-max context utilization (0.0–1.0)
// and a list of model names that were not found in the context window table.
// For each prompt, effective context = input_tokens + cache_read + cache_creation.
// Returns the highest ratio across all prompts, clamped to [0.0, 1.0].
//
// Clamping is necessary because for heavily cached multi-turn Claude Code
// sessions the per-turn cache_read_input_tokens can exceed the model's
// context window — Anthropic reports "tokens read from cache this turn",
// which for long sessions with a lot of repeated context can saturate
// well beyond the physical window. Without clamping the UI renders
// nonsense percentages (e.g. 423%).
func computeContextUsagePercent(prompts []*sessionintelligencepb.Prompt) (float64, []string) {
	var maxUsage float64
	unknownModels := make(map[string]struct{})
	for _, p := range prompts {
		m := p.GetMetadata()
		effective := effectiveContextTokens(m)
		if effective == 0 {
			continue
		}
		cw, known := contextWindowForModel(m.GetModel())
		if !known && m.GetModel() != "" {
			unknownModels[m.GetModel()] = struct{}{}
		}
		usage := float64(effective) / float64(cw)
		if usage > maxUsage {
			maxUsage = usage
		}
	}
	if maxUsage > 1.0 {
		maxUsage = 1.0
	}
	unknown := make([]string, 0, len(unknownModels))
	for m := range unknownModels {
		unknown = append(unknown, m)
	}
	return maxUsage, unknown
}

// computeSessionDuration returns the session duration in seconds.
// Returns 0 if either timestamp is nil or the duration is non-positive.
func computeSessionDuration(conv *sessionintelligencepb.Conversation) int64 {
	if conv == nil {
		return 0
	}
	// Kiro reports its own span, and it is the more accurate of the two. The
	// conversation's last_updated_at is the device's change-detection stamp, which
	// for Kiro covers the sub-execution files as well as the session — a delegated
	// transcript written after the session ended moves it, inflating the derived
	// duration by the gap. The vendor's own createdAt→lastModifiedAt does not move.
	if conv.GetIdeType() == sessionintelligencepb.IdeType_IDE_TYPE_KIRO {
		if mins := conv.GetSessionMeta().GetDurationMinutes(); mins > 0 {
			return int64(mins * 60)
		}
	}
	start := conv.GetConversationStartedAt()
	end := conv.GetLastUpdatedAt()
	if start == nil || !start.IsValid() || end == nil || !end.IsValid() {
		return 0
	}
	d := end.AsTime().Sub(start.AsTime())
	if d <= 0 {
		return 0
	}
	return int64(d.Seconds())
}
