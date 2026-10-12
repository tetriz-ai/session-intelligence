// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// The evidence-producer (E) and chunked-read matrix rows. E is conditional: it fires only
// where the session has something to adjudicate (a confirmed round-trip pair or an invoked
// skill in the static artifact ledger). The chunked path is the ~5% of sessions over the
// rendered-char limit — a windowed digest chain whose partial turn tables are stitched by
// prompt_index, all-or-nothing on a mid-chain failure.

// callStatus reads one producer's status out of the state row's call_state map.
func callStatus(t *testing.T, rawRecordID, key string) (sessionintelligencepb.CallStatus, bool) {
	t.Helper()
	st := readState(t, rawRecordID)
	require.NotNil(t, st, "expected a state row for %s", rawRecordID)
	require.NotNil(t, st.CallState, "expected a call_state map")
	call, ok := st.CallState.GetCalls()[key]
	if !ok {
		return sessionintelligencepb.CallStatus_CALL_STATUS_UNSPECIFIED, false
	}
	return call.GetStatus(), true
}

// turnAt returns the live judged turn row at one prompt index, or nil.
func turnAt(t *testing.T, rawRecordID string, promptIndex int32) *model.SessionTurnAnalysis {
	t.Helper()
	var turns []model.SessionTurnAnalysis
	require.NoError(t, testDB.Where("raw_record_id = ? AND prompt_index = ? AND deleted_at IS NULL", rawRecordID, promptIndex).
		Find(&turns).Error)
	if len(turns) == 0 {
		return nil
	}
	return &turns[0]
}

// roundTripFaultsFor returns E's round-trip faults for a session analysis, ordered by prompt
// index — the E-owned home the faults moved to (off R's turn rows).
func roundTripFaultsFor(t *testing.T, sessionAnalysisID string) []model.SessionRoundTripFault {
	t.Helper()
	var faults []model.SessionRoundTripFault
	require.NoError(t, testDB.Where("session_analysis_id = ? AND deleted_at IS NULL", sessionAnalysisID).
		Order("prompt_index ASC").Find(&faults).Error)
	return faults
}

// The flag_skipped_producers bits (READ=1<<0, EVIDENCE=1<<1, DISCARD=1<<2 in the engine);
// asserted directly so the test does not reach into the package's unexported constants.
const (
	bitRead     int32 = 1 << 0
	bitEvidence int32 = 1 << 1
)

// readFixtureEvidence triggers E's condition two ways over: turn 1 is a refined retry and a
// human correction, and the verdict names an invoked skill. smoothness is smooth so the
// agent-fault round-trip's monotone downgrade is observable.
const readFixtureEvidence = `{
  "turns": [
    {"prompt_index": 0, "prompt_type": "creation", "user_steering": "active", "prompt_issues": [],
     "prompt_objective_met": "not_met", "agent_helpfulness": "partially_helpful", "was_retry": "fresh"},
    {"prompt_index": 1, "prompt_type": "creation", "user_steering": "correcting", "prompt_issues": [],
     "prompt_objective_met": "met", "agent_helpfulness": "helpful", "was_retry": "retry_refined"}
  ],
  "phases": [
    {"prompt_range": {"start_index": 0, "end_index": 1}, "phase_theme": "creation",
     "task_complexity": "moderate", "phase_objective": "ship the parser"}
  ],
  "session_analysis": {
    "underlying_goal": "build a parser", "session_steering": "active", "goal_achieved": "somewhat",
    "smoothness": "smooth", "friction_cause": "none", "speed": "fast", "token_efficiency": "efficient",
    "goal_is_coding_work": true,
    "scores": {"overall": 6, "clarity": 6, "specificity": 6, "context_provision": 6,
               "actionability": 6, "completeness": 6, "efficiency": 6},
    "invoked_skills": [{"invocation_ref": "skill-1", "baseline_intent": "lint the diff",
                        "input_contract": "task", "contract_satisfied": true,
                        "contribution": "ran the linter", "evidence_prompt_index": 0}]
  }
}`

// evidenceFixture is E's three-key response: one adjudicated artifact (two contract checks,
// one a non-real clause; two findings, one accepted one rejected), one skill assessment
// keyed to R's invoked skill, and one round-trip attributing agent fault to the pair's P
// turn (index 0 — the earlier prompt of the (P=0, Q=1) pair the detector offered, since
// prompt_index is always P; a prompt_index the detector never offered is dropped by the
// commit-side validation, so the fixture must name a real candidate P-index).
const evidenceFixture = `{
  "artifacts": [{"artifact_ref": "art-1",
    "contract_checks": [{"gate_ref": "g1", "clause_is_a_real_requirement": true, "evidence_seq": 3},
                        {"gate_ref": "g2", "clause_is_a_real_requirement": false, "evidence_seq": 4}],
    "findings": [{"finding_id": "f1", "severity": "high", "disposition": "accepted", "attribution": "artifact_skill", "evidence_prompt_index": 1},
                 {"finding_id": "f2", "severity": "low", "disposition": "rejected", "attribution": "prompt", "evidence_prompt_index": 1}],
    "human_signal_present": true, "overall_attribution": "artifact_skill"}],
  "skill_assessments": [{"invocation_ref": "skill-1", "role": "producer", "contract_compliance": "adequate",
    "output_quality": "adequate", "execution_reliability": "clean", "human_alignment": "aligned",
    "verdict": "adequate", "evidence_seq": 2}],
  "round_trips": [{"prompt_index": 0, "fault": "agent", "prompt_completeness": "complete", "evidence_prompt_index": 1}]
}`

// TestPipeline_Evidence_Fires — E fires on a session with a round-trip pair and an invoked
// skill: it commits the artifact adjudications, writes the skill assessment to the E-owned
// skill_assessments column (NOT merged into R's invoked_skills), and lands the round-trip
// fault in the E-owned session_round_trip_faults table (NOT on R's turn row). R stays
// immutable — its invoked_skills and smoothness are untouched. call_state EVIDENCE is DONE, disposition PRODUCED.
func TestPipeline_Evidence_Fires(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	// The detector is live: #9 fault validation only commits a round-trip fault whose
	// prompt index the detector offered as a candidate, so E's fault at index 1 is
	// admitted only because the detector routed that pair to E.
	h := newHarness(defaultConfig().withDetector(true), readFixtureEvidence)
	h.gen.evidenceResult = evidenceFixture
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	assert.Equal(t, 1, h.gen.evidences(), "E fired exactly once")

	status, ok := callStatus(t, rr, "EVIDENCE")
	require.True(t, ok, "the EVIDENCE call is recorded")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, status)

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Zero(t, st.FlagSkippedProducers&bitEvidence, "E ran, so its flag-skipped bit is clear")

	an := readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.JudgedDisposition)
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED, *an.JudgedDisposition)

	// Artifact adjudications: findings aggregated to counts, gate clause tallied.
	require.NotNil(t, an.ArtifactAdjudications, "E's artifact adjudications are committed")
	require.Len(t, an.ArtifactAdjudications.GetItems(), 1)
	adj := an.ArtifactAdjudications.GetItems()[0]
	assert.Equal(t, "art-1", adj.GetArtifactRef())
	assert.Equal(t, int32(2), adj.GetGatesTotal())
	assert.Equal(t, int32(1), adj.GetGatesClauseNotReal(), "one contract check judged not a real requirement")
	assert.Equal(t, int32(1), adj.GetFindingsAccepted())
	assert.Equal(t, int32(1), adj.GetFindingsRejected())
	assert.Equal(t, sessionintelligencepb.OverallAttribution_OVERALL_ATTRIBUTION_ARTIFACT_SKILL, adj.GetOverallAttribution())

	// R's invoked_skills stays R-only — E's assessment is NOT merged in (R is immutable).
	require.NotNil(t, an.InvokedSkills)
	require.Len(t, an.InvokedSkills.GetItems(), 1)
	skill := an.InvokedSkills.GetItems()[0]
	assert.Equal(t, "skill-1", skill.GetInvocationRef())
	assert.Equal(t, "ran the linter", skill.GetContribution(), "R's field is untouched")
	assert.Equal(t, sessionintelligencepb.SkillVerdict_SKILL_VERDICT_UNSPECIFIED, skill.GetSkillVerdict(),
		"E's verdict is NOT merged into R's invoked_skills")

	// E's skill assessment lands in the E-owned skill_assessments column, keyed by invocation_ref.
	require.NotNil(t, an.SkillAssessments)
	require.Len(t, an.SkillAssessments.GetItems(), 1)
	assessment := an.SkillAssessments.GetItems()[0]
	assert.Equal(t, "skill-1", assessment.GetInvocationRef())
	assert.Equal(t, sessionintelligencepb.SkillVerdict_SKILL_VERDICT_ADEQUATE, assessment.GetSkillVerdict())
	assert.Equal(t, sessionintelligencepb.SkillRole_SKILL_ROLE_PRODUCER, assessment.GetRole())

	// The round-trip fault lands in the E-owned session_round_trip_faults table, NOT on R's turn row.
	// Its prompt_index is the pair's P turn (0 — the detector-offered candidate); a fault E returns
	// for a prompt_index the detector never offered is dropped by writeRoundTripFaults.
	turn := turnAt(t, rr, 1)
	require.NotNil(t, turn)
	assert.Nil(t, turn.RoundTripFault, "E no longer stamps the fault on R's turn row")
	faults := roundTripFaultsFor(t, an.ID)
	require.Len(t, faults, 1)
	assert.Equal(t, int32(0), faults[0].PromptIndex, "fault keyed by the pair's P index (0), the detector candidate")
	require.NotNil(t, faults[0].Fault)
	assert.Equal(t, sessionintelligencepb.RoundTripFault_ROUND_TRIP_FAULT_AGENT, *faults[0].Fault)
	require.NotNil(t, faults[0].EvidencePromptIndex)
	assert.Equal(t, int32(1), *faults[0].EvidencePromptIndex)

	// R's smoothness is NOT downgraded — R is immutable, so a smooth verdict stays smooth.
	require.NotNil(t, an.Smoothness)
	assert.Equal(t, sessionintelligencepb.Smoothness_SMOOTHNESS_SMOOTH, *an.Smoothness,
		"E no longer downgrades R's smoothness")
}

// TestPipeline_Evidence_ConditionNotMet — an evidence-free session (no round-trip pair, no
// invoked skill) records E as NOT_FIRED: no inference, the flag-skipped bit stays clear
// (an absence, not a skip), disposition PRODUCED.
func TestPipeline_Evidence_ConditionNotMet(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	assert.Equal(t, 0, h.gen.evidences(), "E does not fire on an evidence-free session")

	status, ok := callStatus(t, rr, "EVIDENCE")
	require.True(t, ok)
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED, status)

	st := readState(t, rr)
	require.NotNil(t, st)
	assert.Zero(t, st.FlagSkippedProducers&bitEvidence, "NOT_FIRED clears the flag-skipped bit")

	an := readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.JudgedDisposition)
	assert.Equal(t, sessionintelligencepb.JudgedDisposition_JUDGED_DISPOSITION_PRODUCED, *an.JudgedDisposition,
		"a NOT_FIRED conditional producer is not a partial judge")
	assert.Nil(t, an.ArtifactAdjudications, "no adjudications on an evidence-free session")
}

// TestPipeline_Evidence_FiresOnArtifactLedger — E fires from the spine's static artifact
// ledger alone, with NO round-trip tag in R's output: a session that invoked a skill has an
// adjudicable artifact, so E fires even though R tagged no retry. This is the designed
// code-owned fire decision — E is not gated on R's judged
// invoked-skills list.
func TestPipeline_Evidence_FiresOnArtifactLedger(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean) // clean read: no round-trip, no invoked skill
	h.gen.evidenceResult = evidenceFixture
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: convSignals{skillCalls: 1}})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, 1, h.gen.evidences(), "the static artifact ledger fires E without a round-trip tag")
	status, ok := callStatus(t, rr, "EVIDENCE")
	require.True(t, ok)
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, status)
}

// TestPipeline_Discard_ConditionGates — D is gated on the spine's branch/revert ledger, not
// on the flag alone: a session with no revert/abandon evidence records D as NOT_FIRED (no
// inference), while one with a user interruption fires D.
func TestPipeline_Discard_ConditionGates(t *testing.T) {
	discardResult := `{"discarded_ranges": [{"prompt_range": {"start_index": 0, "end_index": 0}, "reason": "reverted", "evidence_prompt_index": 0}]}`
	tests := []struct {
		name       string
		signals    convSignals
		wantFired  bool
		wantStatus sessionintelligencepb.CallStatus
	}{
		{name: "no revert evidence: D not fired", signals: convSignals{}, wantFired: false, wantStatus: sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED},
		{name: "a user interruption fires D", signals: convSignals{userInterruptions: 1}, wantFired: true, wantStatus: sessionintelligencepb.CallStatus_CALL_STATUS_DONE},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			org, person := uuid.NewString(), uuid.NewString()
			cleanupOrg(t, org)
			h := newHarness(defaultConfig(), readFixtureClean)
			h.gen.discardResult = discardResult
			rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: tt.signals})

			n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
			require.NoError(t, err)
			require.Equal(t, 1, n)

			status, ok := callStatus(t, rr, "DISCARD")
			require.True(t, ok)
			assert.Equal(t, tt.wantStatus, status)

			an := readAnalysis(t, rr)
			require.NotNil(t, an)
			if tt.wantFired {
				assert.NotNil(t, an.WastedTokens, "D fired and populated the waste ledger")
			} else {
				assert.Nil(t, an.WastedTokens, "D did not fire, so no waste ledger")
			}
		})
	}
}

// TestPipeline_MasterGateOff_ThenEnable is the R/E/D-dark-via-master-gate proof. R, E and D all ride
// the master SessionAnalysisEnabled gate (ReadEnabled); none carries a flag of its own. With the
// master gate off, runProducers' single early return holds the WHOLE producer chain off — R, E, and
// D all skip, no inference runs, and the row lands measured-only with judged_at NULL (withheld from
// serving). While the org stays dark the row must NOT churn category-9 re-admission. Once the org is
// enabled the row is re-admitted and E and D fire under the shared gates (their static ledgers).
func TestPipeline_MasterGateOff_ThenEnable(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	// The session fires BOTH E (an invoked skill in the static artifact ledger) and D (a user
	// interruption in the branch/revert ledger), so an enabled run exercises both tail producers.
	discardResult := `{"discarded_ranges": [{"prompt_range": {"start_index": 0, "end_index": 0}, "reason": "reverted", "evidence_prompt_index": 0}]}`
	h := newHarness(defaultConfig().withReadEnabled(false), readFixtureClean)
	h.gen.evidenceResult = evidenceFixture
	h.gen.discardResult = discardResult
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: convSignals{skillCalls: 1, userInterruptions: 1}})

	// Pass 1 — master gate off: the chain is held off; no producer runs, judged_at NULL.
	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	require.Zero(t, h.gen.reads(), "master gate off runs no R inference")
	require.Zero(t, h.gen.evidences(), "master gate off runs no E inference")
	require.Zero(t, h.gen.discards(), "master gate off runs no D inference")

	st := readState(t, rr)
	require.NotNil(t, st)
	require.NotZero(t, st.FlagSkippedProducers&bitRead, "master gate off flag-skips R")
	an := readAnalysis(t, rr)
	require.NotNil(t, an, "the row exists (measured superset)")
	require.Nil(t, an.JudgedAt, "no verdict while the master gate is off → withheld from serving")

	// Pass 2 — master gate STILL off: the re-admit mask is 0, so nothing churns.
	n, err = h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a master-off org does not churn category-9 re-admission")

	// Pass 3 — master gate on: category-9 re-admits and the full re-run fires E and D.
	proc := newHarnessProc(h, h.cfg.withReadEnabled(true))
	n, err = proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n, "master gate on → the row is re-admitted and re-run")
	assert.Equal(t, 1, h.gen.reads(), "R ran on the re-admit (a full run, not spine-only)")
	assert.Equal(t, 1, h.gen.evidences(), "E fires under the shared gates once the org is enabled")
	assert.Equal(t, 1, h.gen.discards(), "D fires under the shared gates once the org is enabled")

	eStatus, ok := callStatus(t, rr, "EVIDENCE")
	require.True(t, ok)
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, eStatus, "E ran on the re-admit")
	dStatus, ok := callStatus(t, rr, "DISCARD")
	require.True(t, ok)
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, dStatus, "D ran on the re-admit")

	an = readAnalysis(t, rr)
	require.NotNil(t, an)
	assert.NotNil(t, an.JudgedAt, "master gate on + re-run → verdict stamped, now served")
}

// windowRead builds a per-window analyze_read result: main-range turns at local indices
// 0..count-1 (plus an optional trailing look-ahead turn the stitch must drop), one covering
// phase, and a verdict carrying the given overall score so the final window's verdict is
// identifiable.
func windowRead(count int, withLookahead bool, overall int) string {
	turns := ""
	total := count
	if withLookahead {
		total = count + 1
	}
	for i := 0; i < total; i++ {
		if i > 0 {
			turns += ",\n"
		}
		turns += fmt.Sprintf(`{"prompt_index": %d, "prompt_type": "creation", "user_steering": "active", "prompt_issues": [],
     "prompt_objective_met": "met", "agent_helpfulness": "helpful", "was_retry": "fresh"}`, i)
	}
	return fmt.Sprintf(`{
  "turns": [%s],
  "phases": [{"prompt_range": {"start_index": 0, "end_index": %d}, "phase_theme": "creation",
    "task_complexity": "moderate", "phase_objective": "window work"}],
  "session_analysis": {"underlying_goal": "g", "session_steering": "active", "goal_achieved": "fully",
    "smoothness": "smooth", "friction_cause": "none", "speed": "fast", "token_efficiency": "efficient",
    "goal_is_coding_work": true,
    "scores": {"overall": %d, "clarity": 7, "specificity": 7, "context_provision": 7,
               "actionability": 7, "completeness": 7, "efficiency": 7}}
}`, turns, count-1, overall)
}

// TestPipeline_ChunkedRead_StitchesWindows — a session over the char limit runs the windowed
// read chain (window=2, lookahead=1) as two analyze_read calls, stitches the partial turn
// tables into one complete table by re-based prompt_index (dropping the look-ahead), and
// lands the final window's verdict.
func TestPipeline_ChunkedRead_StitchesWindows(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	// A tiny char limit forces the 4-human-turn body onto the chunked path; window 2 +
	// look-ahead 1 gives two windows [0,2) and [2,4).
	cfg := defaultConfig().withChunkCharLimit(50).withChunkWindow(2, 1)
	h := newHarness(cfg, "")
	h.gen.readByChunk = map[int32]string{
		0: windowRead(2, true, 5),  // window 0: main turns 0,1 + a look-ahead turn to drop
		1: windowRead(2, false, 8), // window 1: main turns 0,1 -> re-based to 2,3; verdict lands here
	}
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, body: mustBody(t, 4)})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	assert.Equal(t, 2, h.gen.reads(), "two windowed analyze_read calls")

	// The stitched turn table is complete: four turns at global prompt_index 0..3, none
	// duplicated, the look-ahead turn dropped.
	assert.Equal(t, int64(4), countRows(t, "session_turn_analyses", "raw_record_id = ? AND deleted_at IS NULL", rr),
		"all four human turns stitched, look-ahead dropped")
	for idx := int32(0); idx < 4; idx++ {
		turn := turnAt(t, rr, idx)
		require.NotNil(t, turn, "turn %d present in the stitched table", idx)
		require.NotNil(t, turn.ChunkIndex, "turn %d carries its chunk index", idx)
		wantChunk := int32(idx / 2)
		assert.Equal(t, wantChunk, *turn.ChunkIndex, "turn %d stitched from window %d", idx, wantChunk)
	}

	an := readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.ScoreOverall)
	assert.Equal(t, int32(8), *an.ScoreOverall, "the final window carries the session verdict")
}

// TestPipeline_ChunkedRead_MidChainFailure_AllOrNothing — a mid-chain window failure aborts
// the whole read chain: no turn/phase grain is committed (all-or-nothing) and the state row
// lands FAILED, so the next tick re-runs the entire chain (no per-window resumption). The
// measured content row spine_done already wrote survives the failure and serves statically:
// markFailed stamps it JUDGE_FAILED rather than leaving it un-servable — no judged verdict lands, only the measured (cost/token) columns.
func TestPipeline_ChunkedRead_MidChainFailure_AllOrNothing(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	cfg := defaultConfig().withChunkCharLimit(50).withChunkWindow(2, 1)
	h := newHarness(cfg, "")
	h.gen.readByChunk = map[int32]string{0: windowRead(2, true, 5)}
	failWindow := int32(1)
	h.gen.failChunk = &failWindow // window 1 fails after window 0 succeeded
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, body: mustBody(t, 4)})

	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err, "a per-record failure is isolated")
	require.Equal(t, 0, n, "the failed chunked read is not counted as processed")

	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_FAILED, statusOf(t, rr))
	assert.Equal(t, int64(0), countRows(t, "session_turn_analyses", "raw_record_id = ?", rr),
		"no window's partial output is committed (all-or-nothing)")

	an := readAnalysis(t, rr)
	require.NotNil(t, an, "the early measured write at spine_done survives the mid-chain failure")
	assert.Nil(t, an.JudgedAt, "a single (retryable) mid-chain failure leaves the row measured-only, not JUDGE_FAILED")
	assert.Nil(t, an.ScoreOverall, "no judged verdict lands on a mid-chain failure")
}

// mustBody builds a promptCount-human-turn conversation body, failing the test on error.
func mustBody(t *testing.T, promptCount int) []byte {
	t.Helper()
	body, err := buildConversationBody("IDE_TYPE_CLAUDE_CODE", time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC), promptCount)
	require.NoError(t, err)
	return body
}
