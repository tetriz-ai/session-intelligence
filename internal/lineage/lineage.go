// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package lineage defines the session-lineage metadata contract shared across
// the session pipeline — the flat raw_records.metadata keys the desktop sets on
// a child conversation, and the values the processor reads to fold children onto
// their parent. One low-level package (importable by the serving-side writer, the
// the consumer and DAO, and the processor) keeps the writer and reader from
// drifting. The full structured lineage still rides the S3 payload
// (SessionMeta.lineage); this flat copy is the query/fold surface.
package lineage

// Metadata keys carried on a child conversation's raw_records.metadata. A child
// tags itself with its parent's native session id under MetaKeyParentRecordKey,
// which equals the parent raw_record's record_key — the fold join.
const (
	MetaKeyParentRecordKey   = "parent_record_key"
	MetaKeyRelationType      = "relation_type"
	MetaKeyLinkageConfidence = "linkage_confidence"
	MetaKeyOriginAgentType   = "origin_agent_type"
	MetaKeyPluginName        = "plugin_name"
	MetaKeySpawnDepth        = "spawn_depth"
	MetaKeyToolUseID         = "tool_use_id"

	// MetaKeyCostUsdMicros and MetaKeyTokens are the processor-authoritative fold
	// inputs the parent aggregation sums, distinct from the desktop-written keys
	// above. Both hold integer units (micro-USD / token count) so the SQL
	// aggregation stays exact, and both must reflect the deduped per-response
	// totals — the raw per-content-block sum over-counts a Claude Code child.
	MetaKeyCostUsdMicros = "cost_usd_micros"
	MetaKeyTokens        = "tokens"
)

// RelationType values — the ConversationType enum without its CONVERSATION_TYPE_
// prefix. Empty or PRIMARY is a top-level session; the rest are delegated
// children.
const (
	RelationTypePrimary  = "PRIMARY"
	RelationTypeSubagent = "SUBAGENT"
	RelationTypePlugin   = "PLUGIN"
	// RelationTypeHook is a non-interactive run invoked by automation (a hook
	// firing `codex exec`, a headless `claude -p`/SDK call), not a human. When the
	// hook-lineage sidecar resolves its spawning session it folds onto that parent
	// (STRONG, origin_agent_type "hook") exactly like a subagent; an unresolved
	// hook has no parent and is mark-and-skip only.
	RelationTypeHook = "HOOK"
)

// LinkageConfidence values. Only STRONG children are folded/attributed; weaker
// links are captured but not counted, so a heuristic mislink never inflates a
// parent's totals.
const (
	LinkageConfidenceStrong    = "STRONG"
	LinkageConfidenceHeuristic = "HEURISTIC"
	LinkageConfidenceNone      = "NONE"
)

// IsChild reports whether a relation_type denotes a unit that is captured but
// never analyzed as a standalone session — a delegated child (subagent, plugin)
// or a non-interactive automation run (hook). Empty = primary, so records
// uploaded before lineage capture are treated as primaries.
func IsChild(relationType string) bool {
	switch relationType {
	case RelationTypeSubagent, RelationTypePlugin, RelationTypeHook:
		return true
	default:
		return false
	}
}
