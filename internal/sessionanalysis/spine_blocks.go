// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"fmt"
	"strings"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
)

// measuredTag marks a rendered line as code-derived and authoritative — never a model
// guess — so R/E/D can tell a measured fact from a judged one at the point of use
// .
const measuredTag = "MEASURED — authoritative"

// capability is the producer-facing fidelity shape the capabilities deriver renders per
// session. Its vocabulary is deliberately the producer fence's own, decoupled from the
// TokenFidelity telemetry enum, so a change to the telemetry taxonomy does not rewrite the
// prompts — capabilityByFidelity below is where the two vocabularies meet.
type capability string

const (
	capabilityFull      capability = "full"
	capabilityPartial   capability = "partial"
	capabilityEstimated capability = "estimated"
	capabilityNone      capability = "none"
)

// capabilityByFidelity is the explicit fidelity→capability table (the sdlcStageByLabel
// pattern, llm_client.go): a plain switch cannot be reviewed as one unit the way a table
// can, and a new fidelity tier is a one-line addition here rather than a rewritten function.
var capabilityByFidelity = map[sessionintelligencepb.TokenFidelity]capability{
	sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED:       capabilityFull,
	sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_NATIVE_PARTIAL: capabilityPartial,
	sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_ESTIMATE:       capabilityEstimated,
	sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_UNSPECIFIED:    capabilityNone,
}

// tokenSplitCapabilityByCapability maps the spine's data-presence capability to the
// cap_token_split column enum — the two vocabularies are 1:1 (both grade how completely the
// capture reports the token split). The other cap_* columns (tool_outcome / reasoning_trace /
// assistant_text) have no clean data-presence signal in the conversation proto today, so they are
// left unset rather than graded by guesswork.
var tokenSplitCapabilityByCapability = map[capability]sessionintelligencepb.TokenSplitCapability{
	capabilityFull:      sessionintelligencepb.TokenSplitCapability_TOKEN_SPLIT_CAPABILITY_FULL,
	capabilityPartial:   sessionintelligencepb.TokenSplitCapability_TOKEN_SPLIT_CAPABILITY_PARTIAL,
	capabilityEstimated: sessionintelligencepb.TokenSplitCapability_TOKEN_SPLIT_CAPABILITY_ESTIMATED,
	capabilityNone:      sessionintelligencepb.TokenSplitCapability_TOKEN_SPLIT_CAPABILITY_NONE,
}

// tokenSplitCapabilityFor derives the cap_token_split value from the session's prompts, reusing
// capabilityFor so cap_token_split and the LLM-facing capabilities block can never disagree.
func tokenSplitCapabilityFor(prompts []*sessionintelligencepb.Prompt) *sessionintelligencepb.TokenSplitCapability {
	tokenSplitCap := tokenSplitCapabilityByCapability[capabilityFor(prompts)]
	return &tokenSplitCap
}

// machineTurnFilter reports whether a turn-group is human-authored; the shared spine drops
// every turn it says no to before assembly. It is a seam,
// not a single hardwired call: today's implementation (prefixHumanTurnFilter) is a
// role+is_subagent PREFIX bridge over the group's opening prompt; the durable replacement —
// reading the SessionEventAuthor stamp off session_events once the peer's presence-tracking
// follow-up lands — is a drop-in swap of this value, not a rewrite of the assembly.
type machineTurnFilter func(group []*sessionintelligencepb.Prompt) bool

// prefixHumanTurnFilter is today's machine-turn filter: a turn-group is human-authored iff
// its OPENING user prompt is (sessionintelligence.IsHumanPrompt — role_user and not a delegated agent's
// turn folded into the user role). "Opening" is the group's first ROLE_USER prompt, not
// necessarily group[0]: splitHumanTurns folds a leading non-user preamble into the first
// group ahead of its user prompt, so group[0] there is the preamble, not the human turn. A
// group with no user prompt at all is never human-authored. This depends on a role/
// is_sub_agent proxy rather than the authorship stamp itself, which is not carried on
// every upload.
func prefixHumanTurnFilter(group []*sessionintelligencepb.Prompt) bool {
	for _, prompt := range group {
		if prompt.GetRole() == sessionintelligencepb.Role_ROLE_USER {
			return sessionintelligence.IsHumanPrompt(prompt)
		}
	}
	return false
}

// turnBlock is one entry of the shared turn-grouped body every producer reads — a human
// prompt plus its following assistant/tool turns (splitHumanTurns' split, the same one
// prompt_index indexes elsewhere in the engine), carrying the measured telemetry
// buildMeasuredTurnRows already derives for the DB row.
type turnBlock struct {
	// Index is the turn's whole-session ordinal — splitHumanTurns' group position, stable
	// across chunked windows and equal to prompt_index elsewhere in the engine.
	Index int32
	// Sequence is the turn's 0-based position within THIS render: equal to Index on the
	// single-chunk fast path, but reset to 0 at the start of each chunked window, since a
	// window renders starting at prompt_index 0 and R must tag it from there.
	Sequence     int32
	Author       string
	IsSubagent   bool
	Messages     []*sessionintelligencepb.Prompt
	Tokens       *sessionintelligencepb.AnalysisTokenBreakdown
	Fidelity     sessionintelligencepb.TokenFidelity
	Models       []string
	ToolNames    []string
	FilesTouched *sessionintelligencepb.FileTouchList
}

// buildTurnBlocks derives the shared turn-grouped body over the whole-session turn-group
// range [rangeStart, rangeEnd), machine-turn-filtered, reusing buildMeasuredTurnRows'
// per-message derivation (turngrouper.go) so the LLM-facing block and the measured DB row
// can never disagree about a turn's tokens, fidelity, files, or authorship
// . groups and rows must be the same-length, index-aligned
// slices splitHumanTurns/buildMeasuredTurnRows produce over the same prompts; turnUsage is
// the session's TurnUsage list (SessionMeta.turn_usage — Kiro today), positionally aligned
// to the same whole-session ordinal (TurnUsage is documented as "one user turn's metering").
func buildTurnBlocks(groups [][]*sessionintelligencepb.Prompt, rows []*model.SessionTurnAnalysis, turnUsage []*sessionintelligencepb.TurnUsage, filter machineTurnFilter, rangeStart, rangeEnd int) []*turnBlock {
	blocks := make([]*turnBlock, 0, rangeEnd-rangeStart)
	var sequence int32
	for i := rangeStart; i < rangeEnd; i++ {
		group := groups[i]
		if !filter(group) {
			continue
		}
		row := rows[i]
		author := ""
		if row.Author != nil {
			author = *row.Author
		}
		fidelity := sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_UNSPECIFIED
		if row.TokenFidelity != nil {
			fidelity = *row.TokenFidelity
		}
		blocks = append(blocks, &turnBlock{
			Index:        int32(i), //nolint:gosec // group count is bounded by the parsed conversation length
			Sequence:     sequence,
			Author:       author,
			IsSubagent:   row.IsSubagent != nil && *row.IsSubagent,
			Messages:     group,
			Tokens:       row.TokenBreakdown,
			Fidelity:     fidelity,
			Models:       turnModels(group),
			ToolNames:    turnToolNamesFrom(turnUsage, i),
			FilesTouched: row.FilesTouched,
		})
		sequence++
	}
	return blocks
}

// sequenceToRawIndex returns, for a block slice buildTurnBlocks produced, the array mapping
// each block's rendered position (Sequence — what R tags as prompt_index) back to its raw
// whole-session ordinal (Index — what the measured turn table is keyed by). blocks is always
// in Sequence order (buildTurnBlocks appends with Sequence 0, 1, 2, ... in step), so
// indexMap[seq] is simply the Sequence-th block's Index.
func sequenceToRawIndex(blocks []*turnBlock) []int32 {
	indexMap := make([]int32, len(blocks))
	for _, block := range blocks {
		indexMap[block.Sequence] = block.Index
	}
	return indexMap
}

// rawIndexOf translates one rendered position (R's tagged prompt_index) to its raw whole-
// session ordinal via indexMap. ok is false for a position R could not have rendered (out of
// indexMap's bounds — a hallucinated tag), which the caller must treat as unresolvable rather
// than guessing a raw index for it.
func rawIndexOf(sequence int32, indexMap []int32) (int32, bool) {
	if sequence < 0 || int(sequence) >= len(indexMap) {
		return 0, false
	}
	return indexMap[sequence], true
}

// turnModels collects the distinct, non-empty per-message models used within a turn-group,
// in first-seen order — the same PromptMetadata.model field ExtractMetrics reads for
// pricing, read here per turn rather than per session.
func turnModels(group []*sessionintelligencepb.Prompt) []string {
	seen := map[string]bool{}
	var models []string
	for _, p := range group {
		model := p.GetMetadata().GetModel()
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	return models
}

// turnToolNamesFrom returns the turn's tool-call sequence from the session's TurnUsage list,
// aligned by the turn's whole-session ordinal. A session with no TurnUsage list (every
// non-Kiro surface today) or one shorter than this turn's ordinal reports no tool names for
// it — a decision-log gap (see the per-turn tool-outcome join it deliberately does not
// attempt), never a guessed empty measurement.
func turnToolNamesFrom(turnUsage []*sessionintelligencepb.TurnUsage, wholeSessionIndex int) []string {
	if wholeSessionIndex < 0 || wholeSessionIndex >= len(turnUsage) {
		return nil
	}
	return turnUsage[wholeSessionIndex].GetToolNames()
}

// capabilityFor derives the session's capability from data presence alone — the worst
// TokenFidelity any token-bearing message in the session reports, never from an enablement
// gate or an IDE-type allowlist: a session
// whose messages carry no token-bearing metadata at all (an older upload predating this
// metadata) has no fidelity signal to rank, and reports none rather than a false measured
// zero. It scans prompts directly rather than the turn-collapsed measured rows: a turn's
// stored TokenFidelity deliberately drops a token-bearing message that reports no fidelity so
// one unmeasured message does not drag the WHOLE TURN (turngrouper.go's applyMeasuredTurnColumns)
// — correct for that DB row, but capability asks the opposite question (any missing fidelity
// among counted messages must read none), so it needs the per-message signal the turn collapse
// discards. Reuses the same token-bearing test and worseFidelity (turngrouper.go) so the two
// readers can never disagree on what counts as token-bearing or which tier is worse.
func capabilityFor(prompts []*sessionintelligencepb.Prompt) capability {
	worst := sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED
	haveAny := false
	for _, p := range prompts {
		md := p.GetMetadata()
		if md == nil {
			continue
		}
		if md.GetInputTokens() == 0 && md.GetOutputTokens() == 0 && md.GetCacheReadInputTokens() == 0 && md.GetCacheCreationInputTokens() == 0 {
			continue // not token-bearing
		}
		fidelity := md.GetTokenFidelity()
		if !haveAny || worseFidelity(fidelity, worst) {
			worst = fidelity
		}
		haveAny = true
	}
	if !haveAny {
		return capabilityNone
	}
	return capabilityByFidelity[worst]
}

// buildSpineContent renders the shared structured blocks every producer reads — the
// machine-turn-filtered, turn-grouped body with its measured telemetry and author, plus the
// per-record capabilities block — into the single content string producers receive
// (block delivery is text in content, not a new wire field).
// Built unconditionally on every pass: the spine is the free, no-LLM static pass, and a
// flag-off org's capability degrades to none rather than this assembly being skipped — P3
// gates the producer CALL, not this render. The second return is the rendered blocks'
// Sequence->Index map (sequenceToRawIndex): the single-chunk fast path tags turns and phases
// against THIS render, so the caller (spine.go) carries it on spineResult to translate R's
// rendered positions back to the raw whole-session ordinal before they address the measured
// turn table — the chunked path builds its own per-window
// map instead (chunking.go), since a window's rendering resets Sequence to 0.
func buildSpineContent(orgID, rawRecordID string, sp *spineResult) (string, []int32) {
	blocks := buildSpineTurnBlocks(orgID, rawRecordID, sp)
	sp.turnBlocks = blocks
	var b strings.Builder
	for _, block := range blocks {
		renderTurnBlock(&b, block)
	}
	renderCapabilities(&b, capabilityFor(sp.prompts))
	return b.String(), sequenceToRawIndex(blocks)
}

// buildSpineTurnBlocks derives the shared machine-turn-filtered, turn-grouped body every producer
// reads — the same splitHumanTurns / buildMeasuredTurnRows / turn-usage assembly, cut once — so R's
// spine content and D's discard content render the identical turn set. The whole-session range is always the full group list; a windowed read builds its blocks
// through buildTurnBlocks directly with its own range instead.
func buildSpineTurnBlocks(orgID, rawRecordID string, sp *spineResult) []*turnBlock {
	groups := splitHumanTurns(sp.prompts)
	rows := buildMeasuredTurnRows(orgID, rawRecordID, sp)
	turnUsage := sp.usage.GetTurnUsage()
	return buildTurnBlocks(groups, rows, turnUsage, prefixHumanTurnFilter, 0, len(groups))
}

// renderTurnBlock writes one turn's header, its measured telemetry (tagged MEASURED —
// authoritative), and its rendered messages to b. A telemetry field absent on the turn (a
// nil token breakdown, no model, no tool names, no touched files) is omitted rather than
// rendered as a false zero.
func renderTurnBlock(b *strings.Builder, block *turnBlock) {
	// The header shows ONLY the per-render sequence (0..N-1 within this render/window) —
	// the single number R tags back as prompt_index. The block's whole-session raw Index is
	// internal (rebaseToRawIndex maps R's sequence back to it after the read); rendering it
	// here too would give R two conflicting numbers to echo and defeat that rebase on any
	// machine-turn-filtered session.
	fmt.Fprintf(b, "=== TURN %d (author=%s) ===\n", block.Sequence, block.Author)
	if block.IsSubagent {
		b.WriteString("is_subagent: true\n")
	}
	if len(block.Models) > 0 {
		fmt.Fprintf(b, "model: %s [%s]\n", strings.Join(block.Models, ","), measuredTag)
	}
	if block.Tokens != nil {
		fmt.Fprintf(b, "tokens: input=%d output=%d cache_read=%d cache_write=%d [%s]\n",
			block.Tokens.GetInputTokens(), block.Tokens.GetOutputTokens(),
			block.Tokens.GetCacheReadTokens(), block.Tokens.GetCacheWriteTokens(), measuredTag)
	}
	if len(block.ToolNames) > 0 {
		fmt.Fprintf(b, "tool_names: %s [%s]\n", strings.Join(block.ToolNames, ","), measuredTag)
	}
	renderFilesTouched(b, block.FilesTouched)
	b.WriteString(sessionintelligence.ConversationToText(block.Messages))
}

// renderFilesTouched writes the turn's edited and read file sets as separate, tagged lines,
// omitting a set that touched nothing.
func renderFilesTouched(b *strings.Builder, list *sessionintelligencepb.FileTouchList) {
	if list == nil || len(list.GetItems()) == 0 {
		return
	}
	var read, edited []string
	for _, item := range list.GetItems() {
		switch item.GetTouchKind() {
		case sessionintelligencepb.TouchKind_TOUCH_KIND_EDITED:
			edited = append(edited, item.GetPath())
		case sessionintelligencepb.TouchKind_TOUCH_KIND_READ:
			read = append(read, item.GetPath())
		case sessionintelligencepb.TouchKind_TOUCH_KIND_UNSPECIFIED:
			// No touch kind reported — omit the path rather than guess edited-vs-read.
		}
	}
	if len(edited) > 0 {
		fmt.Fprintf(b, "files_edited: %s [%s]\n", strings.Join(edited, ","), measuredTag)
	}
	if len(read) > 0 {
		fmt.Fprintf(b, "files_read: %s [%s]\n", strings.Join(read, ","), measuredTag)
	}
}

// renderCapabilities writes the per-record capabilities block every producer reads to decide
// whether to fire its abstain value instead of trusting a token/cost figure.
func renderCapabilities(b *strings.Builder, tokenFidelityCapability capability) {
	fmt.Fprintf(b, "=== CAPABILITIES ===\ntoken_fidelity_capability: %s [%s]\n", tokenFidelityCapability, measuredTag)
}
