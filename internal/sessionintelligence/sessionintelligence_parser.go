// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionintelligence

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// parseFlexibleTimestamp parses an ISO-8601 timestamp, accepting both the
// fractional-second (RFC3339Nano) and whole-second (RFC3339) layouts. The
// retry is folded here so the first-layout parse error is carried into the
// returned error rather than silently overwritten by the fallback assignment.
func parseFlexibleTimestamp(ts string) (time.Time, error) {
	// time.RFC3339Nano accepts whole-second values too, so a single parse against
	// it covers both the fractional- and whole-second layouts; the returned error
	// is the authoritative cause when neither layout matches.
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", ts, err)
	}
	return t, nil
}

// ParseConversation unmarshals S3 conversation content (single JSON object)
// into a Conversation proto.
func ParseConversation(content []byte) (*sessionintelligencepb.Conversation, error) {
	var conv sessionintelligencepb.Conversation
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(content, &conv); err != nil {
		return nil, fmt.Errorf("unmarshal conversation: %w", err)
	}
	if filled, ambiguous := backfillPromptModels(&conv); filled > 0 {
		modelBackfilledTotal.WithLabelValues(conv.GetIdeType().String(), backfillOutcomeFilled).Inc()
	} else if ambiguous {
		modelBackfilledTotal.WithLabelValues(conv.GetIdeType().String(), backfillOutcomeAmbiguous).Inc()
	}
	return &conv, nil
}

// modelIsConversationWideIdeTypes are the IDEs whose extractor reports ONE model for
// the whole conversation, so an unnamed prompt in such a conversation was served by that
// model. The proto is explicit that this is per-IDE, not universal: PromptMetadata.model
// reads "Claude Code: per-message. Cursor: conversation-wide (same for all messages)".
// The set is therefore opt-in per IDE, mirroring how harnesside keeps CostBlindIdeTypes
// rather than assuming one rule covers every tool.
//
// Do NOT add an IDE here without evidence its extractor reports conversation-wide.
// Getting it wrong OVER-bills: a per-message IDE can serve a cheap auxiliary turn (a
// Claude Code sidechain, a router fallback) whose model the extractor did not record, and
// filling that turn from the session's expensive model prices it at the wrong rate. The
// ambiguity check does not protect against that, because the failure mode is the second
// model's name being ABSENT rather than present.
var modelIsConversationWideIdeTypes = []sessionintelligencepb.IdeType{
	sessionintelligencepb.IdeType_IDE_TYPE_CURSOR,
}

// maxBackfillModelLen bounds a model name we will propagate. Real vendor ids are far
// shorter; the cap stops a truncated or junk value on one prompt from being copied across
// the conversation and into the unknown-model metric's label, where it would never
// resolve and so never stop firing.
const maxBackfillModelLen = 128

// backfillPromptModels fills metadata.model on prompts that name no model, using the
// conversation's own model when the conversation names exactly one. It reports how many
// prompts it filled and whether it declined because the conversation named several
// models, so the caller can see how much attribution was inferred.
//
// It runs at the parse boundary rather than at each consumer because FIVE of them read
// the field independently — pricing, contextWindowForModel, the Cursor cache-continuity
// check, the request-tier carry-forward that compares a prompt's model against the
// pending prompt's to decide tier inheritance, and modelsUsed — and this package has
// already been bitten by a normalization that had to be reapplied per consumer.
// Normalizing once here means no consumer can be added that forgets it.
//
// Every model-less prompt is filled, not only the token-bearing ones: a zero-token prompt
// carries no price, so nothing can be mis-attributed, and leaving it empty would break
// the request-tier carry-forward, whose guard keys on model equality against the pending
// prompt and would stop consuming a pending tier that then leaks to a later request.
//
// DECISION OWNERSHIP: "attribute an unnamed prompt
// to the conversation's sole model" is OUR uniform pricing policy — not a customer
// setting and not a domain fact. It trades a known-wrong default rate for an
// inferred-but-sourced one, scoped by the per-IDE contract above rather than applied
// globally, and it declines on ambiguity because an inferred price that is silently wrong
// is worse than a default price a counter reports.
//
// Deliberately NOT sourced from SessionMeta.default_model: that is the model selected in
// the IDE's chat panel at EXTRACTION time, not the one that served any historical turn,
// so pricing past tokens from it would invent attribution.
func backfillPromptModels(conv *sessionintelligencepb.Conversation) (filled int, ambiguous bool) {
	if !modelIsConversationWide(conv.GetIdeType()) {
		return 0, false
	}
	model, ok := soleConversationModel(conv)
	if !ok {
		// Naming NO model is not ambiguity — there is nothing to resolve from, and that
		// residue is what the unknown-model counter's ide_type label reports.
		return 0, conversationNamesSeveralModels(conv)
	}
	for _, p := range conv.GetPrompts() {
		m := p.GetMetadata()
		if m == nil || m.GetModel() != "" {
			continue
		}
		m.Model = model
		filled++
	}
	return filled, false
}

func modelIsConversationWide(ide sessionintelligencepb.IdeType) bool {
	for _, t := range modelIsConversationWideIdeTypes {
		if t == ide {
			return true
		}
	}
	return false
}

// soleConversationModel returns the one propagatable model named across conv's prompts,
// and false when it names none or several.
func soleConversationModel(conv *sessionintelligencepb.Conversation) (string, bool) {
	found, several := scanConversationModels(conv)
	if several {
		return "", false
	}
	return found, found != ""
}

// conversationNamesSeveralModels reports whether conv names two or more distinct
// propagatable models — i.e. whether soleConversationModel declined on ambiguity rather
// than on finding nothing.
func conversationNamesSeveralModels(conv *sessionintelligencepb.Conversation) bool {
	_, several := scanConversationModels(conv)
	return several
}

func scanConversationModels(conv *sessionintelligencepb.Conversation) (found string, several bool) {
	for _, p := range conv.GetPrompts() {
		m := propagatableModel(p.GetMetadata().GetModel())
		if m == "" {
			continue
		}
		if found != "" && m != found {
			return "", true
		}
		found = m
	}
	return found, false
}

// propagatableModel returns model if it is one we may copy onto another prompt, else "".
//
// Rejected: blank once trimmed; over maxBackfillModelLen; and any PRICING SENTINEL — a
// name that is a placeholder rather than a model. "<synthetic>" is Claude Code's marker
// for a locally-generated line, and a key whose modelPricingTable row is the zero value
// (cursor-default today) stands for "a subscription covers this", which
// resolveModelPricing reports as KNOWN with $0 rates. Propagating either is the worst
// case for this function: the sentinel prices real tokens at zero AND drops them off
// unknownModelPricingTotal, so the misprice is silent. Testing the zero row rather than
// naming cursor-default means a future sentinel is covered without another edit here.
func propagatableModel(model string) string {
	m := strings.TrimSpace(model)
	if m == "" || isSyntheticModel(m) || len(m) > maxBackfillModelLen {
		return ""
	}
	if pr, _, known := lookupModelTable(modelPricingTable, m); known && pr == (modelPricing{}) {
		return ""
	}
	return m
}

// stripPathPrefix removes common absolute path prefixes from file paths,
// returning only the portion relative to a project root.
// e.g., "/Users/alice/code/myapp/src/handler.go" → "src/handler.go"
// If no known prefix is found, the path is returned as-is.
func stripPathPrefix(filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}

	// prefix → number of path segments to skip after the prefix.
	// /Users/<user>/<workspace>/<project>/ → skip 3 (user, workspace, project)
	// /home/<user>/<workspace>/<project>/ → skip 3
	// /root/<workspace>/<project>/ → skip 2 (no user segment)
	// C:\Users\<user>\<workspace>\<project>\ → skip 3
	type rule struct {
		prefix string
		skip   int
	}
	rules := []rule{
		{"/Users/", 3},
		{"/home/", 3},
		{"/root/", 2},
		{"C:\\Users\\", 3},
		{"C:/Users/", 3},
	}

	for _, r := range rules {
		if strings.HasPrefix(filePath, r.prefix) {
			rest := filePath[len(r.prefix):]
			slashCount := 0
			for i, c := range rest {
				if c == '/' || c == '\\' {
					slashCount++
					if slashCount == r.skip {
						return rest[i+1:]
					}
				}
			}
			// Path matched a known prefix but had fewer segments than expected
			// (e.g., "/Users/alice/file.go"). Return empty to avoid leaking
			// absolute paths containing user home directories into files_edited.
			return ""
		}
	}

	return filePath
}

// ExtractSessionSignals maps session-level signals from SessionMeta and
// SessionFacets (populated by the desktop app) onto UsageMetadata fields
// for storage in session_insights JSONB.
func ExtractSessionSignals(conv *sessionintelligencepb.Conversation, um *sessionintelligencepb.UsageMetadata) {
	if conv == nil {
		return
	}

	if sm := conv.GetSessionMeta(); sm != nil {
		if langs := sm.GetLanguages(); len(langs) > 0 {
			um.Languages = languageNames(langs)
		}
		um.LinesAdded = sm.GetLinesAdded()
		um.LinesRemoved = sm.GetLinesRemoved()
		um.EstimatedLocChanged = sm.GetLinesAdded() + sm.GetLinesRemoved()
		um.GitCommits = sm.GetGitCommits()
		um.GitPushes = sm.GetGitPushes()
		um.UsesMcp = sm.GetUsesMcp()
		um.UsesWebSearch = sm.GetUsesWebSearch()
		um.UsesWebFetch = sm.GetUsesWebFetch()
		um.UsesTaskAgent = sm.GetUsesTaskAgent()
		um.UserInterruptions = sm.GetUserInterruptions()
		um.ToolCounts = sm.GetToolCounts()
		um.ToolErrors = sm.GetToolErrors()
		um.ToolErrorCategories = sm.GetToolErrorCategories()
		um.UserMessageCount = sm.GetUserMessageCount()
		um.AssistantMessageCount = sm.GetAssistantMessageCount()
		um.DurationMinutes = sm.GetDurationMinutes()
		um.FirstPrompt = sm.GetFirstPrompt()
		um.ProjectPath = sm.GetProjectPath()
		um.CompactionCount = sm.GetCompactionCount()
		um.ManualCompactionCount = sm.GetManualCompactionCount()
		um.AvgPreCompactionTokens = sm.GetAvgPreCompactionTokens()
		// Scalar pr_* fields are intentionally passed through during the
		// transition window: pre-upgrade readers of session_insights still
		// rely on them. Remove these three lines (and the deprecated proto
		// fields) once all readers have migrated to pr_references.
		um.PrUrl = sm.GetPrUrl()               //nolint:staticcheck // back-compat dual-write
		um.PrNumber = sm.GetPrNumber()         //nolint:staticcheck // back-compat dual-write
		um.PrRepository = sm.GetPrRepository() //nolint:staticcheck // back-compat dual-write
		um.PrReferences = sm.GetPrReferences()
		um.SessionTitle = sm.GetSessionTitle()
		um.GitBranch = sm.GetGitBranch() //nolint:staticcheck // back-compat dual-write
		um.GitBranches = sm.GetGitBranches()
		// Without this the matcher never sees a stamped branch: SessionMeta is
		// the upload contract and UsageMetadata is what gets stored and read, so
		// a field the bridge skips is invisible however faithfully the device
		// populates it.
		um.BranchSignals = sm.GetBranchSignals()
		// Scalar → plural backfill. An upload written before its extractor
		// emitted the repeated field carries a branch only in the deprecated
		// scalar, which leaves every plural-only reader (BQ analytics) blind for
		// that session. Reprocessing heals those rows without a re-upload. The
		// matcher already reads through its own scalar fallback, so this changes
		// what analytics sees, not what attribution matches.
		if len(um.GetGitBranches()) == 0 && um.GetGitBranch() != "" { //nolint:staticcheck // legacy upload fallback
			um.GitBranches = []string{um.GetGitBranch()} //nolint:staticcheck // legacy upload fallback
		}
		um.FilesModified = sm.GetFilesModified()
		um.TrackedFilePaths = sm.GetTrackedFilePaths()
		um.LastPrompt = sm.GetLastPrompt()
		um.BridgeSessionUrl = sm.GetBridgeSessionUrl()
		um.McpServerNames = sm.GetMcpServerNames()
		um.UserResponseTimes = sm.GetUserResponseTimes()
		um.UserMessageTimestamps = sm.GetUserMessageTimestamps()
		um.MessageHours = sm.GetMessageHours()
		um.SessionSource = sm.GetSource()
		// Per-session run mode. Sourced here (not from InstallationMetadata, whose
		// session_kind is deprecated and install-level) so an interactive and a
		// headless session from the same install classify differently.
		um.SessionKind = sm.GetSessionKind()
		um.GitSha = sm.GetGitSha()
		um.ReasoningEffort = sm.GetReasoningEffort()
		um.MemoryMode = sm.GetMemoryMode()
		um.ApprovalMode = sm.GetApprovalMode()
		um.SandboxPolicy = sm.GetSandboxPolicy()
		if edited := sm.GetFilesEdited(); len(edited) > 0 {
			um.FilesEdited = edited
		}
		um.UsesExtendedThinking = sm.GetUsesExtendedThinking()
		um.DynamicToolNames = sm.GetDynamicToolNames()
		um.SubAgentCount = sm.GetSubAgentCount()
		um.AgentTranscriptCount = sm.GetAgentTranscriptCount()
		um.ToolOutputCount = sm.GetToolOutputCount()
		um.TerminalSessionCount = sm.GetTerminalSessionCount()
		um.ProjectMcpServerNames = sm.GetProjectMcpServerNames()
		um.CursorLinesChanged = sm.GetCursorLinesChanged()
		um.CursorFilesModified = sm.GetCursorFilesModified()
		um.CursorContextUsagePercent = sm.GetCursorContextUsagePercent()
		um.ContextTokensUsed = sm.GetContextTokensUsed()
		um.ContextTokenLimit = sm.GetContextTokenLimit()
		um.ContextComposition = sm.GetContextComposition()

		um.AgentMode = sm.GetAgentMode()
		um.AutonomyMode = sm.GetAutonomyMode()
		um.IdeContextUsagePercent = sm.GetIdeContextUsagePercent()
		um.McpContextPercent = sm.GetMcpContextPercent()
		um.SteeringContextPercent = sm.GetSteeringContextPercent()
		um.SpecCount = sm.GetSpecCount()
		um.SteeringFileCount = sm.GetSteeringFileCount()
		um.HookCount = sm.GetHookCount()
		um.SpawnedAgentNames = sm.GetSpawnedAgentNames()
		um.SteeringInclusionCount = sm.GetSteeringInclusionCount()
		um.SteeringDocsUsed = sm.GetSteeringDocsUsed()
		um.ParentSessionId = sm.GetParentSessionId()
		um.QuotaInvocationsUsed = sm.GetQuotaInvocationsUsed()
		um.QuotaInvocationsLimit = sm.GetQuotaInvocationsLimit()
		um.TotalModelRequestCount = sm.GetTotalModelRequestCount()
		um.TotalAgentCycles = sm.GetTotalAgentCycles()
		um.BillingCreditsUsed = sm.GetBillingCreditsUsed()
		if paths := sm.GetSandboxAllowedReadPaths(); len(paths) > 0 {
			um.SandboxAllowedReadPaths = paths
		}
		if paths := sm.GetSandboxAllowedWritePaths(); len(paths) > 0 {
			um.SandboxAllowedWritePaths = paths
		}

		// Agentic session detail (SessionMeta 83-93). Written by Kiro today; the
		// fields are surface-neutral so another agentic extractor fills the same
		// ones. Straight passthrough — the extractor already omits anything it
		// did not observe, so an unset field here means "not reported", never
		// "measured zero".
		um.KiroId = sm.GetKiroId()
		um.ToolApprovalOutcomes = sm.GetToolApprovalOutcomes()
		um.ModelRequestAttempts = sm.GetModelRequestAttempts()
		um.AgentActiveSeconds = sm.GetAgentActiveSeconds()
		um.TurnUsage = sm.GetTurnUsage()
		um.SubAgentRuns = sm.GetSubAgentRuns()
		um.UserPromptChars = sm.GetUserPromptChars()
		um.AssistantResponseChars = sm.GetAssistantResponseChars()
		um.BuiltinToolUses = sm.GetBuiltinToolUses()
		um.ToolOriginCounts = sm.GetToolOriginCounts()
		um.AgenticSessionConfig = sm.GetAgenticSessionConfig()
	}

	if sf := conv.GetSessionFacets(); sf != nil {
		um.SessionOutcome = sf.GetOutcome()
		um.SessionType = sf.GetSessionType()
		um.BriefSummary = sf.GetBriefSummary()
		um.ClaudeHelpfulness = sf.GetClaudeHelpfulness()
		um.UnderlyingGoal = sf.GetUnderlyingGoal()
		um.GoalCategories = sf.GetGoalCategories()
		um.PrimarySuccess = sf.GetPrimarySuccess()
		um.FrictionCounts = sf.GetFrictionCounts()
		um.FrictionDetail = sf.GetFrictionDetail()
		um.UserSatisfactionCounts = sf.GetUserSatisfactionCounts()
	}

	// Enrich languages from the captured files, appending only the ones the
	// desktop app did not already report — never overriding a desktop-supplied
	// value (the desktop signal stays the source of truth). Runs last so it sees
	// the final FilesEdited (set above from sm.FilesEdited). This covers sessions
	// with no reported languages (CC/Codex/Copilot/Antigravity) AND older builds
	// that reported a partial set from a narrower language map, so an S3 reprocess
	// backfills the new basename/extension languages without waiting for every
	// client to re-upload. No-op for current builds (same files, same map).
	if files := um.GetFilesEdited(); len(files) > 0 {
		if derived := deriveLanguagesFromFiles(files); len(derived) > 0 {
			langs := um.GetLanguages()
			seen := make(map[string]struct{}, len(langs))
			for _, l := range langs {
				seen[l] = struct{}{}
			}
			for lang := range derived {
				if _, ok := seen[lang]; !ok {
					langs = append(langs, lang)
					seen[lang] = struct{}{}
				}
			}
			um.Languages = langs
		}
	}

	// Canonical order. FilesEdited arrives either from a map iteration
	// (ExtractUsageMetadata) or verbatim from the client, so its order is not stable
	// across extractions of the same conversation. The insight upsert's
	// re-attribution request comparison is order-sensitive, so an unstable order
	// raises a request on every re-upsert of a session that has not changed —
	// re-running attribution for no new information. Sorting also keeps
	// usage_metadata byte-stable, so an unchanged reprocess stops rewriting the
	// TOASTed blob and recomputing its generated columns. Order carries no meaning:
	// every reader treats this as a set.
	sort.Strings(um.GetFilesEdited())
}

// ConversationToText flattens parsed prompts into a single text string
// suitable for sending to the LLM for analysis. Each prompt is formatted
// as "[timestamp] role: content" when a timestamp is present, or
// "role: content" when it is absent, separated by newlines.
func ConversationToText(prompts []*sessionintelligencepb.Prompt) string {
	var b strings.Builder
	for _, p := range prompts {
		role := strings.TrimPrefix(strings.ToLower(p.GetRole().String()), "role_")
		if ts := p.GetTimestamp(); ts != nil && ts.IsValid() {
			b.WriteString("[")
			b.WriteString(ts.AsTime().UTC().Format(time.RFC3339))
			b.WriteString("] ")
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(p.GetContent())
		b.WriteString("\n\n")
	}
	return b.String()
}
