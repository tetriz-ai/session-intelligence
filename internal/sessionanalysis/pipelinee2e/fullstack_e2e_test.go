// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	refllmclient "github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// The committed full-stack driver. TestRealGeminiPipelineE2E already proves the pipeline
// against real Gemini, but it commits to the rolled-back lease DB, so a separately-running
// reader can never see its rows. This driver runs the SAME real pipeline but COMMITS into a
// persistent dev database (the session_intelligence database) instead of the rolled-back lease
// DB, so the judged rows survive the run and a separately-running reader can see them.
//
// Two env vars gate the skip, so it stays out of an ordinary CI run (which sets neither); a third
// selects the DB and defaults to the local dev instance:
//   - SESSIONINTEL_FULLSTACK_E2E=1        — the opt-in switch (gates)
//   - SESSIONINTEL_REAL_GEMINI_URL=<url>  — a running reference-llm backed by real Gemini (gates)
//   - SESSIONINTEL_FULLSTACK_DSN=<dsn>    — the dev DB (optional; defaults to local session_intelligence)
//
// Run it, with reference-llm up on :8000:
//
//	SESSIONINTEL_FULLSTACK_E2E=1 SESSIONINTEL_REAL_GEMINI_URL=http://localhost:8000 \
//	  go test -count=1 -run TestFullStackSessionAnalysisE2E \
//	  ./internal/sessionanalysis/pipelinee2e/
//
// Because the model output is non-deterministic every assertion is STRUCTURAL — a row is
// present, a call reached DONE, a count is positive — never an exact score or verdict.
//
// It seeds a throwaway UUID org and always deletes its rows at cleanup. The driver commits a
// judged row; whether an org's window is then served from the session tables is a downstream
// reader's decision, not asserted here. A real org's committed serving-seed is the scheduler's job, not
// this driver's — ProcessFlat scans the org's whole actionable backlog, so a real org's counts
// are not this driver's to pin.
//
// SESSIONINTEL_FULLSTACK_DSN must point at a LOCAL dev database and carry the __system__ / -infinity
// bypass options (see fullstackDSN); the driver commits and then DELETEs, so never point it at a
// shared or production database. The package TestMain also leases a throwaway _test pool DB, so a
// reachable local Postgres is required even though this driver writes only to the dev DB.

// fullstackDSN is the libpq DSN for the committed dev DB. The single-tenant build applies no
// row-level security, so the connection carries no tenant/data-cutoff GUCs — only the fixed
// UTC time zone, so timestamps the seed writes and the read-backs compare are unambiguous.
func fullstackDSN() string {
	if dsn := os.Getenv("SESSIONINTEL_FULLSTACK_DSN"); dsn != "" {
		return dsn
	}
	return "host=localhost port=5432 sslmode=disable dbname=session_intelligence " +
		"TimeZone=UTC"
}

// openDevDB opens the committed dev DB. Unlike the leased test DB it is NOT rolled back —
// every write persists, which is the whole point (a running backend reads them).
func openDevDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(fullstackDSN()), &gorm.Config{})
	require.NoError(t, err, "open committed dev DB")
	return db
}

// newDevProc builds the processor over the committed dev DB with the REAL reference-llm client,
// mirroring newRealHarness but swapping the leased conn for the dev conn.
func newDevProc(devDB *gorm.DB, url string, cfg pipeConfig) (*sa.SessionAnalysisProcessor, *fakeS3) {
	store := newFakeS3()
	proc := sa.NewSessionAnalysisProcessor(
		dao.NewRawRecordRepo(devDB),
		dao.NewSessionAnalysisRepo(devDB),
		dao.NewSessionAnalysisStateRepo(devDB),
		dao.NewSessionTurnAnalysisRepo(devDB),
		dao.NewSessionPhaseAnalysisRepo(devDB),
		dao.NewSessionRoundTripFaultRepo(devDB),
		dao.NewSecurityFlagRepo(devDB),
		dao.NewHarnessArtifactRepo(devDB),
		dao.NewHarnessArtifactClassificationRepo(devDB),
		store,
		sa.NewLLMClient(refllmclient.New(url)),
		nil,
		devDB,
		cfg,
	)
	return proc, store
}

// seedDev commits one session raw_record to the dev DB and stages its body into the in-memory
// S3 the processor reads. conversationStart drives session_analyses.conversation_started_at,
// which the serving router's coverage gate reads — a post-cutover date keeps the judged row in
// the servable window.
func seedDev(t *testing.T, devDB *gorm.DB, store *fakeS3, orgID, personID string, conversationStart time.Time, promptCount int, signals convSignals) string {
	t.Helper()
	body, err := buildConversationBody("IDE_TYPE_CLAUDE_CODE", conversationStart, promptCount, signals)
	require.NoError(t, err, "build conversation body")

	s3Key := "s3://body/" + uuid.NewString()
	store.put(s3Key, body)

	payload, err := json.Marshal(map[string]string{"s3_key": s3Key})
	require.NoError(t, err)
	meta, err := json.Marshal(map[string]string{"person_id": personID})
	require.NoError(t, err)

	record := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      sessionConnectorID,
		OperationID:      "conversation_upload",
		DataDate:         conversationStart,
		RecordKey:        "fullstack-" + uuid.NewString()[:8],
		Payload:          payload,
		Metadata:         meta,
		FetchedAt:        conversationStart.Add(12 * time.Hour),
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, devDB.Create(record).Error, "seed raw_record into dev DB")
	return record.ID
}

// cleanupDevOrg removes every row the driver commits for the throwaway org, in FK-safe order
// (children before session_analyses, then state, then raw_records).
func cleanupDevOrg(t *testing.T, devDB *gorm.DB, orgID string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tbl := range []string{
			"security_flags", "session_round_trip_faults", "session_turn_analyses",
			"session_phase_analyses", "session_analyses",
			"session_analysis_state", "raw_records",
		} {
			devDB.Exec("DELETE FROM "+tbl+" WHERE organization_id = ?", orgID)
		}
	})
}

func devReadAnalysis(t *testing.T, devDB *gorm.DB, rawRecordID string) *model.SessionAnalysis {
	t.Helper()
	var rows []model.SessionAnalysis
	require.NoError(t, devDB.Where("raw_record_id = ?", rawRecordID).Find(&rows).Error)
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

func devReadState(t *testing.T, devDB *gorm.DB, rawRecordID string) *model.SessionAnalysisState {
	t.Helper()
	var states []model.SessionAnalysisState
	require.NoError(t, devDB.Where("raw_record_id = ?", rawRecordID).
		Order("created_at DESC").Find(&states).Error)
	if len(states) == 0 {
		return nil
	}
	return &states[0]
}

// devCallStatus reads one producer's terminal call status from the committed state row's
// call_state map (keyed "READ"/"EVIDENCE"/"DISCARD"), mirroring the harness callStatus helper
// but against the dev conn.
func devCallStatus(t *testing.T, devDB *gorm.DB, rawRecordID, key string) (sessionintelligencepb.CallStatus, bool) {
	t.Helper()
	st := devReadState(t, devDB, rawRecordID)
	require.NotNil(t, st, "a state row exists for %s", rawRecordID)
	require.NotNil(t, st.CallState, "the committed state carries a call_state map")
	call, ok := st.CallState.GetCalls()[key]
	if !ok {
		return sessionintelligencepb.CallStatus_CALL_STATUS_UNSPECIFIED, false
	}
	return call.GetStatus(), true
}

func TestFullStackSessionAnalysisE2E(t *testing.T) {
	if os.Getenv("SESSIONINTEL_FULLSTACK_E2E") != "1" {
		t.Skip("set SESSIONINTEL_FULLSTACK_E2E=1 to run the committed full-stack E2E driver")
	}
	url := os.Getenv("SESSIONINTEL_REAL_GEMINI_URL")
	if url == "" {
		t.Skip("set SESSIONINTEL_REAL_GEMINI_URL to run the committed full-stack E2E driver")
	}
	devDB := openDevDB(t)

	// A post-cutover conversation start keeps the judged row inside the serving window, and a
	// generous data window + the record's person seated is the same unlimitedCtx the harness
	// uses so only the seeded signals gate E/D.
	conversationStart := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := defaultConfig().withCutover(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))

	// A signal-bearing session so all three producers fire and commit against the dev DB: a
	// SKILL_CALL ledger fires E, user interruptions fire D, R always runs. One committed judged
	// row carrying the full R verdict plus terminal E and D is the servable artifact. A fresh
	// UUID org isolates the run (exactly one actionable record) and is cleaned up.
	org, person := uuid.NewString(), uuid.NewString()
	cleanupDevOrg(t, devDB, org)
	proc, store := newDevProc(devDB, url, cfg)
	rr := seedDev(t, devDB, store, org, person, conversationStart, 2, convSignals{skillCalls: 3, userInterruptions: 2})

	processed, err := proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err, "the real pipeline committed against the dev DB")
	require.Equal(t, 1, processed, "the seeded session was processed")

	an := devReadAnalysis(t, devDB, rr)
	require.NotNil(t, an, "R committed a session_analyses row into the dev DB")
	assert.NotNil(t, an.ScoreOverall, "R judged an overall input-quality score")
	assert.NotNil(t, an.GoalAchieved, "R judged the session's goal outcome")
	require.NotNil(t, an.TurnCount, "R materialized the turn count")
	assert.Positive(t, *an.TurnCount, "the judged turn table is non-empty")

	turnRows := int64(0)
	require.NoError(t, devDB.Table("session_turn_analyses").
		Where("raw_record_id = ? AND deleted_at IS NULL", rr).Count(&turnRows).Error)
	assert.Positive(t, turnRows, "turn children committed for the judged session")

	state := devReadState(t, devDB, rr)
	require.NotNil(t, state, "a lifecycle state row committed")
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, state.Status,
		"the committed state reached the terminal JUDGED disposition")

	evStatus, ok := devCallStatus(t, devDB, rr, "EVIDENCE")
	require.True(t, ok, "the EVIDENCE producer recorded a call state")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, evStatus, "E ran to completion against real Gemini")

	dStatus, ok := devCallStatus(t, devDB, rr, "DISCARD")
	require.True(t, ok, "the DISCARD producer recorded a call state")
	assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, dStatus, "D ran to completion against real Gemini")

	t.Logf("committed JUDGED session_analyses id=%s org=%s raw_record=%s turns=%d — servable window from %s",
		an.ID, org, rr, turnRows, conversationStart.Format("2006-01-02"))
}

// TestFullStackHitlGateE2E drives a real session whose HITL permission mode SWITCHES mid-session
// (plan on the early turns, acceptEdits after) through the real pipeline + real Gemini, then asserts
// each committed phase's hitl_gate is the mode active at the phase's RangeStart turn — the
// position-aware forward-fill, proven end-to-end against a real Postgres write.
func TestFullStackHitlGateE2E(t *testing.T) {
	if os.Getenv("SESSIONINTEL_FULLSTACK_E2E") != "1" {
		t.Skip("set SESSIONINTEL_FULLSTACK_E2E=1 to run the committed full-stack E2E driver")
	}
	url := os.Getenv("SESSIONINTEL_REAL_GEMINI_URL")
	if url == "" {
		t.Skip("set SESSIONINTEL_REAL_GEMINI_URL to run the committed full-stack E2E driver")
	}
	devDB := openDevDB(t)

	conversationStart := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cfg := defaultConfig().withCutover(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))

	// Six user turns with a mid-session HITL switch: plan (turns 0-2) then acceptEdits (turns 3-5).
	// permission_mode is stamped on those user turns; the spine forward-fills per prompt_index and
	// buildPhaseRows reads the gate at each phase's RangeStart.
	// Eight user turns with a clear phase boundary and a HITL switch at it: a design/planning
	// segment (turns 0-3, plan mode) then an implementation segment (turns 4-7, acceptEdits), so
	// real Gemini is likely to split into distinct phases and the per-phase switch is visible.
	perTurn := []string{"plan", "plan", "plan", "plan", "acceptEdits", "acceptEdits", "acceptEdits", "acceptEdits"}
	contents := []string{
		"Let's design the CSV parser. Outline the module structure and the tokenizer approach. Do not write code yet.",
		"Compare a hand-written state machine vs a regex tokenizer for the quoting rules. Recommend one.",
		"Sketch the error-reporting model (line/column, partial rows). Still just the design, no code.",
		"Finalize the design: list the functions and their signatures we will implement.",
		"Great, now implement the tokenizer function we designed and add a table-driven test.",
		"Implement the row assembler and its tests.",
		"Implement the error-reporting path and cover it with tests.",
		"Wire it together into the public Parse() entrypoint and add an end-to-end test.",
	}
	expectByTurn := []sessionintelligencepb.HitlGate{
		sessionintelligencepb.HitlGate_HITL_GATE_PLAN, sessionintelligencepb.HitlGate_HITL_GATE_PLAN,
		sessionintelligencepb.HitlGate_HITL_GATE_PLAN, sessionintelligencepb.HitlGate_HITL_GATE_PLAN,
		sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS, sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS,
		sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS, sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS,
	}

	org, person := uuid.NewString(), uuid.NewString()
	cleanupDevOrg(t, devDB, org)
	proc, store := newDevProc(devDB, url, cfg)
	rr := seedDev(t, devDB, store, org, person, conversationStart, len(perTurn),
		convSignals{skillCalls: 3, userInterruptions: 2, permissionModes: perTurn, contents: contents})

	processed, err := proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err, "the real pipeline committed against the dev DB")
	require.Equal(t, 1, processed, "the seeded session was processed")

	state := devReadState(t, devDB, rr)
	require.NotNil(t, state, "a lifecycle state row committed")
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, state.Status,
		"JUDGED so phase rows exist to carry hitl_gate")

	an := devReadAnalysis(t, devDB, rr)
	require.NotNil(t, an, "R committed a session_analyses row")
	var phases []model.SessionPhaseAnalysis
	require.NoError(t, devDB.Where("session_analysis_id = ? AND deleted_at IS NULL", an.ID).
		Order("range_start").Find(&phases).Error)
	require.NotEmpty(t, phases, "R committed phase rows")

	sawPlan, sawAcceptEdits := false, false
	for _, ph := range phases {
		require.NotNil(t, ph.RangeStart, "phase carries a RangeStart")
		start := int(*ph.RangeStart)
		require.GreaterOrEqual(t, start, 0)
		exp := expectByTurn[len(expectByTurn)-1]
		if start < len(expectByTurn) {
			exp = expectByTurn[start]
		}
		require.NotNil(t, ph.HitlGate, "phase starting at turn %d has hitl_gate populated (not NULL)", start)
		assert.Equal(t, exp, *ph.HitlGate,
			"phase starting at turn %d must carry the HITL mode active there (forward-filled)", start)
		switch *ph.HitlGate {
		case sessionintelligencepb.HitlGate_HITL_GATE_PLAN:
			sawPlan = true
		case sessionintelligencepb.HitlGate_HITL_GATE_ACCEPT_EDITS:
			sawAcceptEdits = true
		}
		t.Logf("phase range_start=%d hitl_gate=%s", start, ph.HitlGate.String())
	}
	t.Logf("hitl_gate e2e: %d phase(s); sawPlan=%v sawAcceptEdits=%v (both true means the mid-session switch landed on distinct phases)",
		len(phases), sawPlan, sawAcceptEdits)
}
