// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
	"github.com/tetriz-ai/session-intelligence/internal/tenantdb"
)

// writeChildMeasuredTurns writes a delegated child conversation's measured turn rows,
// stamped with the parent locator, when a CHILD skip is admitted (session-turn-telemetry
// design, #6). A skipped child never reaches the commit path, so without this its turns are
// never written and the child-to-parent rollup has nothing to join. The write is fully
// zero-LLM: it reads and parses the child body (an S3 read, run OUTSIDE the tenant
// transaction ) and derives the code-owned measured columns exactly as the
// parent's grouper does, forcing the delegated-agent identity and leaving
// parent_prompt_index NULL for the parent's backfill to resolve.
//
// It runs BEFORE MarkSkipped so a grouping failure fails the skip (retryable) rather than
// marking the child skipped with no turn rows — matching the child-pricing discipline.
func (p *SessionAnalysisProcessor) writeChildMeasuredTurns(ctx context.Context, orgID string, record *model.RawRecordModel, meta *rawRecordMetadata, payload *rawRecordPayload) error {
	// S3 read + parse outside any tenant transaction: a held-open tx idling on an
	// S3 round-trip is killed by PostgreSQL.
	body, err := p.s3Client.ReadObject(ctx, payload.S3Key)
	if err != nil {
		return fmt.Errorf("read child s3 object %q: %w", payload.S3Key, err)
	}
	conv, err := sessionintelligence.ParseConversation(body)
	if err != nil {
		return fmt.Errorf("parse child conversation: %w", err)
	}
	// The per-body digest UpsertMeasured requires: the payload's when present, else the
	// spine's own body digest (a legacy body), mirroring the parent spine.
	contentSha256 := payload.ContentSha256
	if contentSha256 == "" {
		contentSha256 = ContentDigest(body)
	}
	sp := &spineResult{
		prompts:               conv.GetPrompts(),
		conversationStartedAt: conversationStart(conv, record.DataDate),
		contentSha256:         contentSha256,
		repoName:              meta.RepoName,
	}
	if len(sp.prompts) == 0 {
		return nil // an empty body cuts to no turns; nothing to write
	}
	rows := buildMeasuredTurnRowsWithChild(orgID, record.ID, sp, &childTurnLocator{
		parentConversationID: meta.ParentRecordKey,
		toolUseID:            meta.ToolUseID,
	})
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		return p.upsertAndRetireMeasured(sctx, orgID, record.ID, rows)
	}); err != nil {
		return fmt.Errorf("writing child measured turns (raw_record %s): %w", record.ID, err)
	}
	return nil
}

// childInWindowAndSeated reports whether a delegated child passes the SAME data-window and
// seat gates prefetchGate applies to a primary session. A child's measured turn rows roll
// up under its parent and the read side scopes them by the conversation day (the turn
// table's data_window policy) and by seat, so a child outside the org's window or whose
// person is unseated must not get turn rows the read side would exclude (session-turn-
// telemetry design, decision A). The skip itself is classified CHILD before those gates run,
// so this re-derives them on the child record here. Both knobs are tri-state through the
// planscoped helpers: an unbounded window / unlimited seats fails open and admits.
func (p *SessionAnalysisProcessor) childInWindowAndSeated(ctx context.Context, record *model.RawRecordModel, meta *rawRecordMetadata) bool {
	if cfg, ok := planscoped.ConfigFromContext(ctx); ok {
		if cutoff, capped := planscoped.DataCutoff(cfg); capped && startOfDayUTC(record.DataDate).Before(cutoff) {
			return false
		}
	}
	return planscoped.HasSeatPersonID(ctx, meta.PersonID)
}

// backfillChildParentPromptIndex resolves the parent_prompt_index on this parent's already-
// written child turn rows, from the dispatch → prompt_index map the spine derived
// . It is BEST-EFFORT and runs AFTER the parent commit, in
// its own tenant scope: a failure is logged, never propagated, because the child-refold scan
// leg (children_seq > folded_children_seq) re-admits the parent for a re-fold (which re-derives
// the map and re-runs this) whenever a child is (re-)sighted — coupling the child repair to the
// parent's terminal commit would risk losing the parent's judged verdict on a child-only failure
// (decision B). Skipped when the map is empty (a no-timeline parent, or one that
// delegated nothing).
func (p *SessionAnalysisProcessor) backfillChildParentPromptIndex(ctx context.Context, orgID string, record *model.RawRecordModel, sp *spineResult) {
	if len(sp.dispatchToolUseToPromptIndex) == 0 {
		return
	}
	if err := tenantdb.RunInScope(ctx, orgID, tenantdb.ScopeEnforced, func(sctx context.Context) error {
		_, err := p.sessionTurnDAO.BackfillChildParentPromptIndex(sctx, orgID, record.RecordKey, sp.dispatchToolUseToPromptIndex)
		if err != nil {
			return fmt.Errorf("backfilling child parent-prompt-index: %w", err)
		}
		return nil
	}); err != nil {
		logger.WarnWithCtx(ctx, "session analysis: child parent-prompt-index backfill failed (best-effort; re-runs on parent re-fold)",
			zap.String(logger.OrgID, orgID),
			zap.String("raw_record_id", record.ID),
			zap.Error(err))
	}
}

// deriveDispatchPromptIndex builds the map from a sub-agent dispatch's tool-use id to the
// parent human turn (splitHumanTurns group index == prompt_index) that spawned it, walking
// the normalized session_events timeline.
//
// PromptMetadata carries no tool-call id and the transcript strips tool_use blocks, so the
// event timeline is the only structured source. prompt_ordinal is a 1-based index into
// Conversation.prompts and is set only on PROMPT_REFERENCE events, NOT the group index that
// is prompt_index — so the walk translates the prompts-array position of the latest
// PROMPT_REFERENCE into the group index, and attributes each intervening dispatch's call_id
// to it. An assistant response shares its group with the preceding user prompt, and the
// extractor emits a message's tool events just before that message's PROMPT_REFERENCE, so
// the latest prompt_ordinal always resolves to the correct human turn.
//
// A Claude Code sub-agent dispatch (a `Task` tool_use naming a subagent_type) normalizes to
// a SKILL_CALL whose operation_name is the agent type and whose call_id is the dispatch's
// tool_use id — validated against a real parent+subagent dump (the child raw record's
// lineage tool_use_id equals the parent Task block id equals this call_id). So the dispatch
// set is SKILL_CALL and SUBAGENT_STARTED events, plus a TOOL_CALL named Task/Agent
// (defensive, for an adapter that emits the dispatch as a plain tool call). A skill or
// other call swept in never matches a child's parent_tool_use_id, so it is inert in the
// backfill. Returns nil when the body has no call-id-bearing timeline (a no-timeline
// adapter): those children stay NULL until a timeline-bearing re-parse resolves them.
func deriveDispatchPromptIndex(conv *sessionintelligencepb.Conversation) map[string]int32 {
	events := conv.GetSessionEvents()
	if len(events) == 0 {
		return nil
	}
	posGroup := promptPositionGroupIndex(conv.GetPrompts())

	// Walk by Sequence (the source-normalized order); copy + sort defensively rather than
	// assume the slice is already ordered.
	ordered := make([]*sessionintelligencepb.SessionEvent, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].GetSequence() < ordered[j].GetSequence() })

	out := map[string]int32{}
	var curGroup int32 = -1 // no current turn until the first PROMPT_REFERENCE
	for _, e := range ordered {
		if e.GetKind() == sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE {
			pos := int(e.GetPromptOrdinal()) - 1 // ordinal is 1-based into prompts
			if pos >= 0 && pos < len(posGroup) {
				curGroup = posGroup[pos]
			}
			continue
		}
		if curGroup >= 0 && e.GetCallId() != "" && isDispatchEvent(e) {
			out[e.GetCallId()] = curGroup
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isDispatchEvent reports whether a session event is (or may be) a sub-agent dispatch that
// spawns a child conversation. A Claude Code Task normalizes to SKILL_CALL; an inline span
// to SUBAGENT_STARTED; a plain-tool-call adapter would use TOOL_CALL named Task/Agent. A
// non-dispatch event swept in (a skill invocation, say) is harmless — its call_id never
// matches a child's parent_tool_use_id.
func isDispatchEvent(e *sessionintelligencepb.SessionEvent) bool {
	switch e.GetKind() {
	case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL,
		sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SUBAGENT_STARTED:
		return true
	case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_TOOL_CALL:
		n := e.GetOperationName()
		return n == "Task" || n == "Agent"
	default:
		return false
	}
}

// promptPositionGroupIndex maps each prompt-array position (0-based) to its splitHumanTurns
// group index (the prompt_index the grouper stamps). It mirrors splitHumanTurns exactly:
// each ROLE_USER prompt opens a group, and non-user prompts (leading ones included) attach
// to the current group — leading non-user prompts fold into group 0.
func promptPositionGroupIndex(prompts []*sessionintelligencepb.Prompt) []int32 {
	out := make([]int32, len(prompts))
	var g int32 = -1
	for i, prompt := range prompts {
		if prompt.GetRole() == sessionintelligencepb.Role_ROLE_USER {
			g++
			out[i] = g
			continue
		}
		if g < 0 {
			out[i] = 0 // leading non-user folds into the first group (group 0)
			continue
		}
		out[i] = g
	}
	return out
}
