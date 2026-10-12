// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/metricutil"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
)

// SessionAnalysisRepo implements ISessionAnalysisDAO for the seat-scoped
// session_analyses content table.
//
// session_analyses carries the data_window RLS policy, so writes go through
// planscoped.WriteDB (sets the data_window GUC for the RETURNING insert, no seat
// clause) and seat-scoped reads through planscoped.DB (seat IN-clause + cutoff).
//
// Surface: the seat-scoped write path (Create / UpsertMeasured / UpsertJudged /
// SupersedeContent / ClearServedVerdict, all through planscoped.WriteDB) plus the serving
// aggregate reads over session_analyses (overview / person-summary / work-type / weekly-trend /
// PR-card, through planscoped.DB) and the frozen-org purge.
type SessionAnalysisRepo struct {
	db *gorm.DB
}

// NewSessionAnalysisRepo creates a new SessionAnalysisRepo.
func NewSessionAnalysisRepo(db *gorm.DB) *SessionAnalysisRepo {
	return &SessionAnalysisRepo{db: db}
}

// Create inserts one session-analysis row.
func (r *SessionAnalysisRepo) Create(ctx context.Context, analysis *model.SessionAnalysis) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "Create", time.Now())
	if analysis == nil {
		return fmt.Errorf("creating session analysis: nil analysis")
	}
	if analysis.ID == "" {
		analysis.ID = uuid.NewString()
	}
	if err := planscoped.WriteDB(ctx, r.db).Create(analysis).Error; err != nil {
		return fmt.Errorf("creating session analysis (org %s, raw_record %s): %w", analysis.OrganizationID, analysis.RawRecordID, err)
	}
	return nil
}

// sessionAnalysisMeasuredColumns are the code-owned measured columns a spine-only
// (no-LLM) reprocess refreshes on conflict — everything the spine re-derives, and
// nothing else. It deliberately OMITS every judged column and judged_at /
// judged_disposition / vocabulary_version / superseded_at, so a measured-only
// correction never clobbers the prior generation's judged verdict with NULL
// .
var sessionAnalysisMeasuredColumns = []string{
	// person_id is re-stamped on a spine-only reprocess so an unchanged-payload re-upload that
	// corrected the owner re-attributes the content row (serving filters on it); a no-op when the
	// owner is unchanged. Not a judged column — it never clobbers a verdict.
	"person_id",
	"ide_type", "estimated_cost_usd", "own_estimated_cost_usd", "total_prompts",
	"total_tokens", "token_breakdown", "iteration_count", "effective_active_duration_s",
	"effective_ai_code_percent", "effective_context_usage_percent", "mcp_server_count",
	"is_session_effective", "user_interruptions", "session_duration_seconds",
	"cache_hit_rate_percent", "compaction_count", "files_edited_count", "project_name",
	"models_used", "delegation_level", "effectiveness_source", "session_outcome",
	"turn_count", "repo_name", "subagent_usage", "updated_at",
	// The verbatim vendor usage blob (the source the STORED gen-cols and the entrypoint index derive
	// from) plus the seat/plan identity, languages and friction counts — all re-derived by
	// applyMeasuredColumns from the enriched usage on every spine pass (session-analyses-serving-parity
	// design). The generated columns (session_type, cost_is_*, user_message_timestamps, mcp_server_names,
	// tool_errors) are NEVER listed here — Postgres rejects a write to a generated column.
	"session_meta_raw", "plan_tier", "plan_tier_source", "plan_tier_origin",
	"seat_monthly_price_usd", "has_extra_usage_enabled", "account_uuid", "login_email",
	"languages", "friction_counts",
	// Identity + session-blob measured scalars re-derived every spine pass: session_id, the
	// active-share ratio and the first user-message timestamp from the usage blob, conversation_id
	// from the parsed S3 key, and last_active_at from the raw record's fetched_at (the session
	// last_conversation_at equivalent — the desktop's last-sync time). Measured (never judged) — they
	// never clobber a verdict.
	"session_id", "conversation_id", "active_share", "first_user_msg_ts", "last_active_at",
}

// sessionAnalysisJudgedColumns are the judged R/E/D-VERDICT columns this pipeline writes and that
// a JUDGE_FAILED must NULL — the ones whose value is meaningless without a successful verdict. The
// full R input-quality score family (score_overall AND score_clarity/specificity/context_provision/
// actionability/completeness/efficiency) is written here from read.Verdict (buildSessionAnalysis),
// so all seven are judged columns; weighted_score (score_overall × tokens) and phase_count
// (len(R's phases)) are R-derived rollups that follow the same rule. MarkJudgeFailed nulls exactly
// this set alongside judged_at/judged_disposition: a content-change reopen's ClearServedVerdict
// clears only judged_at, so a row whose re-judge then fails would otherwise resurface its PRIOR
// generation's stale verdict under a JUDGE_FAILED disposition — this keeps the
// "JUDGE_FAILED's judged columns are NULL" invariant true regardless of what a prior successful
// judge left behind. The turn/prompt-derived measured aggregates the judge also writes
// (human_turn_count, …) are NOT here — they live in sessionAnalysisJudgeWrittenMeasuredColumns so a
// judge failure does not wipe still-valid measured data; the cap_* columns other than cap_token_split
// currently have no writer.
var sessionAnalysisJudgedColumns = []string{
	"session_steering", "goal_achieved", "smoothness", "friction_cause", "speed",
	"token_efficiency", "goal_is_coding_work", "underlying_goal", "work_type", "score_overall",
	"score_clarity", "score_specificity", "score_context_provision", "score_actionability",
	"score_completeness", "score_efficiency",
	"invoked_skills", "harness_findings", "read_security_issues",
	"artifact_adjudications", "skill_assessments",
	"wasted_tokens", "discarded_ranges",
	"discard_disposition",
	"evidence_artifact_disposition",
	"evidence_roundtrip_disposition",
	// R-owned new-vocabulary rollups (session-analyses-serving-parity design): derived from R's
	// per-turn judgments at commit time, so they join the judged set — a failed judge NULLs them,
	// and a spine-only reprocess (measured set only) leaves them untouched.
	"taxonomy_issues", "waste_categories",
	// phase_count and weighted_score — the R-verdict-derived rollups the doc above covers.
	"phase_count", "weighted_score",
}

// sessionAnalysisJudgeWrittenMeasuredColumns are measured aggregates the judge computes from the
// turn/prompt grain on a full judged commit (the turns are rebuilt then), but which do NOT depend
// on R's verdict succeeding. They are refreshed on a judged re-commit (so they join the judged
// upsert set) yet are deliberately NOT nulled by MarkJudgeFailed — the measured turn rows are
// unchanged on a judge failure, so wiping their session-level rollups would discard still-valid
// data. They are also absent from the spine-only reaffirm set, so a re-pick preserves them.
var sessionAnalysisJudgeWrittenMeasuredColumns = []string{
	"human_turn_count", "delegated_turn_count", "tool_call_count", "cap_token_split",
}

// sessionAnalysisJudgedUpsertColumns is the exact set a judged re-upsert refreshes on conflict:
// every column this pipeline owns (measured + judged verdict + judge-written measured aggregates)
// plus the judge's own lifecycle columns (judged_at, judged_disposition) and the generation keys it
// re-stamps (conversation_started_at, vocabulary_version). It DELIBERATELY excludes every
// session_analyses column owned by NO current writer (the cap_* family other than cap_token_split,
// segmentation_suspect, signal_coverage): a blind UpdateAll would clobber such a
// co-owner's value to NULL on every re-judge — the list-drift trap. Enforcing the set makes "a
// column this pipeline does not own is untouched by the judge" a real guarantee rather than an
// asserted one, so a future post-hoc writer survives a re-judge. Every column name in these
// sets is exercised against real Postgres by the round-trip DAO tests (CopyUpsert, UpsertJudged,
// UpsertMeasuredReaffirmingJudged, MarkJudgeFailed), so a mistyped or dropped column fails the SQL.
var sessionAnalysisJudgedUpsertColumns = func() []string {
	cols := append([]string{}, sessionAnalysisMeasuredColumns...)
	cols = append(cols, sessionAnalysisJudgedColumns...)
	cols = append(cols, sessionAnalysisJudgeWrittenMeasuredColumns...)
	return append(cols, "judged_at", "judged_disposition", "conversation_started_at", "vocabulary_version")
}()

// UpsertMeasured refreshes ONLY the measured columns of the live generation, for a
// spine-only reprocess (a spine-version bump whose producer-read inputs are unchanged,
// so no producer ran). Conflict-targeted on the live-generation partial unique, it
// DO-UPDATEs only sessionAnalysisMeasuredColumns, leaving the judged verdict, judged_at,
// and disposition intact — the fix for the measured-only path clobbering the judged
// output. On the (unexpected) no-prior-row case it inserts the full row. Runs inside
// the caller's commit transaction.
func (r *SessionAnalysisRepo) UpsertMeasured(ctx context.Context, analysis *model.SessionAnalysis) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "UpsertMeasured", time.Now())
	if analysis == nil {
		return fmt.Errorf("upserting measured session analysis: nil analysis")
	}
	if analysis.ID == "" {
		analysis.ID = uuid.NewString()
	}
	err := planscoped.WriteDB(ctx, r.db).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "superseded_at IS NULL AND deleted_at IS NULL"}}},
		DoUpdates:   clause.AssignmentColumns(sessionAnalysisMeasuredColumns),
	}, clause.Returning{Columns: []clause.Column{{Name: "id"}}}).Create(analysis).Error
	if err != nil {
		return fmt.Errorf("upserting measured session analysis (org %s, raw_record %s): %w", analysis.OrganizationID, analysis.RawRecordID, err)
	}
	return nil
}

// sessionAnalysisReaffirmColumns is the measured set PLUS judged_at, for a spine-only reprocess
// that RE-AFFIRMS a prior JUDGED generation on an unchanged body: it refreshes the measured
// columns AND restores judged_at, while still leaving the judged VERDICT columns untouched (the
// carried verdict is unchanged). A fresh slice, never append-mutating the measured var.
var sessionAnalysisReaffirmColumns = append(append([]string{}, sessionAnalysisMeasuredColumns...), "judged_at")

// UpsertMeasuredReaffirmingJudged is UpsertMeasured plus restoring judged_at. It is for the
// spine-only re-affirm path ONLY: spineOnlyReprocess has proved a prior JUDGED generation with
// settled producers and an unchanged body, so the carried verdict is still valid — but a cat-3
// legacy reopen (an idempotent connector re-emit that bumps raw_records.updated_at with no body
// change) may have cleared judged_at via ClearServedVerdict, and a plain UpsertMeasured would then
// leave the still-valid session permanently absent from serving (judged_at NULL). This restores it
// without touching the judged verdict columns. Runs inside the caller's commit transaction.
func (r *SessionAnalysisRepo) UpsertMeasuredReaffirmingJudged(ctx context.Context, analysis *model.SessionAnalysis) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "UpsertMeasuredReaffirmingJudged", time.Now())
	if analysis == nil {
		return fmt.Errorf("upserting measured session analysis (reaffirm): nil analysis")
	}
	if analysis.ID == "" {
		analysis.ID = uuid.NewString()
	}
	err := planscoped.WriteDB(ctx, r.db).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "superseded_at IS NULL AND deleted_at IS NULL"}}},
		DoUpdates:   clause.AssignmentColumns(sessionAnalysisReaffirmColumns),
	}, clause.Returning{Columns: []clause.Column{{Name: "id"}}}).Create(analysis).Error
	if err != nil {
		return fmt.Errorf("upserting measured session analysis reaffirming judged (org %s, raw_record %s): %w", analysis.OrganizationID, analysis.RawRecordID, err)
	}
	return nil
}

// UpsertJudged writes the session-grain content row for the judged commit,
// conflict-targeted on the session_analyses_one_live partial unique
// (organization_id, raw_record_id) WHERE superseded_at IS NULL AND deleted_at IS
// NULL — so a re-judge of the live generation updates in place while a superseded
// generation coexists. Writes through
// planscoped.WriteDB (sets the data_window GUC for the RETURNING upsert). The
// caller runs this inside the one tenant-scoped commit transaction alongside the
// state transition and the security-flag reconcile.
//
// On a re-judge conflict the DoUpdates set (sessionAnalysisJudgedUpsertColumns) refreshes
// every pipeline-owned column — including the two E-owned columns (skill_assessments,
// artifact_adjudications) — while leaving any different-writer column untouched. analysis
// carries E's fresh output only when E's grain is AUTHORITATIVE this pass (evidenceAuthoritative —
// E ran and produced output, OR E errored and its FAILED disposition is set); otherwise those
// fields are nil on analysis (buildSessionAnalysis leaves them unset), and a blind refresh would
// NULL out the prior generation's valid E output on an R-only re-judge or a D-only retry that
// carries E. So when evidenceAuthoritative is false, the live row's current E-owned values are read
// and stamped onto analysis before the write, carrying them forward unchanged. A fresh INSERT (no
// live row yet) has nothing to carry.
func (r *SessionAnalysisRepo) UpsertJudged(ctx context.Context, analysis *model.SessionAnalysis, evidenceAuthoritative bool) error {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "UpsertJudged", time.Now())
	if analysis == nil {
		return fmt.Errorf("upserting session analysis: nil analysis")
	}
	if analysis.ID == "" {
		analysis.ID = uuid.NewString()
	}
	if !evidenceAuthoritative {
		prior, err := r.GetByRawRecordID(ctx, analysis.RawRecordID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("reading live session analysis to carry forward E-owned columns (org %s, raw_record %s): %w", analysis.OrganizationID, analysis.RawRecordID, err)
		}
		if err == nil {
			analysis.SkillAssessments = prior.SkillAssessments
			analysis.ArtifactAdjudications = prior.ArtifactAdjudications
			// E's per-half readiness is likewise E-owned: when E did not fire this pass, its
			// prior disposition must survive an R-only re-judge's UpdateAll, or a previously
			// produced E output would read as never-produced.
			analysis.EvidenceArtifactDisposition = prior.EvidenceArtifactDisposition
			analysis.EvidenceRoundtripDisposition = prior.EvidenceRoundtripDisposition
		}
	}
	// The D-owned columns in the refresh set below (wasted_tokens, discarded_ranges,
	// discard_disposition) are DELIBERATELY not carried forward: D fires as part of R's fresh
	// judgment of the body (firesDiscard is a spine-ledger fact of this pass), so an R re-judge
	// where D no longer fires SHOULD null a now-stale waste ledger — unlike E, whose firing is
	// gated by an independent detector flag/threshold that can toggle without the judgment
	// changing. Do not symmetrize this into a D carry-forward, or a re-judge would resurrect a
	// prior generation's discarded-range/wasted-token data.
	// Returning id so analysis.ID is the LIVE row's id on a re-judge conflict (not the
	// fresh uuid), which the caller threads into the turn/phase composite FK.
	err := planscoped.WriteDB(ctx, r.db).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "organization_id"}, {Name: "raw_record_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "superseded_at IS NULL AND deleted_at IS NULL"}}},
		DoUpdates:   clause.AssignmentColumns(sessionAnalysisJudgedUpsertColumns),
	}, clause.Returning{Columns: []clause.Column{{Name: "id"}}}).Create(analysis).Error
	if err != nil {
		return fmt.Errorf("upserting session analysis (org %s, raw_record %s): %w", analysis.OrganizationID, analysis.RawRecordID, err)
	}
	return nil
}

// SupersedeContent marks the live content generation for a raw record SUPERSEDED
// (superseded_at = now), dropping it out of the one-live-generation partial unique so
// a fresh generation at a new vocabulary_version can be inserted alongside it. Used on
// a vocabulary bump, where the old and new generations must coexist (trends never
// cross the boundary). Runs inside the caller's commit
// transaction. Returns whether a live row was superseded.
func (r *SessionAnalysisRepo) SupersedeContent(ctx context.Context, rawRecordID string) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "SupersedeContent", time.Now())
	now := time.Now().UTC()
	res := planscoped.WriteDB(ctx, r.db).Model(&model.SessionAnalysis{}).
		Where("raw_record_id = ? AND superseded_at IS NULL AND deleted_at IS NULL", rawRecordID).
		Updates(map[string]any{"superseded_at": now, "updated_at": now})
	if res.Error != nil {
		return false, fmt.Errorf("superseding session analysis content (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// ClearServedVerdict invalidates the live generation's served verdict by clearing
// judged_at on the live content row (superseded_at IS NULL AND deleted_at IS NULL),
// dropping it out of the served set — sessionServingWhere requires judged_at IS NOT
// NULL — WITHOUT superseding it, so a re-judge of the same live generation restamps
// judged_at in place (UpsertJudged conflicts on superseded_at IS NULL, unaffected by
// judged_at). Called on a content-change reopen so a re-run that then fails never keeps
// serving the stale verdict for a conversation whose body has changed
// . Runs inside the caller's tenant-scoped transaction.
// Returns whether a live judged row was cleared.
func (r *SessionAnalysisRepo) ClearServedVerdict(ctx context.Context, rawRecordID string) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "ClearServedVerdict", time.Now())
	now := time.Now().UTC()
	res := planscoped.WriteDB(ctx, r.db).Model(&model.SessionAnalysis{}).
		Where("raw_record_id = ? AND superseded_at IS NULL AND deleted_at IS NULL AND judged_at IS NOT NULL", rawRecordID).
		Updates(map[string]any{"judged_at": nil, "updated_at": now})
	if res.Error != nil {
		return false, fmt.Errorf("clearing served verdict (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// MarkJudgeFailed — see ISessionAnalysisDAO. The judged_at IS NULL guard is what makes
// this a natural no-op both before spine_done (no measured row exists yet, so the
// UPDATE matches zero rows) and against a generation that already carries a valid
// judged verdict (judged_at already set), so a later terminal R failure can never
// clobber an earlier successful judge. It also nulls every sessionAnalysisJudgedColumns
// field: a content-change reopen's ClearServedVerdict clears only judged_at, so a row
// whose PRIOR generation was judged (and whose re-judge then fails) still carries that
// prior verdict — without this, restamping judged_at here would resurface stale judged
// data under a JUDGE_FAILED disposition, violating "JUDGE_FAILED's judged columns are
// NULL".
func (r *SessionAnalysisRepo) MarkJudgeFailed(ctx context.Context, rawRecordID string) (bool, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "MarkJudgeFailed", time.Now())
	now := time.Now().UTC()
	disposition := sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_JUDGE_FAILED
	updates := map[string]any{"judged_at": now, "judged_disposition": disposition, "updated_at": now}
	for _, column := range sessionAnalysisJudgedColumns {
		updates[column] = nil
	}
	res := planscoped.WriteDB(ctx, r.db).Model(&model.SessionAnalysis{}).
		Where("raw_record_id = ? AND superseded_at IS NULL AND deleted_at IS NULL AND judged_at IS NULL", rawRecordID).
		Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("marking judge-failed (raw_record %s): %w", rawRecordID, res.Error)
	}
	return res.RowsAffected == 1, nil
}

// GetByRawRecordID returns the live (non-superseded) analysis for a raw record,
// or ErrNotFound. One live generation exists per raw record; superseded
// generations are excluded.
func (r *SessionAnalysisRepo) GetByRawRecordID(ctx context.Context, rawRecordID string) (*model.SessionAnalysis, error) {
	defer metricutil.TrackDuration("session-intelligence/dao", "SessionAnalysisRepo", "GetByRawRecordID", time.Now())
	var result model.SessionAnalysis
	err := planscoped.DB(ctx, r.db).
		Where("raw_record_id = ? AND superseded_at IS NULL AND deleted_at IS NULL", rawRecordID).
		First(&result).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get session analysis by raw record (%s): %w", rawRecordID, err)
	}
	return &result, nil
}
