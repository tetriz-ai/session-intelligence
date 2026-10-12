// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
	"github.com/tetriz-ai/session-intelligence/internal/planscoped"
	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
	"github.com/tetriz-ai/session-intelligence/internal/s3"
	sa "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"
	"github.com/tetriz-ai/session-intelligence/internal/testutil"
)

// testDB is the test database leased once for this package (latest.sql).
// Every case here needs it — an unreachable server means zero assertions ran — so
// TestMain gates through MustOpenOrFail (fail, not silent skip, under the CI gate).
var testDB *gorm.DB

func TestMain(m *testing.M) {
	db, cleanup := testutil.MustOpenOrFail("session-analysis pipeline e2e")
	testDB = db
	code := m.Run()
	cleanup()
	os.Exit(code)
}

const sessionConnectorID = sa.SessionConnectorID

// ── The processor's dynamic Config, as a per-case-tunable value ──────────────────────

// pipeConfig implements sessionanalysis.Config with fixed defaults a case can override
// through the with* helpers. The defaults are the shipped v1 stamps: one current
// version of each stamp effective from the epoch, all producers enabled, caching on, a
// 12h reprocess throttle. A knob here is owned by us uniformly or per rollout phase,
// never by the customer.
type pipeConfig struct {
	enabled          bool
	readEnabled      bool
	vocabVersion     int32
	vocabEffective   time.Time
	spineVersion     int32
	reprocessMin     time.Duration
	childRefoldMin   time.Duration
	chunkCharLimit   int
	chunkWindow      int
	chunkLookahead   int
	perRecordTimeout time.Duration
	compatWrite      bool
	detectorEnabled  bool
	cutover          time.Time
}

func defaultConfig() pipeConfig {
	return pipeConfig{
		enabled:          true,
		readEnabled:      true,
		vocabVersion:     1,
		vocabEffective:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		spineVersion:     2,
		reprocessMin:     12 * time.Hour,
		childRefoldMin:   12 * time.Hour,
		chunkCharLimit:   350000,
		chunkWindow:      20,
		chunkLookahead:   6,
		perRecordTimeout: 15 * time.Minute,
	}
}

func (c pipeConfig) withVocab(version int32, effective time.Time) pipeConfig {
	c.vocabVersion, c.vocabEffective = version, effective
	return c
}
func (c pipeConfig) withSpineVersion(v int32) pipeConfig { c.spineVersion = v; return c }
func (c pipeConfig) withReadEnabled(v bool) pipeConfig   { c.readEnabled = v; return c }
func (c pipeConfig) withReprocessMin(d time.Duration) pipeConfig {
	c.reprocessMin = d
	return c
}
func (c pipeConfig) withChunkCharLimit(n int) pipeConfig { c.chunkCharLimit = n; return c }

func (c pipeConfig) withChunkWindow(window, lookahead int) pipeConfig {
	c.chunkWindow, c.chunkLookahead = window, lookahead
	return c
}
func (c pipeConfig) withCutover(t time.Time) pipeConfig { c.cutover = t; return c }
func (c pipeConfig) withDetector(v bool) pipeConfig     { c.detectorEnabled = v; return c }

func (c pipeConfig) Enabled(string) bool                   { return c.enabled }
func (c pipeConfig) BatchSize() int                        { return 50 }
func (c pipeConfig) RecordConcurrency() int                { return 8 }
func (c pipeConfig) GlobalBudgetPerOrgTick() int           { return 200 }
func (c pipeConfig) RetryCap() int32                       { return 3 }
func (c pipeConfig) ReprocessMinInterval() time.Duration   { return c.reprocessMin }
func (c pipeConfig) ChildRefoldMinInterval() time.Duration { return c.childRefoldMin }
func (c pipeConfig) PerRecordTimeout() time.Duration       { return c.perRecordTimeout }
func (c pipeConfig) OutageSignalWindow() time.Duration     { return 10 * time.Minute }
func (c pipeConfig) OutageSignalMinRecords() int           { return 5 }
func (c pipeConfig) ProviderRetryBackoff() time.Duration   { return 30 * time.Minute }
func (c pipeConfig) ChunkCharLimit() int                   { return c.chunkCharLimit }
func (c pipeConfig) ChunkWindowPrompts() int               { return c.chunkWindow }
func (c pipeConfig) ChunkLookaheadPrompts() int            { return c.chunkLookahead }
func (c pipeConfig) VocabularyVersion() int32              { return c.vocabVersion }
func (c pipeConfig) VocabularyEffectiveFrom() time.Time    { return c.vocabEffective }
func (c pipeConfig) SpineVersion() int32                   { return c.spineVersion }
func (c pipeConfig) SpineEffectiveFrom() time.Time         { return time.Time{} }
func (c pipeConfig) ContractVersionRead() int32            { return 1 }
func (c pipeConfig) ContractVersionEvidence() int32        { return 1 }
func (c pipeConfig) ContractVersionDiscard() int32         { return 1 }
func (c pipeConfig) ContractEffectiveFrom() time.Time      { return time.Time{} }

// CutoverDate defaults to zero (no floor), so the live-path cases process every in-window
// date; a case opts into a floor via withCutover.
func (c pipeConfig) CutoverDate(string) time.Time                   { return c.cutover }
func (c pipeConfig) UseSystemPromptCache() bool                     { return true }
func (c pipeConfig) ReadEnabled(string) bool                        { return c.readEnabled }
func (c pipeConfig) EvidenceSecurityFlagPublishEnabled(string) bool { return true }

func (c pipeConfig) CompatWriteEnabled() bool { return c.compatWrite }

// EvidenceEnabled gates the WHOLE E producer (detector + E inference + E writes); the case field is
// kept as detectorEnabled. When on, the harness wires echoDetector, which scores every guard-valid
// pair as a candidate so E's round-trip half has pairs to adjudicate.
func (c pipeConfig) EvidenceEnabled(string) bool { return c.detectorEnabled }
func (c pipeConfig) DetectorTopN() int           { return 50 }
func (c pipeConfig) DetectorConcurrency() int    { return 8 }
func (c pipeConfig) DetectorPairCeiling() int    { return 1000 }

// echoDetector is the harness PAIRS detector: it returns every supplied guard-valid pair as an
// above-threshold candidate (score 1.0), so a session with round-trip pairs routes them all to E.
// Real scoring/thresholding is exercised in the reference-llm detector tests, not here.
type echoDetector struct{}

func (echoDetector) Detect(_ context.Context, _, _ string, pairs []refllmclient.RoundTripPair, topN, ceiling int) (*sa.DetectorResult, error) {
	if ceiling > 0 && len(pairs) > ceiling {
		pairs = pairs[:ceiling]
	}
	candidates := make([]sa.DetectedPair, 0, len(pairs))
	for _, pair := range pairs {
		if topN > 0 && len(candidates) >= topN {
			break
		}
		candidates = append(candidates, sa.DetectedPair{PromptIndex: pair.PromptIndex, Score: 1.0})
	}
	return &sa.DetectorResult{Candidates: candidates}, nil
}

// ── In-memory S3 (the conversation bodies the spine reads) ───────────────────────────

// fakeS3 is an in-memory s3.S3Client: the harness controls both the seeded body and the
// key, so the spine's ReadObject resolves deterministically with no LocalStack. The
// processor only ever calls ReadObject; the embedded nil interface satisfies the rest of
// the surface and panics loudly if the processor ever grows a new S3 call this fake must
// then implement.
type fakeS3 struct {
	s3.S3Client
	mu      sync.Mutex
	objects map[string][]byte
	reads   int
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = body
}

func (f *fakeS3) ReadObject(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	body, ok := f.objects[path]
	if !ok {
		return nil, fmt.Errorf("fakeS3: no object at %q", path)
	}
	return body, nil
}

// ── The LLM seam (a fixture ContentGenerator behind the REAL LLMClient) ──────────────

// fixtureGenerator is a refllmclient.ContentGenerator that returns W's recorded
// analyze_read response, so the REAL sessionanalysis.LLMClient parser runs end-to-end
// with no live model. readResult is the analyze_read Result JSON (W's AnalyzeReadResponse
// shape); err, when set, simulates a provider failure (the post-spine-failure case). It
// counts analyze_read calls so a case can assert the zero-LLM spine-only path.
type fixtureGenerator struct {
	mu             sync.Mutex
	readResult     string
	readByChunk    map[int32]string // per-window analyze_read Result, keyed by chunk_index (chunked path)
	failChunk      *int32           // when set, the analyze_read for this chunk_index fails (mid-chain abort)
	discardResult  string           // the analyze_discard Result JSON; "" answers with an empty envelope
	evidenceResult string           // the analyze_evidence Result JSON; "" answers with an empty envelope
	err            error
	readCalls      int
	evidenceCalls  int
	discardCalls   int
}

func (g *fixtureGenerator) GenerateContent(_ context.Context, req refllmclient.GenerateContentRequest) (*refllmclient.GenerateContentResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.TaskType == refllmclient.TaskTypeAnalyzeRead {
		g.readCalls++
		if g.err != nil {
			return nil, g.err
		}
		chunk := chunkIndexOf(req)
		if g.failChunk != nil && *g.failChunk == chunk {
			return nil, fmt.Errorf("fixture: forced analyze_read failure on chunk %d", chunk)
		}
		result := g.readResult
		if perChunk, ok := g.readByChunk[chunk]; ok {
			result = perChunk
		}
		return &refllmclient.GenerateContentResponse{
			Result:   result,
			TaskType: req.TaskType,
			Model:    "gemini-2.5-flash",
			Provider: "gemini",
			Usage:    &refllmclient.TokenUsage{PromptTokens: 1200, CompletionTokens: 400, CacheReadTokens: 900},
		}, nil
	}
	// E runs after R when its fire condition holds; return the seeded evidence
	// fixture, or an empty envelope (E fired but adjudicated nothing).
	if req.TaskType == refllmclient.TaskTypeAnalyzeEvidence {
		g.evidenceCalls++
		result := g.evidenceResult
		if result == "" {
			result = "{}"
		}
		return &refllmclient.GenerateContentResponse{
			Result:   result,
			TaskType: req.TaskType,
			Model:    "gemini-2.5-flash",
			Provider: "gemini",
			Usage:    &refllmclient.TokenUsage{PromptTokens: 500, CompletionTokens: 120},
		}, nil
	}
	// D runs last (the static revert/abandon ledger fired); return the seeded discard
	// fixture, or an empty envelope (the common "nothing discarded" answer).
	// The call is counted whether or not a fixture is seeded, so a case can assert D's LLM
	// call budget (the static-signal gating: no ledger evidence → D never reaches here).
	if req.TaskType == refllmclient.TaskTypeAnalyzeDiscard {
		g.discardCalls++
		result := g.discardResult
		if result == "" {
			result = "{}"
		}
		return &refllmclient.GenerateContentResponse{
			Result:   result,
			TaskType: req.TaskType,
			Model:    "gemini-2.5-flash",
			Provider: "gemini",
			Usage:    &refllmclient.TokenUsage{PromptTokens: 300, CompletionTokens: 40},
		}, nil
	}
	return &refllmclient.GenerateContentResponse{Result: "{}", TaskType: req.TaskType}, nil
}

// chunkIndexOf reads the chunk_index the client threaded into the session context, so the
// fixture can answer a windowed read chain per window. Absent (single-chunk fast path) is 0.
func chunkIndexOf(req refllmclient.GenerateContentRequest) int32 {
	if req.SessionContext == nil {
		return 0
	}
	if raw, ok := req.SessionContext["chunk_index"]; ok {
		if idx, ok := raw.(int32); ok {
			return idx
		}
	}
	return 0
}

func (g *fixtureGenerator) reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readCalls
}

func (g *fixtureGenerator) evidences() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.evidenceCalls
}

func (g *fixtureGenerator) discards() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.discardCalls
}

// ── The processor under test ─────────────────────────────────────────────────────────

// harness bundles the REAL processor with the fixture LLM generator and in-memory S3 it
// was built over, so a case can flip the fixture (inject a failure, count reads) and
// read the same org's committed rows back.
type harness struct {
	proc *sa.SessionAnalysisProcessor
	s3   *fakeS3
	gen  *fixtureGenerator
	cfg  pipeConfig
}

// newHarness builds the processor with real DAOs over the leased test DB, the fixture
// LLM generator, and in-memory S3. The generator returns readResult for every
// analyze_read call.
func newHarness(cfg pipeConfig, readResult string) *harness {
	gen := &fixtureGenerator{readResult: readResult}
	s3 := newFakeS3()
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
		s3,
		sa.NewLLMClient(gen),
		echoDetector{},
		testDB,
		cfg,
	)
	return &harness{proc: proc, s3: s3, gen: gen, cfg: cfg}
}

// newHarnessProc rebuilds the processor with a new config over the SAME in-memory S3 and
// fixture generator, so a second-pass case (a spine or vocabulary bump) reuses the first
// pass's staged bodies and analyze_read call counter.
func newHarnessProc(h *harness, cfg pipeConfig) *sa.SessionAnalysisProcessor {
	return sa.NewSessionAnalysisProcessor(
		dao.NewRawRecordRepo(testDB),
		dao.NewSessionAnalysisRepo(testDB),
		dao.NewSessionAnalysisStateRepo(testDB),
		dao.NewSessionTurnAnalysisRepo(testDB),
		dao.NewSessionPhaseAnalysisRepo(testDB),
		dao.NewSessionRoundTripFaultRepo(testDB),
		dao.NewSecurityFlagRepo(testDB),
		dao.NewHarnessArtifactRepo(testDB),
		dao.NewHarnessArtifactClassificationRepo(testDB),
		h.s3,
		sa.NewLLMClient(h.gen),
		echoDetector{},
		testDB,
		cfg,
	)
}

// ── Seeding ──────────────────────────────────────────────────────────────────────────

// seedOptions configures one seeded session session (raw_record + its S3 body).
type seedOptions struct {
	orgID           string
	personID        string
	dataDate        time.Time
	ideType         string // protojson IdeType name, e.g. "IDE_TYPE_CLAUDE_CODE"
	contentSha      string // payload content_sha256 (drives the content-change signal)
	body            []byte // the conversation protojson body; a default is built if nil
	repoName        string
	relationType    string // a child lineage type (lineage.RelationTypeSubagent, …) makes it a CHILD; PRIMARY/empty is top-level
	parentRecordKey string
	recordKey       string
	signals         convSignals // static E/D fire evidence stamped onto the default body
}

// convSignals are the static ledger signals the default conversation body carries, so a
// pipeline case can make E fire (skillCalls → the artifact ledger) or D fire
// (userInterruptions → the branch/revert ledger) without hand-writing a body.
type convSignals struct {
	userInterruptions int
	skillCalls        int
	// permissionModes, when set, stamps metadata.permission_mode onto each user turn by index
	// (index i → the i-th user prompt), so an e2e can drive a mid-session HITL-mode switch and
	// assert the per-phase hitl_gate forward-fill. Empty entries / short slices leave that turn
	// unstamped (the backend forward-fills).
	permissionModes []string
	// contents, when set, overrides the default user-turn text by index, so an e2e can shape a
	// clearly multi-phase transcript (a planning segment then an implementation segment) that R is
	// likely to split into distinct phases.
	contents []string
}

// seed inserts one raw_record and stages its S3 body into the harness's in-memory S3,
// returning the raw_record id and its S3 key. The raw_record is written on the leased
// conn; the single-tenant build applies no row-level security, so the processor reads it
// back with no tenant scope to satisfy.
func (h *harness) seed(t *testing.T, opts seedOptions) (rawRecordID, s3Key string) {
	t.Helper()
	a := require.New(t)
	if opts.ideType == "" {
		opts.ideType = "IDE_TYPE_CLAUDE_CODE"
	}
	if opts.dataDate.IsZero() {
		opts.dataDate = time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	}
	if opts.recordKey == "" {
		opts.recordKey = "sess-" + uuid.NewString()[:8]
	}
	body := opts.body
	if body == nil {
		var err error
		body, err = buildConversationBody(opts.ideType, opts.dataDate, 2, opts.signals)
		a.NoError(err, "build conversation body")
	}
	s3Key = "s3://body/" + uuid.NewString()
	h.s3.put(s3Key, body)

	payload, err := json.Marshal(map[string]string{"s3_key": s3Key, "content_sha256": opts.contentSha})
	a.NoError(err)
	meta := map[string]string{"person_id": opts.personID}
	if opts.repoName != "" {
		meta["repo_name"] = opts.repoName
	}
	if opts.relationType != "" {
		meta["relation_type"] = opts.relationType
	}
	if opts.parentRecordKey != "" {
		meta["parent_record_key"] = opts.parentRecordKey
	}
	metaJSON, err := json.Marshal(meta)
	a.NoError(err)

	record := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   opts.orgID,
		ConnectorID:      sessionConnectorID,
		OperationID:      "conversation_upload",
		DataDate:         opts.dataDate,
		RecordKey:        opts.recordKey,
		Payload:          payload,
		Metadata:         metaJSON,
		FetchedAt:        opts.dataDate.Add(12 * time.Hour),
		ProcessingStatus: model.ProcessingStatusPending,
	}
	a.NoError(testDB.Create(record).Error, "seed raw_record")
	return record.ID, s3Key
}

// buildConversationBody renders a minimal valid Conversation protojson body the spine's
// sessionintelligence.ParseConversation accepts: an IDE, a session id, and promptCount user/assistant
// pairs with token metadata and a start timestamp (so conversation_started_at resolves).
func buildConversationBody(ideType string, start time.Time, promptCount int, signals ...convSignals) ([]byte, error) {
	prompts := make([]map[string]any, 0, promptCount*2)
	for i := 0; i < promptCount; i++ {
		userMeta := map[string]any{"model": "claude-sonnet-4-6", "inputTokens": "120", "outputTokens": "0"}
		userContent := fmt.Sprintf("Implement step %d of the parser and add a test.", i)
		if len(signals) > 0 {
			if i < len(signals[0].permissionModes) && signals[0].permissionModes[i] != "" {
				userMeta["permission_mode"] = signals[0].permissionModes[i]
			}
			if i < len(signals[0].contents) && signals[0].contents[i] != "" {
				userContent = signals[0].contents[i]
			}
		}
		prompts = append(prompts,
			map[string]any{
				"role":      "ROLE_USER",
				"content":   userContent,
				"timestamp": start.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339),
				"metadata":  userMeta,
			},
			map[string]any{
				"role":      "ROLE_ASSISTANT",
				"content":   "Done — here is the implementation and a table-driven test.",
				"timestamp": start.Add(time.Duration(i)*time.Minute + 30*time.Second).UTC().Format(time.RFC3339),
				"metadata":  map[string]any{"model": "claude-sonnet-4-6", "inputTokens": "0", "outputTokens": "180"},
			},
		)
	}
	conv := map[string]any{
		"ideType":     ideType,
		"sessionId":   "sess-" + uuid.NewString()[:8],
		"projectName": "pipeline-e2e",
		"prompts":     prompts,
	}
	if len(signals) > 0 {
		applyConvSignals(conv, signals[0], start)
	}
	return json.Marshal(conv)
}

// applyConvSignals stamps the static E/D ledger evidence onto a conversation body: a
// SessionMeta.user_interruptions count feeds the branch/revert ledger (fires D), and
// SKILL_CALL session events feed the artifact ledger (fires E).
func applyConvSignals(conv map[string]any, signals convSignals, start time.Time) {
	if signals.userInterruptions > 0 {
		conv["sessionMeta"] = map[string]any{"userInterruptions": signals.userInterruptions}
	}
	if signals.skillCalls > 0 {
		events := make([]map[string]any, 0, signals.skillCalls)
		for i := 0; i < signals.skillCalls; i++ {
			events = append(events, map[string]any{
				"sequence":      fmt.Sprintf("%d", i+1),
				"occurredAt":    start.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339),
				"kind":          "SESSION_EVENT_KIND_SKILL_CALL",
				"operationName": fmt.Sprintf("skill-%d", i),
				"outcome":       "SESSION_EVENT_OUTCOME_SUCCEEDED",
			})
		}
		conv["sessionEvents"] = events
	}
}

// ── Driving context + read-back helpers ──────────────────────────────────────────────

// driveCtx builds the resolved-plan context ProcessFlat's fail-closed preflight requires:
// a seat set (seatedPersonIDs), a generous data window anchored on orgCreatedAt, and the
// window length in days. A case exercising OUT_OF_WINDOW passes a short window + a recent
// anchor; NO_SEAT passes a seat set that excludes the record's person.
func driveCtx(seatedPersonIDs []string, dataWindowDays int, orgCreatedAt time.Time) context.Context {
	cfg := planscoped.Config{
		Plan:           commonv1.Plan_PLAN_TRIAL,
		State:          commonv1.PlanState_PLAN_STATE_ACTIVE,
		SeatsLimit:     100,
		DataWindowDays: dataWindowDays,
		OrgCreatedAt:   orgCreatedAt,
	}
	ctx := planscoped.WithConfigForTest(context.Background(), cfg)
	return planscoped.WithSeats(ctx, planscoped.SeatsContext{PersonIDs: seatedPersonIDs})
}

// unlimitedCtx is the common driving context: the record's person is seated and the data
// window is effectively unbounded, so only the case under test's own gate can fire.
func unlimitedCtx(personID string) context.Context {
	return driveCtx([]string{personID}, 3650, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
}

// cleanupOrg deletes every row this suite writes for an org, in FK-safe order, so a fresh
// UUID org never collides and the leased pool DB is left clean.
func cleanupOrg(t *testing.T, orgID string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tbl := range []string{
			"security_flags", "session_turn_analyses", "session_phase_analyses",
			"session_analyses", "session_analysis_state", "raw_records",
		} {
			testDB.Exec("DELETE FROM "+tbl+" WHERE organization_id = ?", orgID)
		}
	})
}

// readState returns the (latest, non-superseded) state row for a raw record, or nil.
func readState(t *testing.T, rawRecordID string) *model.SessionAnalysisState {
	t.Helper()
	var states []model.SessionAnalysisState
	require.NoError(t, testDB.Where("raw_record_id = ?", rawRecordID).
		Order("created_at DESC").Find(&states).Error)
	if len(states) == 0 {
		return nil
	}
	return &states[0]
}

// allStates returns every state generation for a raw record (a vocab bump coexists two).
func allStates(t *testing.T, rawRecordID string) []model.SessionAnalysisState {
	t.Helper()
	var states []model.SessionAnalysisState
	require.NoError(t, testDB.Unscoped().Where("raw_record_id = ?", rawRecordID).
		Order("created_at ASC").Find(&states).Error)
	return states
}

// readAnalysis returns the (live) content row for a raw record, or nil.
func readAnalysis(t *testing.T, rawRecordID string) *model.SessionAnalysis {
	t.Helper()
	var rows []model.SessionAnalysis
	require.NoError(t, testDB.Where("raw_record_id = ?", rawRecordID).Find(&rows).Error)
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

func countRows(t *testing.T, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, testDB.Table(table).Where(where, args...).Count(&n).Error)
	return n
}
