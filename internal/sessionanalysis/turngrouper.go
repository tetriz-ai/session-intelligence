// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/sessionintelligence"
)

// buildMeasuredTurnRows is the measured grouper: the spine's turn component. It cuts
// the parsed conversation into turns with the SAME split R indexes by
// (splitHumanTurns → one group per human prompt + its following assistant/tool
// prompts, group index == prompt_index), so the measured writer and the judged writer
// address the same (org, raw_record, prompt_index) key. Per turn it derives the
// code-owned measured columns from the per-message PromptMetadata the desktop extractor
// stamped: the token breakdown (deduped by message_id), the worst fidelity among
// token-bearing messages, the read/edited file set, gross line counts, native billing
// units, and the turn's peak context percentage. The model never re-derives these.
//
// Event-derived columns (the tool family — call/error counts + the per-invocation tools
// list, api/tool-retry, compaction position, the precise event-authorship stamp) are layered
// on by the event pass below (applyEventDerivedTurnColumns); the
// sub-agent child parent-locator is stamped by the child grouper. This function owns the
// per-message-metadata half and then folds the event timeline over it.
//
// Rows are cut for EVERY source: this is the shared content builder R/E/D read (spine,
// chunk-window, and round-trip-pair paths all feed these rows to buildTurnBlocks, which
// requires them index-aligned with the group list), and R judges a session regardless of
// its turn-grouping support. The supportsTurnGrouping gate is a DB-WRITE concern only —
// writeMeasuredTurns applies it before persisting turn rows, so an unsupported source
// still renders turn content but writes no measured turn grain.
func buildMeasuredTurnRows(orgID, rawRecordID string, sp *spineResult) []*model.SessionTurnAnalysis {
	rows := buildMeasuredTurnRowsWithChild(orgID, rawRecordID, sp, nil)
	// Second fold: the event-derived pass. It runs on the PARENT grouper only — a delegated
	// child's rows (buildMeasuredTurnRowsWithChild called directly by writeChildMeasuredTurns)
	// carry the forced delegated-agent identity and no timeline of their own, so the event
	// columns stay NULL there by design.
	applyEventDerivedTurnColumns(rows, sp)
	return rows
}

// childTurnLocator carries the parent linkage stamped on a delegated child's measured
// turn rows: the parent conversation's record_key and the tool-use id that dispatched
// this child. parent_prompt_index is left NULL here — only the parent knows which of its
// human turns spawned the dispatch, and it resolves it later via
// BackfillChildParentPromptIndex.
type childTurnLocator struct {
	parentConversationID string
	toolUseID            string
}

// buildMeasuredTurnRowsWithChild builds the measured turn rows and, when child is
// non-nil, stamps the parent locator and forces the delegated-agent identity on every
// turn — a delegated child's turns are all the agent's work, so authorship is the
// conversation's kind, not per-message inference.
func buildMeasuredTurnRowsWithChild(orgID, rawRecordID string, sp *spineResult, child *childTurnLocator) []*model.SessionTurnAnalysis {
	groups := splitHumanTurns(sp.prompts)
	rows := make([]*model.SessionTurnAnalysis, 0, len(groups))
	for i, group := range groups {
		promptIndex := int32(i) //nolint:gosec // group count is bounded by the parsed conversation length
		row := &model.SessionTurnAnalysis{
			OrganizationID:        orgID,
			RawRecordID:           rawRecordID,
			ConversationStartedAt: sp.conversationStartedAt,
			PromptIndex:           promptIndex,
			SourceContentSha256:   &sp.contentSha256,
		}
		applyMeasuredTurnColumns(row, group, sp)
		if child != nil {
			row.ParentConversationID = nilIfEmpty(child.parentConversationID)
			row.ParentToolUseID = nilIfEmpty(child.toolUseID)
			// parent_prompt_index stays nil — resolved by the parent's backfill.
			forceDelegatedAgentIdentity(row)
		}
		rows = append(rows, row)
	}
	return rows
}

// forceDelegatedAgentIdentity overrides a child turn's identity to the delegated agent.
// The per-message is_subagent inference in applyMeasuredTurnColumns can miss a turn whose
// messages carry no sub-agent flag, but the whole child conversation is delegated work,
// so every turn is the agent's.
func forceDelegatedAgentIdentity(row *model.SessionTurnAnalysis) {
	isSubagent := true
	row.IsSubagent = &isSubagent
	author := "SESSION_EVENT_AUTHOR_DELEGATED_AGENT"
	row.Author = &author
	origination := sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_AGENT_DRIVEN
	row.PromptOrigination = &origination
}

// turnGroupingSupportedIDETypes is the ide_type set the measured grouper cuts turns for — the
// single source both supportsTurnGrouping and the coverage-denominator SQL (via
// turnGroupingSupportedIDETypeSQLList) derive from, so a source added here can't silently leave
// a coverage denominator stale. The denominators scope by ide_type only, so Kiro is a member
// here and refined by session_source in supportsTurnGrouping (its Crew surface excluded).
var turnGroupingSupportedIDETypes = []sessionintelligencepb.IdeType{
	sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
	sessionintelligencepb.IdeType_IDE_TYPE_CURSOR,
	sessionintelligencepb.IdeType_IDE_TYPE_CODEX,
	sessionintelligencepb.IdeType_IDE_TYPE_KIRO,
}

// supportsTurnGrouping reports whether the measured grouper has defined turn-cut semantics for
// a source. A source is admitted only if its ide_type is in turnGroupingSupportedIDETypes;
// Kiro's autonomous Crew surface shares IDE_TYPE_KIRO and is told apart only by sessionSource,
// so it is excluded while interactive Kiro is admitted. This scopes only the turn grain —
// session-grain analytics still covers every source.
func supportsTurnGrouping(ide sessionintelligencepb.IdeType, sessionSource string) bool {
	if ide == sessionintelligencepb.IdeType_IDE_TYPE_KIRO && sessionSource == sessionintelligence.SessionSourceKiroCrew {
		return false
	}
	return slices.Contains(turnGroupingSupportedIDETypes, ide)
}

// shouldBuildMeasuredTurns reports whether this commit produces a fresh measured turn set: a
// non-spine-only pass on a source the grouper supports. It is the single predicate behind both
// building the rows (buildSessionAnalysis's turn-author rollups) and persisting them
// (writeMeasuredTurns) — a spine-only reprocess carries the prior turns untouched, and an
// unsupported source owns an empty turn set — so the two sites cannot disagree on whether turns
// exist this pass.
func shouldBuildMeasuredTurns(sp *spineResult) bool {
	return !sp.spineOnly && supportsTurnGrouping(sp.ideType, sp.usage.GetSessionSource())
}

// turnGroupingSupportedIDETypeSQLList renders turnGroupingSupportedIDETypes as a quoted,
// comma-separated SQL literal for an IN (...) clause. session_analyses.ide_type is stored as
// the proto enum name, so a coverage denominator built from this list cannot drift from
// supportsTurnGrouping's ide_type membership.
func turnGroupingSupportedIDETypeSQLList() string {
	quoted := make([]string, len(turnGroupingSupportedIDETypes))
	for i, ide := range turnGroupingSupportedIDETypes {
		quoted[i] = "'" + ide.String() + "'"
	}
	return strings.Join(quoted, ", ")
}

// applyMeasuredTurnColumns folds one turn's per-message metadata onto the row. Every
// column is set present-aware — a real zero (a measured turn that added no lines) stays
// distinct from unset (no source reported it), because a plain proto3 scalar cannot
// carry that and the turn table's presence contract is the point.
func applyMeasuredTurnColumns(row *model.SessionTurnAnalysis, group []*sessionintelligencepb.Prompt, sp *spineResult) {
	bd := &sessionintelligencepb.AnalysisTokenBreakdown{}
	seenMessageIDs := map[string]bool{}
	editedFiles := map[string]bool{}
	readFiles := map[string]bool{}
	// Models the turn ran on, collected in first-seen order (deterministic given the
	// turn's message order) and deduped so a turn served by one model carries one entry.
	// Effort is left empty — per-message metadata carries the model but not its effort.
	var models []*sessionintelligencepb.TurnModel
	seenModels := map[string]bool{}
	var linesAdded, linesRemoved int32
	var haveLinesAdded, haveLinesRemoved bool
	var nativeBilling float64
	var haveNative bool
	var contextPct float64
	var haveContext bool
	// context_tokens is the turn's PEAK raw context footprint: the largest single-message
	// effective-context total (input + cache_read + cache_creation — the same numerator
	// computeContextUsagePercent divides by the model window). NULL when no message carries token
	// counts (a vendor-percent-only provider such as Kiro reports occupancy but no token counts, so
	// the raw footprint is not computable). Unclamped — the raw count, which for a heavily-cached CC
	// turn legitimately exceeds the window (that saturation is what context_pct clamps, not this).
	var peakContextTokens int64
	var haveContextTokens bool
	// startedAt/endedAt bracket the turn: the earliest and latest message timestamp in the
	// group. Prompt.timestamp is optional (not every surface stamps one), so a turn whose
	// messages carry none leaves both NULL rather than inventing a bound.
	var startedAt, endedAt time.Time
	var haveTime bool
	// Claude Code reports per-message token counts but no vendor context indicator, so its
	// turn context% is derived below from our own model→window table. Tracked separately from
	// contextPct so a vendor-supplied percent (Cursor/Kiro) always wins over the derived one.
	isClaudeCode := sp.ideType == sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE
	var derivedContextPct float64
	var haveDerivedContext bool
	var isSubagent bool
	worstFidelity := sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED
	var haveFidelity bool
	var anyTokenBearing bool

	for _, p := range group {
		// Turn span: min/max over the group's message timestamps. Timestamp is a top-level Prompt
		// field, independent of Metadata, so this is folded BEFORE the md == nil guard below — a
		// message that carries a timestamp but no metadata must still bound the turn.
		if ts := p.GetTimestamp(); ts != nil && ts.IsValid() {
			t := ts.AsTime()
			if !haveTime {
				startedAt, endedAt, haveTime = t, t, true
			} else {
				if t.Before(startedAt) {
					startedAt = t
				}
				if t.After(endedAt) {
					endedAt = t
				}
			}
		}

		md := p.GetMetadata()
		if md == nil {
			continue
		}
		if md.GetIsSubAgent() {
			isSubagent = true
		}
		if m := md.GetModel(); m != "" && !seenModels[m] {
			seenModels[m] = true
			models = append(models, &sessionintelligencepb.TurnModel{Model: m})
		}

		// Token breakdown: sum across the turn's DISTINCT messages, deduping a repeated
		// message_id (a repeated-usage agent re-reports the same billed response — the
		// same keep-first dedup the session-grain path uses).
		// Peak context footprint: the largest single-message effective-context total across the
		// turn. Tracked per message (not the deduped sum) because it is the occupancy AT the
		// fullest message; a repeated message_id carries the identical effective total, so dedup
		// never moves the max. Independent of the token-bearing branch below (which sums the turn's
		// billed tokens) — this is a per-message peak, not a total.
		if effective := md.GetInputTokens() + md.GetCacheReadInputTokens() + md.GetCacheCreationInputTokens(); effective > 0 {
			if !haveContextTokens || effective > peakContextTokens {
				peakContextTokens = effective
				haveContextTokens = true
			}
		}

		if md.GetInputTokens() > 0 || md.GetOutputTokens() > 0 || md.GetCacheReadInputTokens() > 0 || md.GetCacheCreationInputTokens() > 0 {
			anyTokenBearing = true
			msgID := md.GetMessageId()
			if msgID == "" || !seenMessageIDs[msgID] {
				if msgID != "" {
					seenMessageIDs[msgID] = true
				}
				bd.InputTokens += md.GetInputTokens()
				bd.OutputTokens += md.GetOutputTokens()
				bd.CacheReadTokens += md.GetCacheReadInputTokens()
				bd.CacheWriteTokens += md.GetCacheCreationInputTokens()
				bd.CacheCreation_5MInputTokens += md.GetCacheCreation_5MInputTokens()
				bd.CacheCreation_1HInputTokens += md.GetCacheCreation_1HInputTokens()
				// reasoning_output_tokens is the "of which" reasoning bucket of output_tokens,
				// not additive to it: output_tokens above already counts these, so this only
				// records the split and must not be summed into the turn's total.
				bd.ReasoningOutputTokens += md.GetReasoningTokens()

				// Claude Code carries no vendor context indicator, so derive this message's
				// context occupancy from the authoritative model→window table (the formula and
				// its clamp/known-model rule live in session next to the table). The turn's
				// value is the PEAK across its distinct messages, matching the vendor-supplied
				// path below; a message on an unknown model contributes nothing (honest-unset).
				if isClaudeCode {
					if pct, ok := sessionintelligence.ContextOccupancyPercentForMessage(md); ok {
						if !haveDerivedContext || pct > derivedContextPct {
							derivedContextPct = pct
							haveDerivedContext = true
						}
					}
				}
			}
			// Fidelity resolves to the WORST (least trustworthy) tier any token-bearing
			// message REPORTS — a turn is only as measured as its shakiest count. A message
			// with no fidelity reported (UNSPECIFIED) does not drag the turn; if none report
			// one, the turn's fidelity stays unset.
			if f := md.GetTokenFidelity(); f != sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_UNSPECIFIED {
				if !haveFidelity || worseFidelity(f, worstFidelity) {
					worstFidelity = f
					haveFidelity = true
				}
			}
		}

		for _, f := range md.GetFilesEdited() {
			editedFiles[f] = true
		}
		for _, f := range md.GetRelevantFiles() {
			readFiles[f] = true
		}
		if md.LinesAdded != nil {
			linesAdded += md.GetLinesAdded()
			haveLinesAdded = true
		}
		if md.LinesRemoved != nil {
			linesRemoved += md.GetLinesRemoved()
			haveLinesRemoved = true
		}
		if md.NativeBillingUnits != nil {
			nativeBilling += md.GetNativeBillingUnits()
			haveNative = true
		}
		// context_usage_percent is 0.0–1.0 on the wire; the column is 0–100. The turn's
		// context is its PEAK (the fullest the window got during the turn).
		if md.ContextUsagePercent != nil {
			pct := md.GetContextUsagePercent() * 100
			if !haveContext || pct > contextPct {
				contextPct = pct
				haveContext = true
			}
		}
	}

	// Identity: authorship approximated from the sub-agent flag (a delegated turn is the
	// agent's, not a person's); the precise event-authorship stamp is layered by the event
	// pass. is_subagent is always set — present-including-false.
	row.IsSubagent = &isSubagent
	author := "SESSION_EVENT_AUTHOR_HUMAN"
	origination := sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_HUMAN
	if isSubagent {
		author = "SESSION_EVENT_AUTHOR_DELEGATED_AGENT"
		origination = sessionintelligencepb.PromptOrigination_PROMPT_ORIGINATION_AGENT_DRIVEN
	}
	row.Author = &author
	row.PromptOrigination = &origination

	if len(models) > 0 {
		row.Models = &sessionintelligencepb.TurnModelList{Items: models}
	}
	if anyTokenBearing {
		row.TokenBreakdown = bd
	}
	// token_fidelity is ALWAYS written so the column can be NOT NULL (nullability
	// enforcement): the worst reported tier for a turn whose token-bearing messages report
	// one, else UNSPECIFIED — no determinable fidelity, which is a no-token turn or one
	// whose messages report none. UNSPECIFIED is the correct value, not NULL.
	fidelity := sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_UNSPECIFIED
	if haveFidelity {
		fidelity = worstFidelity
	}
	row.TokenFidelity = &fidelity
	if ftl := buildFileTouchList(editedFiles, readFiles); len(ftl.GetItems()) > 0 {
		row.FilesTouched = ftl
	}
	if haveLinesAdded {
		row.LinesAdded = &linesAdded
	}
	if haveLinesRemoved {
		row.LinesRemoved = &linesRemoved
	}
	if haveNative {
		row.NativeBillingUnits = &nativeBilling
	}
	// A vendor-supplied per-message context% (Cursor/Kiro, reflecting the client's own
	// live context indicator) is authoritative; fall back to the model-window derivation
	// only for a source that reports tokens but no percent (Claude Code).
	if !haveContext && haveDerivedContext {
		contextPct = derivedContextPct
		haveContext = true
	}
	if haveContext {
		row.ContextPct = &contextPct
	}
	if haveContextTokens {
		row.ContextTokens = &peakContextTokens
	}
	if haveTime {
		row.StartedAt = &startedAt
		row.EndedAt = &endedAt
	}
	if sp.repoName != "" {
		repo := sp.repoName
		row.RepoFullName = &repo
	}
	if s := normalizeSurface(sp.usage.GetSessionSource()); s != nil {
		row.Surface = s
	}
}

// normalizeSurface maps a session's raw source value (UsageMetadata.session_source, which each
// provider's extractor stamps from where it found the session) onto the normalized run surface.
// Empty or an unrecognized value → nil (NULL) — an unknown surface is left NULL, never guessed.
// The raw vocabulary spans providers: Codex stamps cli/vscode/exec/subagent; Kiro stamps
// kiro-ide/kiro-cli/kiro and kiro-crew; Cursor stamps cli (CLI) and ide (IDE); Claude Code stamps
// cowork and its per-session launch entrypoint cli/claude-vscode/claude-desktop/sdk-* (sdk-cli,
// sdk-py, …). Crew DOES reach here: the R/E/D read paths stamp every source's turn rows via
// applyMeasuredTurnColumns, so the explicit kiro-crew case returns nil to leave its surface NULL
// rather than let it fall into the unrecognized default. No Crew turn or phase row ever persists,
// but that is the supportsTurnGrouping write-gate's doing (writeMeasuredTurns/writeTurnPhase in
// commit.go), not this read path — supportsTurnGrouping is a DB-write concern only.
func normalizeSurface(raw string) *sessionintelligencepb.SessionSurface {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return nil
	}
	var s sessionintelligencepb.SessionSurface
	switch v {
	case "cli", "kiro-cli":
		s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_CLI
	case "ide", "vscode", "kiro-ide", "claude-vscode":
		s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_IDE
	case "kiro", "claude-desktop":
		s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_DESKTOP
	case "exec":
		s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_SDK
	case "cowork":
		s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_COWORK
	case "subagent":
		// a delegation-origin surface, kept as its own value (not folded into another)
		s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_SUBAGENT
	case "kiro-crew":
		return nil // Kiro Crew has no turn grain (grouping-excluded); no turn row ever carries it
	default:
		if strings.HasPrefix(v, "sdk-") { // sdk-cli / sdk-py / … — Claude Code headless (Agent SDK)
			s = sessionintelligencepb.SessionSurface_SESSION_SURFACE_SDK
			break
		}
		return nil // unrecognized → NULL, never invented
	}
	return &s
}

// worseFidelity reports whether candidate is a LESS trustworthy tier than current.
// Ordering (best→worst): MEASURED < NATIVE_PARTIAL < ESTIMATE < UNSPECIFIED — a turn's
// fidelity is the worst any of its token-bearing messages carries.
func worseFidelity(candidate, current sessionintelligencepb.TokenFidelity) bool {
	return fidelityRank(candidate) > fidelityRank(current)
}

func fidelityRank(f sessionintelligencepb.TokenFidelity) int {
	switch f {
	case sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_MEASURED:
		return 0
	case sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_NATIVE_PARTIAL:
		return 1
	case sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_ESTIMATE:
		return 2
	default: // UNSPECIFIED — no fidelity reported is the least trustworthy
		return 3
	}
}

// buildFileTouchList unions a turn's edited and read files into the files_touched
// payload. A file both edited and read in the turn is EDITED (the stronger touch); a
// read-only file is READ.
func buildFileTouchList(edited, read map[string]bool) *sessionintelligencepb.FileTouchList {
	items := make([]*sessionintelligencepb.FileTouch, 0, len(edited)+len(read))
	// Sort each set so the payload order is deterministic across runs of the same input:
	// a Go map iterates in randomized order, which would make files_touched (and the byte
	// goldens over it) flaky. Edited paths come first (the stronger touch), then read-only.
	editedPaths := sortedKeys(edited)
	for _, path := range editedPaths {
		items = append(items, &sessionintelligencepb.FileTouch{Path: path, TouchKind: sessionintelligencepb.TouchKind_TOUCH_KIND_EDITED})
	}
	for _, path := range sortedKeys(read) {
		if edited[path] {
			continue
		}
		items = append(items, &sessionintelligencepb.FileTouch{Path: path, TouchKind: sessionintelligencepb.TouchKind_TOUCH_KIND_READ})
	}
	return &sessionintelligencepb.FileTouchList{Items: items}
}

// sortedKeys returns a set's keys in ascending order for deterministic iteration.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
