// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// The R-owned column allowlist — the SINGLE documented source of truth for which
// session_analyses / session_turn_analyses / session_phase_analyses columns carry R's
// judged output, and therefore which columns rehydrateReadResult reads to carry R by
// rehydration. rehydrate.go's reader and these
// lists must stay in lock-step; the round-trip property test
// (TestRehydrateReadResult_RoundTripThroughDAO) proves the reader actually reproduces
// exactly this set, and TestRehydrateColumnSetParity below proves every model column is
// classified — so a newly-added R column cannot silently escape the guard (it lands
// unclassified and fails the parity test until a human files it here and in the reader).
var (
	rOwnedSessionColumns = []string{
		"session_steering", "goal_achieved", "smoothness", "friction_cause", "speed",
		"token_efficiency", "goal_is_coding_work", "underlying_goal", "work_type",
		"score_overall", "score_clarity", "score_specificity", "score_context_provision",
		"score_actionability", "score_completeness", "score_efficiency",
		"invoked_skills", "harness_findings",
		// R's flat security readout — rehydrated as the fallback recovery when a source's turn
		// grain is not stored (no turn rows to flatten from), so R's flags survive a re-pick.
		"read_security_issues",
	}
	rOwnedTurnColumns = []string{
		"prompt_type", "user_steering", "prompt_completeness", "prompt_objective_met",
		"agent_helpfulness", "agent_initiative", "tool_use", "was_retry", "failure_class",
		"token_locus", "waste_cause", "decision_type", "downstream_impact",
		"feedback_target", "feedback_disposition", "referenced_artifact",
		"referenced_finding_ids", "evidence_seq", "prompt_issues", "chunk_index",
		// phase_id is written by buildTurnRow (resolved from the phase ordinal) and read
		// back by the mapper as PhaseOrdinal via the phase set — R-owned linkage.
		"phase_id",
	}
	rOwnedPhaseColumns = []string{
		"phase_ordinal", "range_start", "range_end", "boundary_overridden", "override_reason",
		"phase_theme", "sdlc_stage_set", "task_complexity", "user_steering", "phase_objective",
		"phase_objective_met", "agent_helpfulness", "smoothness", "friction_cause", "speed",
		"went_well", "went_wrong", "coaching", "turn_count",
	}

	// The non-R columns, classified by owner so the parity test's "every column is
	// accounted for" invariant holds. A column here is deliberately NOT rehydrated:
	//   E/D  — written by E or D, re-produced on the re-pick's fresh E/D run, never carried.
	//   measured — code-owned static/telemetry columns (applyMeasuredColumns, the grouper).
	//   infra — identity, lifecycle, generation, and the content-digest columns.
	excludedSessionColumns = []string{
		// E/D-owned judged output — re-run on re-pick, never carried as R.
		"artifact_adjudications", "skill_assessments", "discarded_ranges", "wasted_tokens",
		"discard_disposition", "evidence_artifact_disposition", "evidence_roundtrip_disposition",
		// Measured / code-owned (applyMeasuredColumns) and deferred/unwritten telemetry.
		"ide_type", "weighted_score", "estimated_cost_usd", "own_estimated_cost_usd",
		"total_prompts", "total_tokens", "token_breakdown", "subagent_usage",
		"iteration_count", "effective_active_duration_s", "effective_ai_code_percent",
		"effective_context_usage_percent", "mcp_server_count", "delegation_level",
		"effectiveness_source", "is_session_effective", "session_outcome", "user_interruptions",
		"session_duration_seconds", "cache_hit_rate_percent", "compaction_count",
		"files_edited_count", "project_name", "repo_name", "models_used", "turn_count",
		"human_turn_count", "delegated_turn_count", "phase_count", "tool_call_count",
		"active_share", "segmentation_suspect", "languages", "friction_counts",
		"signal_coverage", "session_meta_raw", "last_active_at", "first_user_msg_ts",
		"cap_token_split", "cap_tool_outcome", "cap_reasoning_trace", "cap_assistant_text",
		"plan_tier", "plan_tier_source", "plan_tier_origin", "seat_monthly_price_usd",
		"has_extra_usage_enabled", "account_uuid", "login_email",
		// Postgres STORED generated columns hoisted from session_meta_raw for serving reads
		// (session-analyses-serving-parity design): computed, never producer-written, not rehydrated.
		"mcp_server_names", "tool_errors", "session_type", "cost_is_estimated",
		"cost_is_input_only", "user_message_timestamps",
		// Serving-parity analysis columns (session-analyses-serving-parity design): taxonomy_issues/
		// waste_categories are rolled up from R's turns and recomputed on re-pick (never rehydrated);
		// the rest are copy/writer-populated, not R input.
		"taxonomy_issues", "waste_categories", "ai_reviewed", "improved_prompt",
		"recommendations", "task_type", "origin",
		// Infra: identity, lifecycle, generation.
		"id", "organization_id", "person_id", "raw_record_id", "conversation_id", "session_id",
		"conversation_started_at", "vocabulary_version", "created_at", "updated_at", "deleted_at",
		"judged_at", "judged_disposition", "superseded_at",
	}
	excludedTurnColumns = []string{
		// E-owned: P1 moved the per-pair fault set into session_round_trip_faults; these
		// turn columns are vestigial and never carried as R.
		"round_trip_fault", "round_trip_evidence_prompt_index",
		// Measured (the grouper) and structural.
		"prompt_origination", "author", "is_subagent", "models", "token_breakdown",
		"token_fidelity", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_tokens", "native_billing_units", "lines_added", "lines_removed",
		"tool_call_count", "tool_error_count", "tools", "tool_retry_count", "api_retry_count",
		"compaction_count", "files_touched", "surface", "started_at", "ended_at",
		"repo_full_name", "context_tokens", "context_pct", "subagent_count",
		"parent_conversation_id", "parent_prompt_index", "parent_tool_use_id",
		// Content digest — not an R judgment.
		"judged_content_sha256", "source_content_sha256",
		// Infra.
		"id", "organization_id", "raw_record_id", "session_analysis_id",
		"conversation_started_at", "prompt_index",
		"created_at", "updated_at", "deleted_at",
	}
	excludedPhaseColumns = []string{
		// Code-owned/measured, not R judgments. hitl_gate, tokens and tool_call_count ARE written by
		// buildPhaseRows but recomputed fresh from measured turn state each commit, so they are not
		// rehydrated from R output; segmentation_suspect is not written there at all.
		"hitl_gate", "tokens", "tool_call_count", "segmentation_suspect",
		// Infra.
		"id", "organization_id", "session_analysis_id", "conversation_started_at",
		"created_at", "updated_at", "deleted_at",
	}
)

// gormColumns returns every column name declared by the struct's gorm:"column:..." tags.
func gormColumns(t reflect.Type) map[string]struct{} {
	columns := make(map[string]struct{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("gorm")
		for _, part := range strings.Split(tag, ";") {
			if strings.HasPrefix(part, "column:") {
				columns[strings.TrimPrefix(part, "column:")] = struct{}{}
			}
		}
	}
	return columns
}

// TestRehydrateReadResult_SecurityRecovery pins that R's security readout must survive a re-pick
// whichever way the turn grain went. With turn rows present (a supported source), the flatten
// recovers it from the per-turn prompt_issues and the persisted column is ignored; with NO turn
// rows (a source whose turn grain is not stored — supportsTurnGrouping == false), the flatten would
// yield an empty set, so the fallback recovers it from read_security_issues instead. Without the
// fallback, the empty set would drive the last-writer-wins security reconcile to delete R's flags.
func TestRehydrateReadResult_SecurityRecovery(t *testing.T) {
	securityIssue := func(slug string) *sessionintelligencepb.PromptIssue {
		return &sessionintelligencepb.PromptIssue{Layer: "security", Note: slug}
	}
	tests := []struct {
		name      string
		analysis  *model.SessionAnalysis
		turns     []*model.SessionTurnAnalysis
		wantSlugs []string
	}{
		{
			name: "turn rows present: flatten from prompt_issues, ignore the column",
			analysis: &model.SessionAnalysis{
				ID: "a1", ReadSecurityIssues: &sessionintelligencepb.PromptIssueList{Items: []*sessionintelligencepb.PromptIssue{securityIssue("stale-from-column")}},
			},
			turns: []*model.SessionTurnAnalysis{{PromptIndex: 0, PromptIssues: &sessionintelligencepb.PromptIssueList{Items: []*sessionintelligencepb.PromptIssue{
				securityIssue("from-turn"),
				{Layer: "quality", Note: "not-security"}, // a non-security issue is not recovered
			}}}},
			wantSlugs: []string{"from-turn"},
		},
		{
			name: "no turn rows (unsupported source): recover from the persisted column",
			analysis: &model.SessionAnalysis{
				ID: "a1", ReadSecurityIssues: &sessionintelligencepb.PromptIssueList{Items: []*sessionintelligencepb.PromptIssue{securityIssue("from-column")}},
			},
			turns:     nil,
			wantSlugs: []string{"from-column"},
		},
		{
			name:      "no turn rows and no column: empty, not nil-crash",
			analysis:  &model.SessionAnalysis{ID: "a1"},
			turns:     nil,
			wantSlugs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rehydrateReadResult(tt.analysis, tt.turns, nil)
			require.NotNil(t, got)
			var gotSlugs []string
			for _, issue := range got.SecurityIssues {
				gotSlugs = append(gotSlugs, issue.GetNote())
			}
			require.Equal(t, tt.wantSlugs, gotSlugs)
		})
	}
}

// TestRehydrateColumnSetParity is the anti-drift guard the reprocessing slice is gated on
// : every column of each rehydrated content table is
// classified as either R-owned (carried by the rehydrate mapper) or explicitly excluded
// with a stated owner. A newly-added column lands in neither set and fails here, so an R
// column cannot be added to the commit writers and silently skipped by the rehydrate
// reader. It also proves the allowlist names are real columns (a rename breaks the test).
func TestRehydrateColumnSetParity(t *testing.T) {
	tests := []struct {
		name     string
		typ      reflect.Type
		rOwned   []string
		excluded []string
	}{
		{"session_analyses", reflect.TypeOf(model.SessionAnalysis{}), rOwnedSessionColumns, excludedSessionColumns},
		{"session_turn_analyses", reflect.TypeOf(model.SessionTurnAnalysis{}), rOwnedTurnColumns, excludedTurnColumns},
		{"session_phase_analyses", reflect.TypeOf(model.SessionPhaseAnalysis{}), rOwnedPhaseColumns, excludedPhaseColumns},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := gormColumns(tt.typ)
			classified := make(map[string]int, len(tt.rOwned)+len(tt.excluded))
			for _, c := range tt.rOwned {
				classified[c]++
			}
			for _, c := range tt.excluded {
				classified[c]++
			}
			// No column is in both sets, and every classified name is a real column.
			for column, count := range classified {
				if count > 1 {
					t.Errorf("%s: column %q is classified more than once (R-owned and excluded are disjoint)", tt.name, column)
				}
				if _, ok := actual[column]; !ok {
					t.Errorf("%s: classified column %q is not a real gorm column (renamed or removed?)", tt.name, column)
				}
			}
			// Every real column is classified — a new column must be filed as R-owned or excluded.
			for column := range actual {
				if _, ok := classified[column]; !ok {
					t.Errorf("%s: column %q is unclassified — add it to rOwned* (and the rehydrate reader) or excluded* with its owner", tt.name, column)
				}
			}
		})
	}
}
