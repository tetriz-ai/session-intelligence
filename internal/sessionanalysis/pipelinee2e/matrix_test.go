// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/lineage"
)

// The pipeline edge-case matrix, each row driven through the REAL P1
// SessionAnalysisProcessor against the leased test DB. The LLM leg is the recorded W
// analyze_read fixture through the real parser (see doc.go / eval_test.go for the
// fixture-vs-real-Gemini split). Each test asserts the definition of done for its row:
// where the state machine lands and which grains were written.

// statusOf reads the live state row's status for a raw record, failing if absent.
func statusOf(t *testing.T, rawRecordID string) sessionintelligencepb.SessionAnalysisStatus {
	t.Helper()
	st := readState(t, rawRecordID)
	require.NotNil(t, st, "expected a session_analysis_state row for %s", rawRecordID)
	return st.Status
}

// TestPipeline_Ordinary — "Ordinary single-chunk session (~95%)": one analyze_read
// call, all grains, JUDGED.
func TestPipeline_Ordinary(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the ordinary session is processed")

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	assert.Equal(t, 1, h.gen.reads(), "exactly one analyze_read call")

	an := readAnalysis(t, rr)
	require.NotNil(t, an, "a session_analyses content row was written")
	assert.NotNil(t, an.JudgedAt, "judged_at is stamped (served)")
	require.NotNil(t, an.JudgedDisposition)
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED, *an.JudgedDisposition)
	require.NotNil(t, an.ScoreOverall, "R's input-quality score threaded through the parser")
	assert.Equal(t, int32(8), *an.ScoreOverall)
	require.NotNil(t, an.TurnCount)
	assert.Equal(t, int32(4), *an.TurnCount, "measured turn_count counts every prompt (2 user + 2 assistant)")

	assert.Equal(t, int64(2), countRows(t, "session_turn_analyses", "raw_record_id = ? AND deleted_at IS NULL", rr), "two judged turns (R's fixture)")
	assert.Equal(t, int64(1), countRows(t, "session_phase_analyses", "session_analysis_id = ? AND deleted_at IS NULL", an.ID), "one phase")
	assert.Equal(t, int64(0), countRows(t, "security_flags", "raw_record_id = ?", rr), "no security flag on a clean session")
}

// TestPipeline_Security — "Security session": the security-layer prompt_issues
// reconcile into security_flags.
func TestPipeline_Security(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureSecurity)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	require.Equal(t, int64(1), countRows(t, "security_flags", "raw_record_id = ? AND status = ?", rr, "open"), "one open security flag")

	var flags []struct {
		FlagType string
		Severity string
		PersonID string
	}
	require.NoError(t, testDB.Table("security_flags").
		Select("flag_type, severity, person_id").
		Where("raw_record_id = ?", rr).Scan(&flags).Error)
	require.Len(t, flags, 1)
	assert.Equal(t, "pii-injection", flags[0].FlagType)
	assert.Equal(t, "high", flags[0].Severity)
	assert.Equal(t, person, flags[0].PersonID)
}

// TestPipeline_Skip_NoSeat — "Skips": an unseated person's record is SKIPPED(NO_SEAT),
// nothing served, no S3 read and no LLM call.
func TestPipeline_Skip_NoSeat(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

	// Seat a DIFFERENT person, so the record's person is not seated.
	ctx := driveCtx([]string{uuid.NewString()}, 3650, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	n, err := h.proc.ProcessFlat(ctx, org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, st.Status)
	require.NotNil(t, st.SkipReason)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_NO_SEAT, *st.SkipReason)
	assert.Nil(t, readAnalysis(t, rr), "nothing served")
	assert.Equal(t, 0, h.gen.reads(), "no LLM call on a pre-fetch skip")
	assert.Equal(t, 0, h.s3.reads, "no S3 read on a pre-fetch skip")
}

// TestPipeline_Skip_OutOfWindow — "Skips": a record before the org's data-window
// cutoff is SKIPPED(OUT_OF_WINDOW).
func TestPipeline_Skip_OutOfWindow(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	// data_date well before a 1-day window anchored on a recent org-created date.
	oldDate := time.Now().UTC().AddDate(0, 0, -120).Truncate(24 * time.Hour)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, dataDate: oldDate})

	ctx := driveCtx([]string{person}, 1, time.Now().UTC())
	n, err := h.proc.ProcessFlat(ctx, org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, st.Status)
	require.NotNil(t, st.SkipReason)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW, *st.SkipReason)
	assert.Equal(t, 0, h.gen.reads(), "no LLM call")
}

// TestPipeline_Skip_Child — "Subagent / child session": a delegated child folds via
// its parent, child state SKIPPED(CHILD), no LLM call.
func TestPipeline_Skip_Child(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{
		orgID: org, personID: person,
		relationType: lineage.RelationTypeSubagent, parentRecordKey: "parent-key-" + uuid.NewString()[:6],
	})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SKIPPED, st.Status)
	require.NotNil(t, st.SkipReason)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisSkipReason_SESSION_ANALYSIS_SKIP_REASON_CHILD, *st.SkipReason)
	assert.Equal(t, 0, h.gen.reads())
}

// TestPipeline_Primary_NotDroppedAsChild asserts an explicit
// relation_type=PRIMARY is a real top-level session, not a child to fold away. The
// CHILD gate classifies by lineage.IsChild (genuine child types only), so a PRIMARY
// session with a parent_record_key present still analyses end-to-end and is JUDGED —
// the presence-check gate used to lose it as SKIPPED(CHILD).
func TestPipeline_Primary_NotDroppedAsChild(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{
		orgID: org, personID: person,
		relationType: lineage.RelationTypePrimary, parentRecordKey: "parent-key-" + uuid.NewString()[:6],
	})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, st.Status,
		"a PRIMARY session must analyse, not fold away as a child")
	assert.Nil(t, st.SkipReason)
	assert.Equal(t, 1, h.gen.reads(), "the read pass ran")
}

// TestPipeline_SpineOnlyReprocess — "Spine-only reprocess": a spine-version bump whose
// producer-read inputs are unchanged corrects the measured columns with zero LLM calls and
// carries the prior judged verdict.
func TestPipeline_SpineOnlyReprocess(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, contentSha: "sha-fixed"})

	// First pass → JUDGED, one analyze_read.
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	require.Equal(t, 1, h.gen.reads())

	// Bump the spine version; the body content + READ contract are unchanged, so the
	// record is version-behind on the spine only → spine-only reprocess, zero LLM.
	h.proc = newHarnessProc(h, defaultConfig().withSpineVersion(3))
	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the version-behind row is re-picked")

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	assert.Equal(t, 1, h.gen.reads(), "spine-only reprocess fires no new analyze_read (the zero-cost path)")

	an := readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.ScoreOverall, "the prior judged verdict is carried, not wiped")
	assert.Equal(t, int32(8), *an.ScoreOverall)
}

// TestPipeline_VocabBump — "Version bump": a vocabulary bump supersedes the old
// generation and analyses a fresh one, so trends never cross the boundary (the two
// generations coexist).
func TestPipeline_VocabBump(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	dataDate := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, dataDate: dataDate, contentSha: "sha-fixed"})

	// First pass at vocab 1 → JUDGED.
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))

	// Bump vocabulary to 2, effective before the record's date → the row is
	// version-behind and is superseded + re-analysed at the new vocab.
	h.proc = newHarnessProc(h, defaultConfig().withVocab(2, dataDate.AddDate(0, 0, -1)))
	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	states := allStates(t, rr)
	var superseded, judgedNew int
	for i := range states {
		switch {
		case states[i].Status == sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SUPERSEDED && states[i].VocabularyVersion == 1:
			superseded++
		case states[i].Status == sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED && states[i].VocabularyVersion == 2:
			judgedNew++
		}
	}
	assert.Equal(t, 1, superseded, "the old vocab-1 generation is superseded, not rewritten")
	assert.Equal(t, 1, judgedNew, "a fresh vocab-2 generation is judged")
}

// TestPipeline_PostSpineFailure_FirstTime — "Post-spine failure (first-time)": R times
// out after the spine wrote its measured columns on a fresh row; the row stays out of every
// served aggregate (no judged content row), state FAILED, attempt incremented.
func TestPipeline_PostSpineFailure_FirstTime(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	h.gen.err = assert.AnError // analyze_read fails
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err, "a per-record failure is isolated, never fails the batch")
	require.Equal(t, 0, n, "a failed record is not counted as processed")

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, st.Status)
	assert.Equal(t, int32(1), st.AttemptCount, "one attempt so far")
	assert.Equal(t, 1, h.s3.reads, "the spine ran (S3 read) before the producer failed")

	// The measured row spine_done wrote survives the producer failure, but a single failure is
	// RETRYABLE (attempt_count < RetryCap), so it is not yet terminally judged: judged_at stays
	// NULL so the row is not served as JUDGE_FAILED during its retry window (a later successful
	// retry judges it normally).
	an := readAnalysis(t, rr)
	require.NotNil(t, an, "the early measured write at spine_done survives the producer failure")
	assert.Nil(t, an.JudgedAt, "a retryable failure leaves the row measured-only, not served as JUDGE_FAILED")

	// Static-on-failure serving: once the retry budget is
	// exhausted (attempt_count reaches RetryCap), the terminal failure stamps JUDGE_FAILED over the
	// measured row, so a session whose R never resolves still serves its cost/token cards.
	retryCap := defaultConfig().RetryCap()
	for i := int32(1); i < retryCap; i++ {
		_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
		require.NoError(t, err)
	}
	st = readState(t, rr)
	require.NotNil(t, st)
	assert.GreaterOrEqual(t, st.AttemptCount, retryCap, "retry budget exhausted")
	an = readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.JudgedAt, "the terminal failure stamps JUDGE_FAILED so the measured row serves")
	require.NotNil(t, an.JudgedDisposition)
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_JUDGE_FAILED, *an.JudgedDisposition)
	assert.Nil(t, an.ScoreOverall, "no judged verdict lands on a terminal producer failure")
}

// TestPipeline_ContentChangeThrottle — "Content-change re-run": the first upload is
// analysed immediately; a content change within reprocess_min_interval is NOT re-analysed
// (at most once per interval).
func TestPipeline_ContentChangeThrottle(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, contentSha: "sha-v1"})

	// First upload → immediate judge.
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	require.Equal(t, 1, h.gen.reads())

	// The body content changes (new sha) immediately after — within the 12h throttle.
	require.NoError(t, testDB.Exec(
		`UPDATE raw_records SET payload = jsonb_set(payload::jsonb, '{content_sha256}', '"sha-v2"') WHERE id = ?`, rr,
	).Error)

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a content change within reprocess_min_interval is not re-picked")
	assert.Equal(t, 1, h.gen.reads(), "no second analyze_read within the throttle window")
}

// TestPipeline_ThinCapture — "Thin-capture IDE": a thin-capture session still produces
// the measured (cost/token/count) columns and is served. NOTE the deterministic
// judged-disposition = NOT_APPLICABLE for a thin IDE is a judged-tier behaviour deferred
// past P1, so this asserts the measured superset is preserved (the row is not hidden).
func TestPipeline_ThinCapture(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, ideType: "IDE_TYPE_ANTIGRAVITY"})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	an := readAnalysis(t, rr)
	require.NotNil(t, an, "the thin-capture session still lands a content row (measured superset)")
	assert.NotNil(t, an.JudgedAt)
	require.NotNil(t, an.TurnCount)
	assert.Equal(t, int32(4), *an.TurnCount, "measured columns present regardless of IDE fidelity")
}

// TestPipeline_ContentChangeReopen_FailedRerunDropsStaleVerdict asserts a
// content-change reopen invalidates the served verdict, so if the re-run then fails, the
// changed conversation is NOT served with the stale verdict — it drops out of serving
// until it is re-judged. (The spine-only reprocess path, which keeps the prior verdict,
// is covered by TestPipeline_SpineOnlyReprocess.)
func TestPipeline_ContentChangeReopen_FailedRerunDropsStaleVerdict(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	// The reprocess throttle is disabled so the content change re-picks immediately.
	h := newHarness(defaultConfig().withReprocessMin(0), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, contentSha: "sha-v1"})

	// Pass 1 → JUDGED and served (judged_at set).
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	first := readAnalysis(t, rr)
	require.NotNil(t, first)
	require.NotNil(t, first.JudgedAt, "served after the first judge")

	// The body content changes (new sha)...
	require.NoError(t, testDB.Exec(
		`UPDATE raw_records SET payload = jsonb_set(payload::jsonb, '{content_sha256}', '"sha-v2"') WHERE id = ?`, rr,
	).Error)
	// ...and the re-run's analyze_read fails.
	h.gen.err = assert.AnError

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err, "a per-record failure never fails the batch")
	require.Equal(t, 0, n, "the failed re-run is not counted as processed")

	// The reopen cleared judged_at, and the failed re-run is retryable (attempt_count < RetryCap),
	// so it does NOT restamp JUDGE_FAILED — judged_at stays NULL, and the changed session drops
	// out of serving entirely until it is re-judged (never served with the stale OLD verdict).
	after := readAnalysis(t, rr)
	require.NotNil(t, after, "the live content row still exists (invalidated in place, not superseded)")
	// judged_at NULL is the serving guarantee: the changed session drops out of serving (the
	// router gates on judged_at IS NOT NULL), so the stale OLD verdict is never shown — regardless
	// of any stale judged field still sitting in the row, which a re-judge or a terminal
	// JUDGE_FAILED will overwrite/clear.
	assert.Nil(t, after.JudgedAt, "a retryable re-run leaves judged_at NULL — the row drops out of serving")
	assert.Nil(t, after.SupersededAt, "invalidated in place, not superseded")
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, statusOf(t, rr))
}

// TestPipeline_WasteLedger_PopulatesWastedTokens asserts D's discarded
// ranges drive the code-summed waste ledger, so a wasted session commits a non-zero
// wasted_tokens (this ledger is its only writer; absent it the field stays 0). D never
// rewrites R's token_efficiency — R is immutable (there is no monotone
// downgrade), so a token-efficient R verdict stays efficient.
func TestPipeline_WasteLedger_PopulatesWastedTokens(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	// D flags turn 0 (the first user/assistant pair) as discarded work.
	h.gen.discardResult = `{"discarded_ranges": [{"prompt_range": {"start_index": 0, "end_index": 0}, "reason": "reverted the approach", "evidence_prompt_index": 0}]}`
	// A user interruption is revert/abandon evidence, so the spine's branch/revert ledger
	// fires D (which then flags the discarded range).
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: convSignals{userInterruptions: 1}})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	an := readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.WastedTokens, "the waste ledger populates wasted_tokens")
	assert.Positive(t, *an.WastedTokens, "a discarded range charges its measured tokens as waste")
	require.NotNil(t, an.DiscardedRanges)
	require.Len(t, an.DiscardedRanges.GetItems(), 1)
	assert.Positive(t, an.DiscardedRanges.GetItems()[0].GetTokenSum(), "the range's own token_sum is code-summed")
	require.NotNil(t, an.TokenEfficiency)
	assert.Equal(t, sessionintelligencepb.TokenEfficiency_TOKEN_EFFICIENCY_EFFICIENT, *an.TokenEfficiency,
		"D never rewrites R's token_efficiency — an efficient verdict stays efficient")
}
