// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

// e2e coverage for two contracts the matrix suite leaves open:
//   - a SIMULTANEOUS vocabulary + spine version bump (the matrix has vocab-only and
//     spine-only, never both at once), where the vocabulary bump must dominate — a fresh
//     generation is superseded-and-re-analysed (a new analyze_read), and that fresh
//     generation carries BOTH new version stamps;
//   - the static-signal gating of the LLM tails: R is the one mandatory call, and the
//     spine's static ledgers gate E and D — a session whose static extraction finds no
//     evidence/revert signal makes NO E/D LLM call at all (the "static extraction adds no
//     LLM calls" contract), so a clean session's whole LLM budget is a single read.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// liveAnalysis returns the non-superseded content row for a raw record (the one serving
// reads), so a post-bump assertion targets the fresh generation, not the retired one.
func liveAnalysis(t *testing.T, rawRecordID string) *model.SessionAnalysis {
	t.Helper()
	var row model.SessionAnalysis
	require.NoError(t, testDB.Where("raw_record_id = ? AND superseded_at IS NULL", rawRecordID).First(&row).Error)
	return &row
}

// judgedState returns the single JUDGED state generation for a raw record — the live one
// after a bump superseded the old. The version stamps (spine/vocabulary) live on the
// state row, not the content row.
func judgedState(t *testing.T, rawRecordID string) model.SessionAnalysisState {
	t.Helper()
	for _, s := range allStates(t, rawRecordID) {
		if s.Status == sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED {
			return s
		}
	}
	t.Fatalf("no JUDGED state generation for raw record %s", rawRecordID)
	return model.SessionAnalysisState{}
}

// TestPipeline_CombinedVocabAndSpineBump asserts that when vocabulary AND spine both bump
// at once, the vocabulary bump dominates: unlike a spine-only reprocess (which re-judges
// the SAME generation with zero LLM), the vocabulary bump supersedes the old generation
// and analyses a fresh one — a new analyze_read — and the fresh generation's content row
// carries both the new spine and the new vocabulary version.
func TestPipeline_CombinedVocabAndSpineBump(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	dataDate := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)

	// First pass at vocab 1 / spine 2 (defaultConfig) → JUDGED, one read.
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, dataDate: dataDate, contentSha: "sha-fixed"})
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	require.Equal(t, 1, h.gen.reads())

	firstState := judgedState(t, rr)
	require.NotNil(t, firstState.SpineVersion)
	assert.Equal(t, int32(2), *firstState.SpineVersion, "first generation is spine 2")
	assert.Equal(t, int32(1), firstState.VocabularyVersion, "first generation is vocab 1")

	// Bump BOTH — vocabulary → 2 and spine → 3, effective the day before the record.
	h.proc = newHarnessProc(h, defaultConfig().withVocab(2, dataDate.AddDate(0, 0, -1)).withSpineVersion(3))
	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the version-behind row is re-picked")

	// The vocabulary bump forces a fresh generation, so a new analyze_read fires — this is
	// what separates a combined bump from a spine-only reprocess (which would stay at 1).
	assert.Equal(t, 2, h.gen.reads(), "the vocabulary bump forces a fresh analyze_read, unlike a spine-only reprocess")

	states := allStates(t, rr)
	var supersededOld, judgedNew int
	for i := range states {
		switch {
		case states[i].Status == sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SUPERSEDED && states[i].VocabularyVersion == 1:
			supersededOld++
		case states[i].Status == sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED && states[i].VocabularyVersion == 2:
			judgedNew++
		}
	}
	assert.Equal(t, 1, supersededOld, "the old vocab-1 generation is superseded, not rewritten")
	assert.Equal(t, 1, judgedNew, "a fresh vocab-2 generation is judged (both coexist)")

	// The fresh generation carries BOTH new stamps — proving the spine bump rode along with
	// the vocabulary bump into the new generation, not just the vocab.
	freshState := judgedState(t, rr)
	require.NotNil(t, freshState.SpineVersion)
	assert.Equal(t, int32(3), *freshState.SpineVersion, "fresh generation is spine 3")
	assert.Equal(t, int32(2), freshState.VocabularyVersion, "fresh generation is vocab 2")
	require.NotNil(t, liveAnalysis(t, rr).ScoreOverall, "the fresh generation is fully judged, not partial")
}

// TestPipeline_UpToDateSessionNotReprocessed is the "no wasted work" companion to the
// version-bump cases: when nothing has changed — same content, same versions — the next
// tick finds the generation up-to-date, re-picks nothing, spawns no new generation, and
// fires no redundant LLM call.
func TestPipeline_UpToDateSessionNotReprocessed(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, contentSha: "sha-fixed"})

	n1, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n1)
	require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))
	require.Equal(t, 1, h.gen.reads())

	// A second identical tick: nothing is version-behind or content-changed.
	n2, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "an up-to-date generation is not re-picked")
	assert.Equal(t, 1, h.gen.reads(), "no redundant analyze_read for an unchanged, up-to-date session")

	states := allStates(t, rr)
	require.Len(t, states, 1, "no new generation is spawned for an unchanged session")
	assert.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, states[0].Status)
}

// TestPipeline_StaticSignalsGateLLMCalls proves the LLM call budget is exactly determined
// by the static extraction: R is the single mandatory call, and the spine's static
// ledgers gate E and D. A session whose static extraction surfaces no evidence/revert
// signal makes NO E/D LLM call (NOT_FIRED, not run), so its whole LLM budget is one read.
func TestPipeline_StaticSignalsGateLLMCalls(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	notFired := sessionintelligencepb.CallStatus_CALL_STATUS_NOT_FIRED
	done := sessionintelligencepb.CallStatus_CALL_STATUS_DONE

	tests := []struct {
		name         string
		signals      convSignals
		wantEvidence int
		wantDiscard  int
		wantEStatus  sessionintelligencepb.CallStatus
		wantDStatus  sessionintelligencepb.CallStatus
	}{
		{
			name:        "clean session makes only the mandatory read — static extraction adds no LLM call",
			signals:     convSignals{},
			wantEStatus: notFired, wantDStatus: notFired,
		},
		{
			name:         "an artifact-ledger signal fires evidence only",
			signals:      convSignals{skillCalls: 2},
			wantEvidence: 1, wantEStatus: done, wantDStatus: notFired,
		},
		{
			name:        "a revert-ledger signal fires discard only",
			signals:     convSignals{userInterruptions: 2},
			wantDiscard: 1, wantEStatus: notFired, wantDStatus: done,
		},
		{
			name:         "both signals fire both tails",
			signals:      convSignals{skillCalls: 2, userInterruptions: 2},
			wantEvidence: 1, wantDiscard: 1, wantEStatus: done, wantDStatus: done,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			org, person := uuid.NewString(), uuid.NewString()
			cleanupOrg(t, org)
			h := newHarness(defaultConfig(), readFixtureClean)
			rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: tt.signals})

			_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
			require.NoError(t, err)
			require.Equal(t, sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED, statusOf(t, rr))

			assert.Equal(t, 1, h.gen.reads(), "exactly one mandatory analyze_read")
			assert.Equal(t, tt.wantEvidence, h.gen.evidences(), "E fires only when the static artifact ledger has content")
			assert.Equal(t, tt.wantDiscard, h.gen.discards(), "D fires only when the static revert ledger has content")

			eStatus, ok := callStatus(t, rr, "EVIDENCE")
			require.True(t, ok, "EVIDENCE call state recorded")
			assert.Equal(t, tt.wantEStatus, eStatus)
			dStatus, ok := callStatus(t, rr, "DISCARD")
			require.True(t, ok, "DISCARD call state recorded")
			assert.Equal(t, tt.wantDStatus, dStatus)

			// The whole LLM budget is the mandatory read plus only the statically-gated
			// tails — never a hidden call from the static extraction itself.
			assert.Equal(t, 1+tt.wantEvidence+tt.wantDiscard, h.gen.reads()+h.gen.evidences()+h.gen.discards(),
				"total LLM calls = mandatory read + only the statically-gated tails")
		})
	}
}
