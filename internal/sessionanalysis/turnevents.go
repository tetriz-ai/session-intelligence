// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"sort"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// applyEventDerivedTurnColumns is the measured grouper's SECOND fold: it walks the
// normalized session_events timeline and stamps the event-grain columns the per-message
// metadata cannot carry — the tool family (tool_call_count, tool_error_count, and the
// per-invocation tools list), tool_retry_count, compaction_count, subagent_count (all
// turn-grain), and the precise event-authorship stamp that replaces the per-message
// is_subagent approximation applyMeasuredTurnColumns left.
//
// The tools column is the per-invocation ToolCallList {name, call_id, outcome, failure_kind} —
// one entry per TOOL_CALL, its outcome stamped from the paired TOOL_RESULT — and
// tool_call_count / tool_error_count are the aggregates DERIVED from it (call total; count
// whose paired result FAILED). failure_kind stays empty: no source event carries a failure
// categorization (a TOOL_RESULT reports only an outcome, plus a checker tool's text summary),
// so the field is left unset rather than synthesized — the same reason the session-turn-telemetry
// design leaves a tool's failure class unrecorded (no provider signal distinguishes it). SKILL/HOOK/
// COMMAND calls are not tools here — the family keys on TOOL_CALL exactly as tool_retry_count
// does, so the counts stay consistent across the family.
//
// Outcome attribution is by the CALL's turn, not the result event's: the outcome stamp targets
// the paired invocation OBJECT (by parent_call_id via toolInvocationByID), so it lands on the turn
// the call was made in even when the result event is walked under a LATER turn — a call and its
// result CAN straddle a turn boundary (the "result in a later turn" test constructs exactly that).
// That pointer targeting is turn-agnostic by construction, which is why the stamp needs no
// within-turn guard, unlike the retry path that keys pendingFailures on the result's current turn.
//
// Zero-vs-null gate (DECIDED — session_events presence): a computed count is written as a
// real 0 when the payload HAS a session_events timeline, and left NULL when it has none
// ("couldn't observe"). Timeline presence is a payload-level property, so when events are
// present EVERY turn gets its count columns (0 where nothing matched); when absent the
// columns are left nil for the whole record. The gate is NOT extractor_version (a
// force-resync counter, not a per-feature version) and NOT event_coverage (which would
// erase measured zeros).
//
// KNOWN, ACCEPTED OVER-CLAIM: a Claude Code build from the window that emitted
// session_events but not yet the COMPACTION event (roughly the→ client range)
// carries a timeline, so its turns get compaction_count=0 — a "measured zero" that is
// really "too old to say". Those payloads age out of the data window, so the approximation
// is bounded and accepted rather than gated around.
//
// api_retry_count is deliberately NOT derived here: no extractor emits an API_RETRY event,
// so a walk for it would be dead code. It stays NULL.
//
// The pass runs on the PARENT grouper only. A delegated child's rows carry the forced
// delegated-agent identity and populate no timeline, so they keep these columns NULL by
// design (and this returns early on the nil timeline anyway).
func applyEventDerivedTurnColumns(rows []*model.SessionTurnAnalysis, sp *spineResult) {
	if len(sp.sessionEvents) == 0 {
		return // no timeline: leave the count columns NULL ("couldn't observe")
	}
	numGroups := len(rows)
	if numGroups == 0 {
		return
	}
	// posGroup maps each prompts[] position to its splitHumanTurns group index (== the
	// prompt_index the grouper stamped), the SAME cut deriveDispatchPromptIndex and
	// splitHumanTurns use — so an event's turn is the turn opened by the most-recent
	// turn-opening PROMPT_REFERENCE.
	posGroup := promptPositionGroupIndex(sp.prompts)

	// Walk by sequence (the source-normalized order); copy + sort defensively rather than
	// assume the slice is already ordered, mirroring deriveDispatchPromptIndex.
	ordered := make([]*sessionintelligencepb.SessionEvent, len(sp.sessionEvents))
	copy(ordered, sp.sessionEvents)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].GetSequence() < ordered[j].GetSequence() })

	retryByGroup := make([]int32, numGroups)
	compactionByGroup := make([]int32, numGroups)
	// Per-invocation tool family. toolsByGroup accumulates one ToolCall per TOOL_CALL in the
	// turn (name + call_id at call time); toolInvocationByID lets the paired TOOL_RESULT stamp
	// that invocation's outcome. tool_call_count and tool_error_count are derived from these in
	// the final fold, so the aggregates never drift from the list.
	toolsByGroup := make([][]*sessionintelligencepb.ToolCall, numGroups)
	toolInvocationByID := map[string]*sessionintelligencepb.ToolCall{}
	// subagentByGroup counts the inline sub-agent dispatches (SUBAGENT_STARTED spans) opened in
	// each turn. Only an inline-span provider (Kiro) emits these; a non-Kiro session folds its
	// sub-agents into separate child records instead, so subagent_count is stamped only for Kiro
	// (NULL elsewhere — "no inline signal", not a measured 0).
	subagentByGroup := make([]int32, numGroups)
	// The opening PROMPT_REFERENCE's author per turn — UNSPECIFIED until seen, then
	// first-wins (a turn's opening user prompt precedes its assistant references in
	// sequence order, so the first reference attributed to the group is the opener).
	openingAuthor := make([]sessionintelligencepb.SessionEventAuthor, numGroups)

	// Retry bookkeeping. A TOOL_RESULT pairs to its TOOL_CALL by id: the extractor stamps the
	// call's id in call_id and the result's parent_call_id (result.parent_call_id ==
	// call.call_id). The retry-defining summary lives on the CALL (commandSummaryOf: the
	// tool's command/file_path/pattern/…), not the result (usually empty). So a retry is a
	// FAILED result whose paired call carried a non-empty summary, followed WITHIN THE SAME
	// TURN by another call with the same operation_name + summary. A call with no summary
	// cannot be shown to be the same call, so it derives no retry (a deliberate rule).
	type toolCall struct{ op, summary string }
	callByID := map[string]toolCall{}
	// pendingFailures[group][op+summary] counts failed attempts in that turn awaiting a re-issue.
	pendingFailures := make([]map[string]int, numGroups)

	curGroup := int32(-1) // no current turn until the first PROMPT_REFERENCE
	for _, e := range ordered {
		switch e.GetKind() {
		case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE:
			pos := int(e.GetPromptOrdinal()) - 1 // ordinal is 1-based into prompts
			if pos >= 0 && pos < len(posGroup) {
				curGroup = posGroup[pos]
				if int(curGroup) < numGroups && openingAuthor[curGroup] == sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_UNSPECIFIED {
					openingAuthor[curGroup] = e.GetAuthor()
				}
			}
		case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMPACTION:
			if curGroup >= 0 && int(curGroup) < numGroups {
				compactionByGroup[curGroup]++
			}
		case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED:
			// One inline sub-agent dispatch opened in this turn (the SUBAGENT_COMPLETED close is
			// not counted — a dispatch is one delegation regardless of how it ended).
			if curGroup >= 0 && int(curGroup) < numGroups {
				subagentByGroup[curGroup]++
			}
		case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL:
			if curGroup < 0 || int(curGroup) >= numGroups {
				continue
			}
			// The per-invocation record for the tools list (and the call-count aggregate); its
			// outcome stays UNSPECIFIED until the paired TOOL_RESULT stamps it below.
			tc := &sessionintelligencepb.ToolCall{Name: e.GetOperationName(), CallId: e.GetCallId()}
			toolsByGroup[curGroup] = append(toolsByGroup[curGroup], tc)
			call := toolCall{op: e.GetOperationName(), summary: e.GetRedactedSummary()}
			if cid := e.GetCallId(); cid != "" {
				callByID[cid] = call
				toolInvocationByID[cid] = tc
			}
			// A re-issue of a same-turn failed call (equal op + non-empty summary) is a retry.
			if call.summary == "" {
				continue
			}
			if pf := pendingFailures[curGroup]; pf != nil && pf[call.op+"\x00"+call.summary] > 0 {
				retryByGroup[curGroup]++
				pf[call.op+"\x00"+call.summary]--
			}
		case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_RESULT:
			// Stamp the paired invocation's outcome for the tools list — ALL outcomes, not just
			// FAILED, so tool_error_count is derivable from the list. Runs before the FAILED gate
			// the retry bookkeeping keeps.
			if tc, ok := toolInvocationByID[e.GetParentCallId()]; ok {
				tc.Outcome = e.GetOutcome()
			}
			if e.GetOutcome() != sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED {
				continue
			}
			if curGroup < 0 || int(curGroup) >= numGroups {
				continue
			}
			c, ok := callByID[e.GetParentCallId()]
			if !ok || c.summary == "" {
				continue // no paired call, or no summary → no retry can be derived
			}
			if pendingFailures[curGroup] == nil {
				pendingFailures[curGroup] = map[string]int{}
			}
			pendingFailures[curGroup][c.op+"\x00"+c.summary]++
		default:
			// Other event kinds (skill/hook/command/goal, turn lifecycle, SUBAGENT_COMPLETED,
			// API_RETRY) carry no event-derived turn column here.
		}
	}

	for g, row := range rows {
		// Present-aware: a real 0 is stamped because the timeline WAS observed, distinct
		// from the NULL a no-timeline record keeps.
		rc := retryByGroup[g]
		row.ToolRetryCount = &rc
		cc := compactionByGroup[g]
		row.CompactionCount = &cc
		// Tool family, same presence gate: the timeline was observed, so a turn with no tool
		// calls gets a measured 0 and an empty-but-non-nil tools list (never NULL). tool_call_count
		// is the invocation total; tool_error_count is the count whose paired result FAILED.
		callCount := int32(len(toolsByGroup[g])) //nolint:gosec // bounded by the parsed timeline length
		row.ToolCallCount = &callCount
		var errCount int32
		for _, tc := range toolsByGroup[g] {
			if tc.GetOutcome() == sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED {
				errCount++
			}
		}
		row.ToolErrorCount = &errCount
		row.Tools = &sessionintelligencepb.ToolCallList{Items: toolsByGroup[g]}
		// subagent_count is the inline-span dispatch count — an inline signal only Kiro emits, so it
		// is stamped only for Kiro (0 is a measured zero there); a non-Kiro row keeps it NULL ("no
		// inline signal"), its sub-agents living in separate child records instead.
		if sp.ideType == sessionintelligencepb.IdeType_IDE_TYPE_KIRO {
			sc := subagentByGroup[g]
			row.SubagentCount = &sc
		}
		// Precise authorship supersedes the per-message approximation only when the turn's
		// opening PROMPT_REFERENCE named an author; absent that, the approximation stands.
		if a := openingAuthor[g]; a != sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_UNSPECIFIED {
			applyPreciseAuthorship(row, a)
		}
	}
}

// applyPreciseAuthorship stamps a turn's identity from its opening PROMPT_REFERENCE's
// author, replacing the is_subagent approximation: HUMAN is a person's turn; AGENT and
// DELEGATED_AGENT are both agent-driven, and only DELEGATED_AGENT is a sub-agent (an
// injected AGENT turn in a top-level session is machine-driven but not delegated work).
// The Author column stores the enum name, matching applyMeasuredTurnColumns and
// forceDelegatedAgentIdentity.
func applyPreciseAuthorship(row *model.SessionTurnAnalysis, author sessionintelligencepb.SessionEventAuthor) {
	name := author.String()
	row.Author = &name
	isSubagent := author == sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_DELEGATED_AGENT
	row.IsSubagent = &isSubagent
	origination := sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_AGENT_DRIVEN
	if author == sessionintelligencepb.SessionEventAuthor_SESSION_EVENT_AUTHOR_HUMAN {
		origination = sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN
	}
	row.PromptOrigination = &origination
}
