// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"context"
	"math/bits"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// TestBuildMeasuredUsage_FoldsSessionSignals is the regression guard for the empty-session_source
// bug: the spine built usage with ExtractUsageMetadata ALONE, which leaves session_source unset, so
// surface resolved to NULL on every turn row in prod. It exercises the real spine seam
// (buildMeasuredUsage) from a Conversation — NOT a hand-fed usage, which is exactly how the earlier
// fixture tests missed this — and asserts the SessionMeta signals fold through and surface resolves.
func TestBuildMeasuredUsage_FoldsSessionSignals(t *testing.T) {
	t.Parallel()
	conv := &sessionintelligencepb.Conversation{
		IdeType:     sessionintelligencepb.IdeType_IDE_TYPE_CLAUDE_CODE,
		SessionMeta: &sessionintelligencepb.SessionMeta{Source: "cli", SessionKind: "interactive", UserInterruptions: 2},
	}
	usage := buildMeasuredUsage(context.Background(), "org", "rr", conv, &sessionintelligencepb.ConversationMetrics{}, "")
	require.NotNil(t, usage)
	assert.Equal(t, "cli", usage.GetSessionSource(), "session_meta.source must fold onto usage.session_source")
	assert.Equal(t, "interactive", usage.GetSessionKind(), "session_meta.session_kind must fold too")
	assert.Equal(t, int32(2), usage.GetUserInterruptions(), "session-grain signals fold as well (session parity)")
	surface := normalizeSurface(usage.GetSessionSource())
	require.NotNil(t, surface, "surface must resolve from the folded source, not NULL")
	assert.Equal(t, sessionintelligencepb.SessionSurface_SESSION_SURFACE_CLI, *surface)
	// buildMeasuredUsage runs the full assembly through ComputeNormalizedFields (the session
	// pipeline defers CNF to after its LLM call). IsActiveSeat is CNF's unconditional stamp, so its
	// presence proves the seam runs CNF — the normalized effective_* / mcp_server_count / delegation
	// fields the measured columns read are populated pre-LLM.
	assert.True(t, usage.GetIsActiveSeat(), "ComputeNormalizedFields must run in the spine seam")
}

// TestApplyMeasuredColumns_WritesUsageBlobAndFlats pins the writer wiring: applyMeasuredColumns
// stores the verbatim usage as session_meta_raw (the source the STORED gen-cols + entrypoint index
// derive from) and maps the seat/plan identity, languages and friction counts onto their flat
// columns. Without the blob write session_meta_raw stays NULL and every hoist is inert (the finding
// this slice closes). It also proves plan_tier_origin resolves (a silent always-nil vendorEnum
// mapping would fail the REPORTED assertion) and that the nullable flats stay NULL on absent input
// rather than storing a wrong value (a $0 seat price).
func TestApplyMeasuredColumns_WritesUsageBlobAndFlats(t *testing.T) {
	t.Parallel()

	t.Run("populated usage maps every flat and stores the blob", func(t *testing.T) {
		usage := &sessionintelligencepb.UsageMetadata{
			SessionId:             "sess-9",
			PlanTier:              "team",
			PlanTierSource:        "cursor_vscdb",
			SeatMonthlyPriceUsd:   40,
			HasExtraUsageEnabled:  true,
			AccountUuid:           "acct-1",
			LoginEmail:            "u@example.com",
			Languages:             []string{"Go", "TypeScript"},
			FrictionCounts:        map[string]int32{"wrong_approach": 2},
			UserMessageTimestamps: []string{"2026-05-10T10:00:00Z"},
		}
		analysis := &model.SessionAnalysis{}
		// conversationID rides the spineResult (parsed from the S3 key upstream); session_id rides the
		// usage blob. Both are mapped here — the wiring guarded against the same silent-no-op class.
		applyMeasuredColumns(analysis, &spineResult{usage: usage, conversationID: "conv-9", metrics: &sessionintelligencepb.ConversationMetrics{}})

		require.Same(t, usage, analysis.SessionMetaRaw, "the verbatim usage blob is stored as session_meta_raw")
		require.NotNil(t, analysis.SessionID, "session_id maps from usage.session_id")
		assert.Equal(t, "sess-9", *analysis.SessionID)
		require.NotNil(t, analysis.ConversationID, "conversation_id maps from the spine's parsed S3 key")
		assert.Equal(t, "conv-9", *analysis.ConversationID)
		require.NotNil(t, analysis.PlanTier)
		assert.Equal(t, "team", *analysis.PlanTier)
		assert.Equal(t, "cursor_vscdb", *analysis.PlanTierSource)
		require.NotNil(t, analysis.PlanTierOrigin, "plan_tier_origin must resolve from plan_tier_source")
		assert.Equal(t, sessionintelligencepb.PlanTierOrigin_PLAN_TIER_ORIGIN_REPORTED, *analysis.PlanTierOrigin)
		require.NotNil(t, analysis.SeatMonthlyPriceUsd)
		assert.Equal(t, 40.0, *analysis.SeatMonthlyPriceUsd)
		require.NotNil(t, analysis.HasExtraUsageEnabled)
		assert.True(t, *analysis.HasExtraUsageEnabled)
		assert.Equal(t, "acct-1", *analysis.AccountUUID)
		assert.Equal(t, "u@example.com", *analysis.LoginEmail)
		require.NotNil(t, analysis.Languages)
		assert.Equal(t, []string{"Go", "TypeScript"}, analysis.Languages.GetValues())
		require.NotNil(t, analysis.FrictionCounts)
		assert.Equal(t, int32(2), analysis.FrictionCounts.GetCounts()["wrong_approach"])
	})

	t.Run("absent inputs leave the nullable flats NULL", func(t *testing.T) {
		usage := &sessionintelligencepb.UsageMetadata{
			SeatMonthlyPriceUsd: 0, // no resolved list price → NULL, not $0
			// Languages / FrictionCounts empty → nil, not an empty proto.
		}
		analysis := &model.SessionAnalysis{}
		// No session_id in the blob and no conversationID on the spine (a malformed S3 key leaves it
		// empty upstream) — both must land NULL, never an empty string.
		applyMeasuredColumns(analysis, &spineResult{usage: usage, metrics: &sessionintelligencepb.ConversationMetrics{}})

		assert.Nil(t, analysis.SeatMonthlyPriceUsd, "seat_monthly_price_usd 0 must be NULL, not $0")
		assert.Nil(t, analysis.Languages, "empty languages must be nil")
		assert.Nil(t, analysis.FrictionCounts, "empty friction_counts must be nil")
		assert.Nil(t, analysis.SessionID, "absent session_id must be NULL, not an empty string")
		assert.Nil(t, analysis.ConversationID, "a malformed/absent S3-key conversation_id must be NULL")
	})
}

// TestProducerBitsRegistryConsistent guards the producerBits registry that finalize/producerBit
// derive the flag_skipped_producers / pending_data_producers masks from: it must cover exactly
// the producer-key constants, and every bit must be a distinct power of two. A producer added
// without a registry entry would silently miss its mask bit (finalize skips a 0 bit), so a new
// producer must extend both the expected set here and the registry.
func TestProducerBitsRegistryConsistent(t *testing.T) {
	want := map[string]int32{
		producerRead:     bitRead,
		producerEvidence: bitEvidence,
		producerDiscard:  bitDiscard,
	}
	assert.Equal(t, want, producerBits, "producerBits must cover exactly the producer keys")

	seen := map[int32]string{}
	for key, bit := range producerBits {
		assert.NotZero(t, bit, "producer %q must have a non-zero bit", key)
		assert.Equal(t, 1, bits.OnesCount32(uint32(bit)), "producer %q bit must be a single power of two", key)
		assert.Equalf(t, bit, producerBit(key), "producerBit(%q) must return the registry bit", key)
		if other, dup := seen[bit]; dup {
			t.Fatalf("producers %q and %q share bit %d", key, other, bit)
		}
		seen[bit] = key
	}
	assert.Zero(t, producerBit("NOT_A_PRODUCER"), "an unknown key maps to 0")
}

// TestSpineOnlyReprocess is the pure decision behind the zero-LLM path: a spine-only reprocess
// requires a prior JUDGED generation whose READ input hash still matches AND both tail producers
// (E, D) settled — a per-producer contract bump (category 4), or the re-admit of a prior
// generation's flag-skipped E/D (category 9), must force a full re-run rather than being silently
// skipped by the R-keyed path. E and D carry no enable flag, so their settledness turns only on
// their recorded call state and contract version.
func TestSpineOnlyReprocess(t *testing.T) {
	readHash := ReadInputHash("sha1", 1)
	const (
		flagSkipped = sessionintelligencepb.CallStatus_CALL_STATUS_FLAG_SKIPPED
		done        = sessionintelligencepb.CallStatus_CALL_STATUS_DONE
		judged      = sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_JUDGED
		spineDone   = sessionintelligencepb.SessionAnalysisStatus_SESSION_ANALYSIS_STATUS_SPINE_DONE
	)
	settled := func() *sessionintelligencepb.CallState {
		return &sessionintelligencepb.CallState{Status: done, ContractVersion: 1}
	}
	// prior builds a JUDGED state whose R carries rHash + rStatus, plus the E and D tail entries
	// (nil for an absent one). E and D are the two producers whose settledness spineOnlyReprocess checks.
	prior := func(rHash string, rStatus sessionintelligencepb.CallStatus, e, d *sessionintelligencepb.CallState) *model.SessionAnalysisState {
		calls := map[string]*sessionintelligencepb.CallState{producerRead: {InputHash: rHash, Status: rStatus, ContractVersion: 1}}
		if e != nil {
			calls[producerEvidence] = e
		}
		if d != nil {
			calls[producerDiscard] = d
		}
		return &model.SessionAnalysisState{Status: judged, CallState: &sessionintelligencepb.CallStateMap{Calls: calls}}
	}
	tests := []struct {
		name                 string
		state                *model.SessionAnalysisState
		readEnabled          bool
		eContract, dContract int32
		want                 bool
	}{
		{name: "nil prior", state: nil, readEnabled: true, want: false},
		{name: "not yet judged", state: &model.SessionAnalysisState{Status: spineDone, CallState: &sessionintelligencepb.CallStateMap{Calls: map[string]*sessionintelligencepb.CallState{producerRead: {InputHash: readHash, Status: done}}}}, readEnabled: true, want: false},
		{name: "R hash changed", state: prior("other", done, settled(), settled()), readEnabled: true, eContract: 1, dContract: 1, want: false},
		{name: "R done, E+D settled → spine-only", state: prior(readHash, done, settled(), settled()), readEnabled: true, eContract: 1, dContract: 1, want: true},
		{name: "R was flag-skipped, R now enabled → full re-run (no verdict to carry)", state: prior(readHash, flagSkipped, settled(), settled()), readEnabled: true, eContract: 1, dContract: 1, want: false},
		{name: "R flag-skipped but R still dark, tail settled → spine-only (R not forced)", state: prior(readHash, flagSkipped, settled(), settled()), readEnabled: false, eContract: 1, dContract: 1, want: true},
		{name: "E flag-skipped in prior → full re-run (category-9 backfill)", state: prior(readHash, done, &sessionintelligencepb.CallState{Status: flagSkipped, ContractVersion: 1}, settled()), readEnabled: true, eContract: 1, dContract: 1, want: false},
		{name: "E entry absent → full re-run", state: prior(readHash, done, nil, settled()), readEnabled: true, eContract: 1, dContract: 1, want: false},
		{name: "E contract behind → full re-run", state: prior(readHash, done, settled(), settled()), readEnabled: true, eContract: 2, dContract: 1, want: false},
		{name: "D flag-skipped in prior → full re-run (category-9 backfill)", state: prior(readHash, done, settled(), &sessionintelligencepb.CallState{Status: flagSkipped, ContractVersion: 1}), readEnabled: true, eContract: 1, dContract: 1, want: false},
		{name: "D contract behind → full re-run", state: prior(readHash, done, settled(), &sessionintelligencepb.CallState{Status: done, ContractVersion: 2}), readEnabled: true, eContract: 1, dContract: 3, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, spineOnlyReprocess(tt.state, readHash, tt.readEnabled, tt.eContract, tt.dContract))
		})
	}
}

// producerFlagsConfig is a minimal Config that answers the R (master) gate and the E gate — any
// other method panics, which is fine because enabledProducerBits reads only ReadEnabled and
// EvidenceEnabled (R and D ride the master gate; E carries its own per-org EvidenceEnabled gate).
type producerFlagsConfig struct {
	Config
	read     bool
	evidence bool
}

func (c producerFlagsConfig) ReadEnabled(string) bool     { return c.read }
func (c producerFlagsConfig) EvidenceEnabled(string) bool { return c.evidence }

// TestEnabledProducerBits guards the category-9 re-admit mask: R and D ride the master
// SessionAnalysisEnabled gate (ReadEnabled) while E rides its own EvidenceEnabled gate. A
// master-off org's every bit must be absent (its flag-skipped rows must not churn re-admission);
// a master-on/evidence-off org enables R and D but NOT E (so E stays FLAG_SKIPPED and only
// re-admits when the E flag turns on); a master-on/evidence-on org enables all three.
func TestEnabledProducerBits(t *testing.T) {
	tests := []struct {
		name     string
		read     bool
		evidence bool
		want     int32
	}{
		{name: "master on + evidence on → all three bits", read: true, evidence: true, want: bitRead | bitEvidence | bitDiscard},
		{name: "master on + evidence off → R and D, no E bit", read: true, evidence: false, want: bitRead | bitDiscard},
		{name: "master off → no bits (no cat-9 re-admit)", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := producerFlagsConfig{read: tt.read, evidence: tt.evidence}
			assert.Equal(t, tt.want, enabledProducerBits(cfg, "org"))
		})
	}
}
