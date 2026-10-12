// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/logger"
)

// runReadPass runs R over the session. On the ~95% single-chunk fast path it is one
// AnalyzeRead call over the whole rendered transcript (unchanged). When the spine marked
// the session chunked (rendered size over the char limit), it runs the windowed digest
// chain instead — one analyze_read per window, stitched by prompt_index
// . The whole chain shares one READ key in call_state and is
// all-or-nothing: any window error aborts the pass with no partial commit, so the next
// tick re-runs the whole chain. Either path returns a ReadResult already translated to the
// raw whole-session prompt_index — the single-chunk path via rebaseToRawIndex, the chunked
// path via its own per-window stitch — so no caller past
// this point ever sees R's rendered-position tagging.
func (p *SessionAnalysisProcessor) runReadPass(ctx context.Context, sp *spineResult, baseReq ProducerRequest) (*ReadResult, error) {
	if !sp.chunked {
		read, err := runSingleChunkRead(ctx, p.producer, sp, baseReq)
		if err != nil {
			return nil, fmt.Errorf("single-chunk read pass: %w", err)
		}
		return read, nil
	}
	read, err := p.runChunkedRead(ctx, sp, baseReq)
	if err != nil {
		return nil, fmt.Errorf("chunked read pass: %w", err)
	}
	return read, nil
}

// rebaseToRawIndex translates the single-chunk read pass's turn and phase positions from R's
// rendered Sequence numbering to the raw whole-session Index the measured turn table is keyed
// by — the chunked path does its own per-window rebase in
// stitchWindowTurns/stitchWindowPhases. A turn tag R could not have rendered a raw index for
// (out of the render's bounds — a hallucinated tag) is set to -1, a prompt_index no measured
// row ever has, so the commit's existing "no measured row matched" drop absorbs it exactly as
// any other out-of-cut index, rather than silently landing on whatever turn raw index 0 is.
func rebaseToRawIndex(read *ReadResult, indexMap []int32) {
	if read == nil {
		return
	}
	for _, turn := range read.Turns {
		if raw, ok := rawIndexOf(turn.PromptIndex, indexMap); ok {
			turn.PromptIndex = raw
		} else {
			turn.PromptIndex = -1
		}
	}
	for _, phase := range read.Phases {
		if phase.RangeStart != nil {
			if raw, ok := rawIndexOf(*phase.RangeStart, indexMap); ok {
				phase.RangeStart = &raw
			}
		}
		if phase.RangeEnd != nil {
			if raw, ok := rawIndexOf(*phase.RangeEnd, indexMap); ok {
				phase.RangeEnd = &raw
			}
		}
	}
}

// rebaseDiscardToRawIndex translates D's discarded-range locators from the rendered Sequence
// numbering (D reads the same machine-turn-filtered whole-session render R does — buildDiscardContent
// over sp.turnBlocks) to the raw whole-session Index that promptTokenCosts and the measured turn
// table are keyed by. Without it, sumDiscardedTokens would index the raw cost slice with a Sequence
// position and attribute a filtered session's waste to the wrong turn. A range whose start or end is
// out of the render's bounds (a hallucinated locator) is DROPPED — mirroring sumDiscardedTokens'
// reject-not-clamp rule — and an unresolvable evidence_prompt_index becomes -1 (a prompt_index no
// measured row has). rawIndexOf is monotonic, so a valid rendered span stays an ordered raw span
// (start <= end); a multi-turn span whose rendered bounds straddle a filtered machine turn therefore
// spans that turn's raw index too, counting the interleaved sub-agent work as part of the discarded
// exchange. Runs once after AnalyzeDiscard, before the waste ledger
// reads the ranges — the D-side counterpart to rebaseToRawIndex for R.
func rebaseDiscardToRawIndex(discard *DiscardResult, indexMap []int32) {
	if discard == nil || discard.DiscardedRanges == nil {
		return
	}
	items := discard.DiscardedRanges.GetItems()
	kept := items[:0]
	for _, r := range items {
		rawStart, okStart := rawIndexOf(r.GetRangeStart(), indexMap)
		rawEnd, okEnd := rawIndexOf(r.GetRangeEnd(), indexMap)
		if !okStart || !okEnd {
			continue // a bound R could not have rendered — drop the whole range
		}
		r.RangeStart = rawStart
		r.RangeEnd = rawEnd
		if rawEvidence, ok := rawIndexOf(r.GetEvidencePromptIndex(), indexMap); ok {
			r.EvidencePromptIndex = rawEvidence
		} else {
			r.EvidencePromptIndex = -1
		}
		kept = append(kept, r)
	}
	discard.DiscardedRanges.Items = kept
}

// readWindow is one window of the chunked read: a main range of human turns whose tags are
// authoritative, plus a read-only look-ahead the model may consult but whose tags are
// discarded (the next window's main range owns them).
type readWindow struct {
	mainStart int // first human-turn ordinal in the authoritative main range
	mainEnd   int // one past the last main-range human turn
	lookEnd   int // one past the last look-ahead human turn (mainEnd <= lookEnd)
}

// runChunkedRead runs the sequential windowed read chain. Each window renders only its own
// human turns (main + look-ahead) as taggable transcript, prefixed by a code-built digest
// of the already-judged prior turns, so R's per-window output stays under the output
// ceiling. The
// windows' partial turn tables are stitched by re-based prompt_index; the final window,
// carrying the whole-session digest, lands the verdict.
func (p *SessionAnalysisProcessor) runChunkedRead(ctx context.Context, sp *spineResult, baseReq ProducerRequest) (*ReadResult, error) {
	groups := splitHumanTurns(sp.prompts)
	windows := planWindows(len(groups), p.cfg.ChunkWindowPrompts(), p.cfg.ChunkLookaheadPrompts())
	if len(windows) == 0 {
		// No human turns to window (an all-assistant body over the char limit); fall back
		// to a single pass over the whole transcript rather than emitting nothing. This
		// renders the identical content the single-chunk fast path would (baseReq.Content is
		// still the whole-session render), so it is rebased through the same sp.turnIndexMap.
		read, err := p.producer.AnalyzeRead(ctx, baseReq)
		if err != nil {
			return nil, fmt.Errorf("single-window fallback read: %w", err)
		}
		rebaseToRawIndex(read, sp.turnIndexMap)
		return read, nil
	}

	// A chunked read is a rare, high-cost event — one analyze_read call per window rather
	// than one over the whole session — whose only other durable trace is chunk_index on the
	// committed turns. Log it once per session so the windowed-read rate and fan-out are
	// visible in prod without a table query. rendered_chars is the whole-session render size
	// counted the same way the spine gated on — runes, not bytes (spine.go: len([]rune(...)) >
	// ChunkCharLimit) — so the logged size is the exact quantity that crossed the limit.
	logger.Info(ctx, "session analysis: chunked read",
		zap.String(logger.OrgID, baseReq.OrgID),
		zap.String(logger.SessionID, baseReq.SessionID),
		zap.Int("rendered_chars", len([]rune(baseReq.Content))),
		zap.Int("human_turn_groups", len(groups)),
		zap.Int("windows", len(windows)),
	)

	// The shared blocks (turn-grouped body, measured telemetry, author, capabilities)
	// are built once over the whole session, not re-derived
	// per window, so a turn's telemetry and the session's capability cannot drift between
	// windows; buildTurnBlocks then slices the per-window range out of this same rows/groups
	// pair each iteration.
	rows := buildMeasuredTurnRows(baseReq.OrgID, baseReq.SessionID, sp)
	turnUsage := sp.usage.GetTurnUsage()
	sessionCapability := capabilityFor(sp.prompts)

	stitched := &ReadResult{}
	phaseOffset := int32(0)
	for windowIndex, window := range windows {
		windowReq := baseReq
		windowReq.ChunkIndex = int32(windowIndex)
		windowBlocks := buildTurnBlocks(groups, rows, turnUsage, prefixHumanTurnFilter, window.mainStart, window.lookEnd)
		windowReq.Content = renderWindowContent(stitched.Turns, windowBlocks, sessionCapability)

		windowRead, err := p.producer.AnalyzeRead(ctx, windowReq)
		if err != nil {
			// All-or-nothing: a mid-chain failure aborts the whole pass. Nothing is
			// committed, so the next tick re-runs the entire window chain.
			return nil, fmt.Errorf("analyze_read chunk %d of %d: %w", windowIndex, len(windows), err)
		}

		// The window's tagged positions are rendered-Sequence, local to windowBlocks — never
		// the raw window size (window.mainEnd - window.mainStart), which over-counts whenever
		// the machine-turn filter drops a main-range group. indexMap translates a rendered
		// position back to its raw whole-session ordinal; keptMainCount is how many of that
		// translation's entries are in the main range rather than the look-ahead.
		indexMap := sequenceToRawIndex(windowBlocks)
		keptMain := keptMainCount(windowBlocks, int32(window.mainEnd))
		keptPhases := stitchWindowPhases(stitched, windowRead.Phases, window, phaseOffset, indexMap, keptMain)
		stitchWindowTurns(stitched, windowRead.Turns, windowIndex, indexMap, keptMain, phaseOffset)
		phaseOffset += keptPhases

		// The final window carries the whole-session digest and lands the verdict.
		if windowIndex == len(windows)-1 {
			stitched.Verdict = windowRead.Verdict
			stitched.Usage = windowRead.Usage
			stitched.ScorerModel = windowRead.ScorerModel
		}
	}
	// work_type is the dominant theme over the whole stitched phase set, not just the
	// final window's phases.
	stitched.Verdict.WorkType = dominantThemeOf(stitched.Phases)
	return stitched, nil
}

// keptMainCount returns how many of a window's rendered (machine-turn-filtered) blocks fall
// in the main range [window.mainStart, mainEnd) rather than the look-ahead — the boundary
// stitchWindowTurns/stitchWindowPhases must judge R's rendered positions against, since the
// raw window size over-counts whenever the filter drops a main-range group
// . blocks is ordered by ascending raw Index
// (buildTurnBlocks' append order), so the kept main-range blocks are always a contiguous
// prefix and rendered position 0..keptMainCount-1 are exactly the main-range turns.
func keptMainCount(blocks []*turnBlock, mainEnd int32) int32 {
	var kept int32
	for _, block := range blocks {
		if block.Index >= mainEnd {
			break
		}
		kept++
	}
	return kept
}

// stitchWindowTurns appends a window's authoritative main-range turns to the stitched
// table, translating each turn's rendered position to the raw whole-session ordinal via
// indexMap and tagging its chunk index. Look-ahead turns (rendered position at or beyond
// keptMainCount — the window's KEPT main-range block count, not the raw window size, since
// the machine-turn filter can drop main-range groups) are dropped — the next window's main
// range owns them. A rendered position R could not have tagged (out of indexMap's bounds —
// a hallucinated index) is set to -1, mirroring the commit's existing "no measured row
// matched" drop for any other out-of-cut index.
func stitchWindowTurns(stitched *ReadResult, turns []*TurnJudgment, windowIndex int, indexMap []int32, keptMainCount, phaseOffset int32) {
	chunkIndex := int32(windowIndex)
	for _, turn := range turns {
		if turn.PromptIndex >= keptMainCount {
			continue
		}
		if raw, ok := rawIndexOf(turn.PromptIndex, indexMap); ok {
			turn.PromptIndex = raw
		} else {
			turn.PromptIndex = -1
		}
		if turn.PhaseOrdinal != nil {
			rebased := *turn.PhaseOrdinal + phaseOffset
			turn.PhaseOrdinal = &rebased
		}
		ci := chunkIndex
		turn.ChunkIndex = &ci
		stitched.Turns = append(stitched.Turns, turn)
		if turn.PromptIssues != nil {
			stitched.SecurityIssues = append(stitched.SecurityIssues, securityIssuesOf(turn.PromptIssues)...)
		}
	}
}

// stitchWindowPhases appends a window's main-range phases (those a main-range turn falls
// in — a contiguous prefix, since phases are ordered by start) to the stitched set,
// translating each phase's ordinal and range to the whole-session frame via indexMap and
// clamping a range that runs into the look-ahead back to the main range (window.mainEnd, a
// raw whole-session bound that filtering does not change) so windows never claim
// overlapping turns. keptMainCount is the window's KEPT main-range block count — see
// stitchWindowTurns. Returns the count of phases kept (the next window's phase-ordinal
// offset).
func stitchWindowPhases(stitched *ReadResult, phases []*PhaseJudgment, window readWindow, phaseOffset int32, indexMap []int32, keptMainCount int32) int32 {
	mainStart := int32(window.mainStart)
	mainEnd := int32(window.mainEnd)
	var kept int32
	for _, phase := range phases {
		if phase.RangeStart != nil && *phase.RangeStart >= keptMainCount {
			continue // a phase wholly inside the look-ahead — the next window owns it
		}
		ordinal := phaseOffset + kept
		phase.PhaseOrdinal = ordinal
		if phase.RangeStart != nil {
			// keptMainCount <= len(indexMap), and the guard above already bounds RangeStart
			// below it, so this only fails to resolve on a negative (hallucinated) start —
			// fall back to the window's own raw floor rather than storing an unmapped value.
			start := mainStart
			if raw, ok := rawIndexOf(*phase.RangeStart, indexMap); ok {
				start = raw
			}
			phase.RangeStart = &start
		}
		if phase.RangeEnd != nil {
			// Default to the main-range ceiling; use the resolved raw index instead when it
			// is valid and tighter, so a look-ahead-spanning phase still clamps to the main
			// range exactly as before, and an unresolvable end degrades to the same ceiling
			// rather than storing an unmapped rendered-sequence value.
			end := mainEnd - 1
			if raw, ok := rawIndexOf(*phase.RangeEnd, indexMap); ok && raw < end {
				end = raw
			}
			phase.RangeEnd = &end
		}
		stitched.Phases = append(stitched.Phases, phase)
		kept++
	}
	return kept
}

// planWindows partitions turnCount human turns into contiguous main ranges of windowSize,
// each carrying a lookahead-turn read-only look-ahead. Returns one window on a session that
// fits in a single main range.
func planWindows(turnCount, windowSize, lookahead int) []readWindow {
	if turnCount <= 0 {
		return nil
	}
	if windowSize <= 0 {
		windowSize = turnCount
	}
	if lookahead < 0 {
		lookahead = 0
	}
	var windows []readWindow
	for start := 0; start < turnCount; start += windowSize {
		mainEnd := start + windowSize
		if mainEnd > turnCount {
			mainEnd = turnCount
		}
		lookEnd := mainEnd + lookahead
		if lookEnd > turnCount {
			lookEnd = turnCount
		}
		windows = append(windows, readWindow{mainStart: start, mainEnd: mainEnd, lookEnd: lookEnd})
	}
	return windows
}

// splitHumanTurns groups the parsed prompts by human turn: each ROLE_USER prompt opens a
// group that runs until the next user prompt, so a group is a user prompt plus the
// assistant/tool prompts answering it. This is the same human-turn ordinal R tags as
// prompt_index and the spine indexes per-turn token costs by. Leading non-user prompts
// (before the first user prompt) attach to the first group.
func splitHumanTurns(prompts []*sessionintelligencepb.Prompt) [][]*sessionintelligencepb.Prompt {
	var groups [][]*sessionintelligencepb.Prompt
	// Non-user prompts before the FIRST user turn are buffered and folded into that first
	// turn's group — never a group of their own. A phantom leading group would shift every
	// group index by one, and the chunked-read stitch re-bases turns by group index while R
	// and perTurnTokenCosts index by user-turn ordinal, so the two frames would disagree by
	// one for every window after the first (a gapped/misaligned prompt_index set).
	var leading []*sessionintelligencepb.Prompt
	for _, prompt := range prompts {
		if prompt.GetRole() == sessionintelligencepb.Role_ROLE_USER {
			group := []*sessionintelligencepb.Prompt{prompt}
			if len(groups) == 0 && len(leading) > 0 {
				group = append(leading, group...)
				leading = nil
			}
			groups = append(groups, group)
			continue
		}
		if len(groups) == 0 {
			leading = append(leading, prompt)
			continue
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], prompt)
	}
	// A conversation with no user prompt at all: keep its prompts as one group so the content
	// still renders rather than being dropped (a degenerate case, but non-crashing).
	if len(groups) == 0 && len(leading) > 0 {
		groups = append(groups, leading)
	}
	return groups
}

// renderWindowContent builds one window's LLM input: a digest of the already-judged prior
// turns (context R may consult but must not re-tag), then the window's own turn-grouped
// body (main range plus read-only look-ahead, machine-turn-filtered, with measured
// telemetry) and the session's capabilities block. R tags
// the window turns from prompt_index 0 (blocks' Sequence numbering, reset per window); the
// caller translates those rendered positions back to the whole-session raw ordinal via
// keptMainCount and the blocks' own Sequence->Index map (sequenceToRawIndex), not the RAW
// group range, so a window whose machine-turn filter drops turns is judged against what was
// actually rendered.
func renderWindowContent(priorTurns []*TurnJudgment, blocks []*turnBlock, sessionCapability capability) string {
	var builder strings.Builder
	if len(priorTurns) > 0 {
		builder.WriteString("=== PRIOR TURNS DIGEST (already judged — context only, do not re-tag) ===\n")
		builder.WriteString(buildTurnDigest(priorTurns))
		builder.WriteString("=== CURRENT WINDOW (tag these; prompt_index starts at 0) ===\n")
	}
	for _, block := range blocks {
		renderTurnBlock(&builder, block)
	}
	renderCapabilities(&builder, sessionCapability)
	return builder.String()
}

// buildTurnDigest renders one compact line per prior turn — its whole-session index and the
// tags that carry the session's shape — so a later window judges consistently with the
// earlier ones without re-reading their transcript.
func buildTurnDigest(turns []*TurnJudgment) string {
	var builder strings.Builder
	for _, turn := range turns {
		fmt.Fprintf(&builder, "turn %d:", turn.PromptIndex)
		if turn.PromptType != nil {
			builder.WriteString(" type=" + shortEnum(turn.PromptType.String(), "PROMPT_TYPE_"))
		}
		if turn.PromptObjectiveMet != nil {
			builder.WriteString(" objective_met=" + shortEnum(turn.PromptObjectiveMet.String(), "PROMPT_OBJECTIVE_MET_"))
		}
		if turn.AgentHelpfulness != nil {
			builder.WriteString(" helpfulness=" + shortEnum(turn.AgentHelpfulness.String(), "AGENT_HELPFULNESS_"))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// shortEnum renders a proto enum constant as its lowercase vendor token (drops the prefix),
// so the digest reads in the same vocabulary the transcript and prompt use.
func shortEnum(name, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(name, prefix))
}

// securityIssuesOf lifts the security-layer prompt issues out of a turn's issue list — the
// three leak slugs the commit reconciles into security_flags — so the chunked stitch
// accumulates them exactly as the single-chunk parser does.
func securityIssuesOf(issues *sessionintelligencepb.PromptIssueList) []*sessionintelligencepb.PromptIssue {
	var security []*sessionintelligencepb.PromptIssue
	for _, issue := range issues.GetItems() {
		if issue.GetLayer() == "security" {
			security = append(security, issue)
		}
	}
	return security
}

// dominantThemeOf returns the theme of the phase spanning the most prompts across the whole
// stitched phase set (the materialized session work_type), or nil when there are no phases.
func dominantThemeOf(phases []*PhaseJudgment) *sessionintelligencepb.PhaseTheme {
	var best *sessionintelligencepb.PhaseTheme
	var bestSpan int32 = -1
	for _, phase := range phases {
		if phase.PhaseTheme == nil || phase.RangeStart == nil || phase.RangeEnd == nil {
			continue
		}
		span := *phase.RangeEnd - *phase.RangeStart
		if span > bestSpan {
			bestSpan = span
			best = phase.PhaseTheme
		}
	}
	return best
}
