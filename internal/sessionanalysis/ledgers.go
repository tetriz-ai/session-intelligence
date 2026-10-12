// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"fmt"
	"sort"
	"strings"

	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// spineLedgers is the free, static (no-LLM) fire evidence the spine builds on every
// session: the artifact/review ledger and the branch/revert
// ledger. They are the DESIGNED conditional-fire signals for the paid E and D producers —
// computed pre-LLM from the parsed body, never from a producer's own output. Gating on a
// code-built ledger (not R's judged list) is deliberate: the fire decision is code-owned
// and pre-computed, it holds even where R abstains, and E's prompt owes an adjudication row
// for every ledger invocation R may not have named (the LLM output contract,
// Producer E: "Whether E fires has been decided from the artifact ledger | code").
type spineLedgers struct {
	// artifactInvocations counts the harness producer/reviewer invocations E adjudicates —
	// skills, slash-commands, hooks, and delegated sub-agent runs. It deliberately EXCLUDES
	// generic tool calls (Read/Bash/Edit): those are not harness artifacts, and counting
	// them would fire E on nearly every session, defeating the cost gate that keeps E to the
	// minority of harness-using sessions (the LLM output contract, Producer E:
	// a thin-harness tenant never fires this call). Zero means no adjudicable artifact.
	artifactInvocations int
	// revertSignals counts the revert/abandon evidence D adjudicates: cancelled/failed
	// operations, user interruptions, git reset/revert/checkout events, deleted files, an
	// abandoned/unmet session outcome, and multiple branches (departed-not-returned). D's
	// gate is deliberately generous — firing on a session with no real discard is cheap (an
	// empty range list), missing one loses the signal (PRD, Producer D). Zero means none.
	revertSignals int
	// discardLedger retains the individual signals behind revertSignals — the matched revert
	// verbs, the branch count, the per-kind counts — so D's request can render each measured
	// fact (the BRANCH LEDGER the ANALYZE_DISCARD_FENCE reads) rather than the bare tally.
	// revertSignals stays the D gate; this is its evidence. Non-nil once buildSpineLedgers runs.
	discardLedger *discardLedgerDetail
	// invocations retains the per-artifact-invocation detail behind artifactInvocations — the
	// resolvable subset (a slug and its artifact kind) E's request renders as the LEDGER and
	// INVOCATIONS blocks and the artifact-intent resolver resolves to harness_artifacts rows
	// (the LLM output contract). artifactInvocations stays the E fire
	// gate; this is only the detail behind it, so a session firing E on a SessionMeta count with
	// no resolvable slug carries an empty detail rather than a phantom invocation.
	invocations []invocationDetail
}

// invocationDetail is one resolvable harness-artifact invocation retained behind
// spineLedgers.artifactInvocations: its bare slug, the artifact kind it names, the human turn it
// fired at when the timeline carries one, and its injected body when the source captured a bounded
// description. It carries what E's request needs to render the invocation and what the resolver
// needs to seek the artifact (slug + kind), never the count itself.
type invocationDetail struct {
	// artifactType is the harness artifact kind the invocation names (SKILL / COMMAND / HOOK /
	// AGENT) — the type axis of the ix_artifact_component resolve seek.
	artifactType commonv1.HarnessArtifactType
	// slug is the bare, lowercased invocation name (SessionEvent.operation_name, already
	// slash-stripped; a sub-agent's display name for a delegated run) — the resolve key.
	slug string
	// promptIndex is the raw whole-session human-turn index the invocation fired at — the same
	// splitHumanTurns group index the measured turn table and the child-dispatch backfill are
	// keyed by, so E can join an invocation to its turn. It is 0 both for the first human turn
	// (group 0) and for an invocation the timeline cannot place on a turn (a SessionMeta
	// sub-agent/spawned-agent run, or a timeline event before the first prompt reference); the
	// INVOCATIONS render emits it only when positive, so group 0 is not distinguished there.
	promptIndex int32
	// injectedBody is the bounded, already-redacted description the source captured for the
	// invocation, when any; "" when the source stored none. The spine never fetches the artifact
	// body from S3 to fill it — this is only what the parsed session already holds.
	injectedBody string
}

// discardLedgerDetail is the retained branch/revert evidence behind spineLedgers.revertSignals:
// the individual branch/revert signals behind that tally. It carries ONLY measured facts, and
// deliberately NOT the identifiers behind them — the raw timeline operation name and the branch
// names are never retained, only the bounded git verb and the branch count, so no identifier
// crosses the send boundary to the model (anonymizePaths hashes only extension-bearing tokens,
// which a branch name or an extensionless operation arg is not, so retaining either would leak it
// verbatim). The ANALYZE_DISCARD_FENCE tells the model the ledger is a weak hint and never proof,
// so the caveat is the prompt's job, not this render's (the LLM output contract).
// signalCount reproduces the D-gate tally exactly.
type discardLedgerDetail struct {
	// revertVerbs are the matched git revert/checkout/switch verbs (matchedRevertToken), one D
	// signal each.
	revertVerbs []string
	// cancelledOrFailed is the number of timeline operations that ended CANCELLED or FAILED —
	// one D signal each.
	cancelledOrFailed int
	// interruptions is the SessionMeta user-interruption count — one D signal each.
	interruptions int32
	// deletedFiles is the AiAttribution deleted-file count; a non-zero value is one D signal.
	deletedFiles int32
	// abandonedOutcome is true when the session facet outcome is abandoned/not_achieved — one
	// D signal.
	abandonedOutcome bool
	// branchCount is the number of git branches the session touched; more than one is the
	// departed-not-returned hint (one D signal).
	branchCount int
}

// signalCount is the sole D-gate tally over the retained signals — the exact sum firesDiscard's
// gate reads, so retaining the detail leaves the gate unchanged. TestBuildSpineLedgers (and the
// reproduces-tally test) pin the exact value, so a change here is caught.
func (d *discardLedgerDetail) signalCount() int {
	if d == nil {
		return 0
	}
	count := d.cancelledOrFailed + len(d.revertVerbs) + int(d.interruptions)
	if d.abandonedOutcome {
		count++
	}
	if d.deletedFiles > 0 {
		count++
	}
	if d.branchCount > 1 {
		count++
	}
	return count
}

// buildSpineLedgers derives the static E/D fire ledgers from the parsed conversation. It
// reads the normalized session-event timeline when the producing adapter emits one, and
// falls back to the vendor-reported SessionMeta aggregates otherwise, so a session on a
// no-timeline adapter still gates correctly on the counts its client did report.
func buildSpineLedgers(conv *sessionintelligencepb.Conversation) spineLedgers {
	var ledgers spineLedgers
	ledgers.artifactInvocations = countArtifactInvocations(conv)
	ledgers.invocations = buildInvocationDetails(conv)
	ledgers.discardLedger = buildDiscardLedgerDetail(conv)
	ledgers.revertSignals = ledgers.discardLedger.signalCount()
	return ledgers
}

// buildInvocationDetails collects the resolvable harness-artifact invocations of the session — the
// timeline's named skill/command/hook events (operation_name is the slug, the kind is the artifact
// type) plus the delegated sub-agent runs, and, only on a no-timeline session, the SessionMeta
// spawned-agent names. It is the detail behind countArtifactInvocations' fire tally, deliberately
// NARROWER: it keeps only invocations that carry a slug the resolver can seek, so a fire-gate
// signal with no resolvable name (a steering-doc / spec / hook COUNT on the no-timeline path)
// contributes to the gate but not to this detail. The two therefore agree on WHETHER E fires and
// may differ on the count, which is correct — a no-row invocation is not pending (it proceeds).
func buildInvocationDetails(conv *sessionintelligencepb.Conversation) []invocationDetail {
	var details []invocationDetail
	// prompt_ordinal is set only on PROMPT_REFERENCE events (zero on skill/command/hook), so a
	// skill/command/hook event cannot report its own turn; resolve each one's surrounding human
	// turn by walking the ordered timeline instead — the same mechanism the child-dispatch
	// backfill uses.
	promptIndexBySeq := timelinePromptIndexBySequence(conv)
	for _, event := range conv.GetSessionEvents() {
		artifactType, ok := invocationArtifactType(event.GetKind())
		if !ok {
			continue
		}
		slug := strings.ToLower(strings.TrimSpace(event.GetOperationName()))
		if slug == "" {
			continue
		}
		details = append(details, invocationDetail{
			artifactType: artifactType,
			slug:         slug,
			promptIndex:  promptIndexBySeq[event.GetSequence()],
			injectedBody: event.GetRedactedSummary(),
		})
	}
	meta := conv.GetSessionMeta()
	// A delegated sub-agent run is an AGENT-kind invocation whether or not the adapter emitted a
	// timeline event for it, so it resolves on both paths — matching the fire tally, which also
	// counts SubAgentRuns unconditionally.
	for _, run := range meta.GetSubAgentRuns() {
		slug := strings.ToLower(strings.TrimSpace(run.GetName()))
		if slug == "" {
			continue
		}
		details = append(details, invocationDetail{
			artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
			slug:         slug,
		})
	}
	if len(conv.GetSessionEvents()) == 0 {
		// No timeline: the spawned-agent names are the only remaining named invocations a
		// no-timeline client reports (the fire tally adds them on the same condition).
		for _, name := range meta.GetSpawnedAgentNames() {
			slug := strings.ToLower(strings.TrimSpace(name))
			if slug == "" {
				continue
			}
			details = append(details, invocationDetail{
				artifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
				slug:         slug,
			})
		}
	}
	return details
}

// timelinePromptIndexBySequence resolves, for each session event, the raw whole-session human turn
// it fell in — keyed by SessionEvent.sequence, valued by the splitHumanTurns group index that is the
// turn table's prompt_index. It is the same walk deriveDispatchPromptIndex runs for delegated
// dispatch, generalized off call_id to a per-event sequence key so any event kind (not just a
// dispatch) can be placed on its turn: prompt_ordinal is set only on PROMPT_REFERENCE (zero on skill/
// command/hook/tool events), so the walk translates the latest PROMPT_REFERENCE's prompts-array
// position to its group index via promptPositionGroupIndex and attributes every following event to
// that turn until the next reference. An event before the first PROMPT_REFERENCE gets no entry, so a
// caller reads its zero value (no resolvable turn). Nil when the body carries no timeline.
func timelinePromptIndexBySequence(conv *sessionintelligencepb.Conversation) map[int64]int32 {
	events := conv.GetSessionEvents()
	if len(events) == 0 {
		return nil
	}
	posGroup := promptPositionGroupIndex(conv.GetPrompts())

	// Walk by Sequence (the source-normalized order); copy + sort defensively rather than assume
	// the slice is already ordered, matching deriveDispatchPromptIndex.
	ordered := make([]*sessionintelligencepb.SessionEvent, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].GetSequence() < ordered[j].GetSequence() })

	out := map[int64]int32{}
	var curGroup int32 = -1 // no current turn until the first PROMPT_REFERENCE
	for _, event := range ordered {
		if event.GetKind() == sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_PROMPT_REFERENCE {
			pos := int(event.GetPromptOrdinal()) - 1 // ordinal is 1-based into prompts
			if pos >= 0 && pos < len(posGroup) {
				curGroup = posGroup[pos]
			}
			continue
		}
		if curGroup >= 0 {
			out[event.GetSequence()] = curGroup
		}
	}
	return out
}

// invocationArtifactType maps a harness-invocation session-event kind to the harness artifact type
// it names; ok is false for a non-artifact event kind (a tool call, a prompt reference, a goal),
// which contributes no resolvable invocation.
func invocationArtifactType(kind sessionintelligencepb.SessionEventKind) (commonv1.HarnessArtifactType, bool) {
	switch kind {
	case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, true
	case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMMAND:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND, true
	case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_HOOK_CALL:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK, true
	default:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_UNSPECIFIED, false
	}
}

// firesEvidence reports whether E (the evidence pass) has anything to adjudicate — the
// conditional E-trigger: a non-empty artifact ledger, OR a confirmed round-trip pair. The
// round-trip half is the tag-informed confirmation applied after R (one detection
// algorithm, confirmed against R's turn tags), so it reads
// R's output; the artifact half is the spine's own code-built signal.
func (l spineLedgers) firesEvidence(read *ReadResult) bool {
	return l.artifactInvocations > 0 || confirmedRoundTrip(read)
}

// firesDiscard reports whether D (the discard tail) should run — the conditional
// D-trigger: a non-empty branch/revert ledger. D reads no producer output, so its gate is
// purely the spine's static evidence.
func (l spineLedgers) firesDiscard() bool {
	return l.revertSignals > 0
}

// countArtifactInvocations counts the harness producer/reviewer invocations in the session.
// When the adapter emitted a normalized timeline, the harness-invocation event kinds are the
// authoritative count; otherwise it falls back to the SessionMeta signals a no-timeline
// client still reports (delegated sub-agent runs, spawned-agent names).
func countArtifactInvocations(conv *sessionintelligencepb.Conversation) int {
	var count int
	for _, event := range conv.GetSessionEvents() {
		switch event.GetKind() {
		case sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_SKILL_CALL,
			sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_COMMAND,
			sessionintelligencepb.SessionEventKind_SESSION_EVENT_KIND_HOOK_CALL:
			count++
		default:
			// TOOL_CALL / TOOL_RESULT / PROMPT_REFERENCE / GOAL / UNSPECIFIED are not harness
			// artifacts, so they do not fire E.
		}
	}
	meta := conv.GetSessionMeta()
	// A delegated sub-agent run is a producer/reviewer invocation whether or not the adapter
	// emitted a timeline event for it, so it counts on both paths.
	count += len(meta.GetSubAgentRuns())
	if len(conv.GetSessionEvents()) == 0 {
		// No timeline: fall back to the vendor aggregates that name harness invocations.
		count += len(meta.GetSpawnedAgentNames())
		count += len(meta.GetSteeringDocsUsed())
		count += int(meta.GetSpecCount())
		count += int(meta.GetHookCount())
	}
	return count
}

// revertOperationTokens are the substrings, matched case-insensitively against an event's
// operation name, that mark a discard: work undone (reset/revert), or the working tree moved
// off the branch the work was on (checkout/switch). A weak hint by design — the D prompt is
// told the ledger is not proof (the LLM output contract).
var revertOperationTokens = []string{"git reset", "git revert", "git checkout", "git switch", "revert"}

// buildDiscardLedgerDetail collects the branch/revert evidence in the session — cancelled or
// failed operations and revert-shaped commands from the timeline when present, plus the
// SessionMeta aggregates (interruptions, deleted files, an abandoned/unmet outcome, multiple
// branches) that a no-timeline client still reports. It retains each signal so D's request can
// render it; signalCount over the result reproduces the historical revert tally exactly.
func buildDiscardLedgerDetail(conv *sessionintelligencepb.Conversation) *discardLedgerDetail {
	detail := &discardLedgerDetail{}
	for _, event := range conv.GetSessionEvents() {
		switch event.GetOutcome() {
		case sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_CANCELLED,
			sessionintelligencepb.SessionEventOutcome_SESSION_EVENT_OUTCOME_FAILED:
			detail.cancelledOrFailed++
		default:
			// succeeded / unknown / unspecified is not a discard signal.
		}
		if verb, ok := matchedRevertToken(event.GetOperationName()); ok {
			detail.revertVerbs = append(detail.revertVerbs, verb)
		}
	}
	meta := conv.GetSessionMeta()
	detail.interruptions = meta.GetUserInterruptions()
	if outcome := strings.ToLower(conv.GetSessionFacets().GetOutcome()); outcome == "abandoned" || outcome == "not_achieved" {
		detail.abandonedOutcome = true
	}
	detail.deletedFiles = conv.GetAiAttribution().GetDeletedFilesCount()
	// More than one branch is a departed-not-returned hint: the session moved off a branch it
	// had been working on. Exactly one branch (or none) is not.
	detail.branchCount = len(meta.GetGitBranches())
	return detail
}

// renderBranchLedger renders the flat BRANCH LEDGER block D reads — one line per signal kind that
// fired, each carrying its measured evidence and tagged authoritative. The block is the measured
// facts only: the fence tells D the ledger is a weak hint and never proof, so no line editorializes
// (the LLM output contract). A nil/empty ledger renders the header with an
// explicit no-signal line rather than a bare header, so the block is never ambiguous.
func renderBranchLedger(detail *discardLedgerDetail) string {
	var b strings.Builder
	b.WriteString("=== BRANCH LEDGER ===\n")
	wrote := false
	if detail != nil {
		if len(detail.revertVerbs) > 0 {
			fmt.Fprintf(&b, "revert_operations: %s [%s]\n", strings.Join(detail.revertVerbs, ", "), measuredTag)
			wrote = true
		}
		if detail.cancelledOrFailed > 0 {
			fmt.Fprintf(&b, "cancelled_or_failed_operations: %d [%s]\n", detail.cancelledOrFailed, measuredTag)
			wrote = true
		}
		if detail.interruptions > 0 {
			fmt.Fprintf(&b, "user_interruptions: %d [%s]\n", detail.interruptions, measuredTag)
			wrote = true
		}
		if detail.deletedFiles > 0 {
			fmt.Fprintf(&b, "deleted_files: %d [%s]\n", detail.deletedFiles, measuredTag)
			wrote = true
		}
		if detail.abandonedOutcome {
			fmt.Fprintf(&b, "session_outcome: abandoned [%s]\n", measuredTag)
			wrote = true
		}
		if detail.branchCount > 1 {
			fmt.Fprintf(&b, "departed_not_returned_branches: %d [%s]\n", detail.branchCount, measuredTag)
			wrote = true
		}
	}
	if !wrote {
		b.WriteString("no measured revert signals\n")
	}
	return b.String()
}

// matchedRevertToken returns the FIRST revertOperationTokens entry contained in the lowercased
// operation name, and whether any matched — the bounded git verb the ledger retains in place of the
// raw operation name. It is the sole matcher, so the fire tally and the retained verb cannot drift.
func matchedRevertToken(operationName string) (string, bool) {
	name := strings.ToLower(operationName)
	for _, token := range revertOperationTokens {
		if strings.Contains(name, token) {
			return token, true
		}
	}
	return "", false
}
