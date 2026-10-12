// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	refllmclient "github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
)

// The integrated real-Gemini E2E: it drives the ACTUAL Go pipeline (ProcessFlat) plus the
// R/E/D producers and the chunking spine against a RUNNING reference-llm backed by real
// Gemini — the whole suite otherwise runs the recorded fixture parser (see fixtures_test.go
// / eval_test.go). This lifts the pipeline + producers + windowing from fixture-only to
// real-Gemini-validated: the fixture proves the parser accepts a known transcript, this
// proves the producers commit a coherent judged contract off a live, non-deterministic
// model. Because the model output is non-deterministic, every assertion is STRUCTURAL —
// a field is present, a count is positive, no error — never an exact score or verdict.
//
// To run it, start reference-llm with the repo-root Gemini key wired and point the env var at
// it, e.g.:
//
//	cd reference-llm && uv run uvicorn app.main:app --port 8000
//	SESSIONINTEL_REAL_GEMINI_URL=http://localhost:8000 go test -count=1 -run TestRealGeminiPipelineE2E \
//	  ./internal/sessionanalysis/pipelinee2e/

// newRealHarness mirrors newHarness but injects the REAL reference-llm client (which satisfies
// the ContentGenerator interface sa.NewLLMClient takes) instead of the fixture generator, so
// a case reuses the harness.seed method and proc.ProcessFlat against a live model. The gen
// field is nil — there is no fixture to flip — and seed reads only the in-memory S3.
func newRealHarness(cfg pipeConfig, url string) *harness {
	fakeStore := newFakeS3()
	proc := sa.NewSessionAnalysisProcessor(
		dao.NewRawRecordRepo(testDB),
		dao.NewSessionAnalysisRepo(testDB),
		dao.NewSessionAnalysisStateRepo(testDB),
		dao.NewSessionTurnAnalysisRepo(testDB),
		dao.NewSessionPhaseAnalysisRepo(testDB),
		dao.NewSessionRoundTripFaultRepo(testDB),
		dao.NewSecurityFlagRepo(testDB),
		dao.NewHarnessArtifactRepo(testDB),
		dao.NewHarnessArtifactClassificationRepo(testDB),
		fakeStore,
		sa.NewLLMClient(refllmclient.New(url)),
		nil,
		testDB,
		cfg,
	)
	return &harness{proc: proc, s3: fakeStore, cfg: cfg}
}

// TestRealGeminiPipelineE2E gates on SESSIONINTEL_REAL_GEMINI_URL, so it stays out of an ordinary
// CI run (which sets no such var) and runs only when an operator points it at a live reference-llm
// — the same spirit as eval_test.go's runRealGeminiEval switch. Each subtest seeds its own
// UUID org, runs the real pipeline, and asserts only structural completeness of the committed
// rows.
func TestRealGeminiPipelineE2E(t *testing.T) {
	url := os.Getenv("SESSIONINTEL_REAL_GEMINI_URL")
	if url == "" {
		t.Skip("set SESSIONINTEL_REAL_GEMINI_URL to run the real-Gemini E2E")
	}

	// A clean coding session (default body, no ledger signals): only R runs. Proves R
	// judged a live transcript and committed a full session verdict.
	t.Run("clean coding session commits a full R verdict", func(t *testing.T) {
		org, person := uuid.NewString(), uuid.NewString()
		cleanupOrg(t, org)
		h := newRealHarness(defaultConfig(), url)
		rr, _ := h.seed(t, seedOptions{orgID: org, personID: person})

		n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		an := readAnalysis(t, rr)
		require.NotNil(t, an, "R committed a session_analyses row")
		assert.NotNil(t, an.ScoreOverall, "R judged an overall input-quality score")
		assert.NotNil(t, an.GoalAchieved, "R judged the session's goal outcome")
		require.NotNil(t, an.TurnCount, "R materialized the turn count")
		assert.Positive(t, *an.TurnCount, "the judged turn table is non-empty")
	})

	// An evidence-triggering session: a static SKILL_CALL ledger makes E fire after R. Proves
	// E ran against real Gemini — the row carries R's contract AND the EVIDENCE producer's call
	// state reaches the terminal DONE. A broken analyze_evidence contract against a live model
	// would instead error out of ProcessFlat or leave EVIDENCE non-DONE, so the two together
	// prove E completed rather than merely being scheduled.
	t.Run("evidence session runs E to a terminal DONE", func(t *testing.T) {
		org, person := uuid.NewString(), uuid.NewString()
		cleanupOrg(t, org)
		h := newRealHarness(defaultConfig(), url)
		rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: convSignals{skillCalls: 3}})

		n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		an := readAnalysis(t, rr)
		require.NotNil(t, an, "R committed the session row")
		assert.NotNil(t, an.ScoreOverall, "the R contract is present alongside E")

		status, ok := callStatus(t, rr, "EVIDENCE")
		require.True(t, ok, "the EVIDENCE producer recorded a call state")
		assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, status,
			"E ran to completion against real Gemini")
	})

	// A discard-triggering session: user interruptions populate the branch/revert ledger, which
	// gates D. Proves D ran to completion against real Gemini via the DISCARD call state — the
	// same E2E signal the evidence case uses for E. wasted_tokens is NOT asserted here: it is set
	// only when D returns actual discarded ranges, and a generic transcript (interruptions in the
	// aggregate but no reverted work) correctly yields none; the waste-ledger sum itself is unit-
	// tested (TestSumDiscardedTokens). If D did find ranges, the value must be coherent.
	t.Run("discard session runs D to a terminal DONE", func(t *testing.T) {
		org, person := uuid.NewString(), uuid.NewString()
		cleanupOrg(t, org)
		h := newRealHarness(defaultConfig(), url)
		rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, signals: convSignals{userInterruptions: 2}})

		n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		an := readAnalysis(t, rr)
		require.NotNil(t, an, "the session row committed")
		status, ok := callStatus(t, rr, "DISCARD")
		require.True(t, ok, "the DISCARD producer recorded a call state")
		assert.Equal(t, sessionintelligencepb.CallStatus_CALL_STATUS_DONE, status,
			"D ran to completion against real Gemini")
		if an.WastedTokens != nil {
			assert.GreaterOrEqual(t, *an.WastedTokens, int64(0), "a populated waste ledger is non-negative")
		}
	})

	// An oversized session forced onto the windowed read chain (tiny char limit, window 2 +
	// look-ahead 1) over enough turns to exceed the limit. Proves the windowed analyze_read
	// calls stitched into one COMPLETE judged row rather than committing a partial verdict.
	t.Run("oversized session stitches windows into a complete verdict", func(t *testing.T) {
		org, person := uuid.NewString(), uuid.NewString()
		cleanupOrg(t, org)
		cfg := defaultConfig().withChunkCharLimit(50).withChunkWindow(2, 1)
		h := newRealHarness(cfg, url)
		rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, body: mustBody(t, 8)})

		n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		an := readAnalysis(t, rr)
		require.NotNil(t, an, "the chunked read stitched a committed session row")
		assert.NotNil(t, an.ScoreOverall, "the stitched verdict is complete, not partial")
		require.NotNil(t, an.TurnCount, "the stitched turn table materialized a count")
		assert.Positive(t, *an.TurnCount, "the stitched turn table is non-empty")
	})

	// System-prompt caching: an explicit Gemini CachedContent for the analyze_read system prompt
	// (gemini-3.7-flash, the producers' pinned primary) is created on the first call and read on the
	// next, so a repeat call reports a cache hit. Proves explicit-first caching works live
	// against real Gemini. The per-model cache key (a distinct cache per model) and the
	// use_system_prompt_cache kill-switch (which skips the explicit CachedContent) are covered by
	// reference-llm's prompt-cache unit tests.
	t.Run("system-prompt caching hits on a repeat analyze_read (gemini-3.7-flash)", func(t *testing.T) {
		client := refllmclient.New(url)
		ctx := unlimitedCtx(uuid.NewString())
		req := refllmclient.GenerateContentRequest{
			TaskType:             refllmclient.TaskTypeAnalyzeRead,
			OrgID:                "cache-e2e",
			SourceID:             uuid.NewString(),
			Content:              "[user] add a nil guard to collapseSessionWeeklyTrend\n[assistant] edits the file, runs the tests, they pass\n[user] thanks",
			UseSystemPromptCache: true,
		}
		// The first call warms/creates the explicit cache for this system prompt.
		_, err := client.GenerateContent(ctx, req)
		require.NoError(t, err)
		// The repeat call must read the cached system prompt rather than re-sending it.
		resp, err := client.GenerateContent(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp.Usage)
		assert.Equal(t, "gemini-3.7-flash", resp.Model, "the session producers pin gemini-3.7-flash")
		assert.Positive(t, resp.Usage.CacheReadTokens, "the explicit system-prompt cache was hit on the repeat call")
	})
}
