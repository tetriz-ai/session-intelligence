// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/session-intelligence/dao/model"
)

// seedChildRawRecord inserts a delegated child raw_record whose metadata the parent's
// AggregateSubagentUsageByParent folds: it must carry the parent's own person_id and
// record_key, a folded child relation type (SUBAGENT), STRONG linkage, and the priced
// tokens/cost the fold sums.
func seedChildRawRecord(t *testing.T, h *harness, orgID, personID, parentRecordKey string, dataDate time.Time, tokens, costUsdMicros int64) {
	t.Helper()
	meta, err := json.Marshal(map[string]string{
		"person_id":          personID,
		"parent_record_key":  parentRecordKey,
		"relation_type":      "SUBAGENT",
		"linkage_confidence": "STRONG",
		"origin_agent_type":  "explorer",
		"tool_use_id":        "toolu_" + uuid.NewString()[:8],
		"tokens":             strconv.FormatInt(tokens, 10),
		"cost_usd_micros":    strconv.FormatInt(costUsdMicros, 10),
	})
	require.NoError(t, err)
	// Stage the child body so the (seated, in-window) child's measured turn grouping can read
	// it — a skipped child now writes its own measured turn rows before MarkSkipped.
	s3Key := "s3://child/" + uuid.NewString()
	body, err := buildConversationBody("IDE_TYPE_CLAUDE_CODE", dataDate, 2)
	require.NoError(t, err)
	h.s3.put(s3Key, body)
	payload, err := json.Marshal(map[string]string{"s3_key": s3Key})
	require.NoError(t, err)
	child := &model.RawRecordModel{
		ID:               uuid.NewString(),
		OrganizationID:   orgID,
		ConnectorID:      sessionConnectorID,
		OperationID:      "conversation_upload",
		DataDate:         dataDate,
		RecordKey:        "child-" + uuid.NewString()[:8],
		Payload:          payload,
		Metadata:         meta,
		FetchedAt:        dataDate.Add(12 * time.Hour),
		ProcessingStatus: model.ProcessingStatusPending,
	}
	require.NoError(t, testDB.Create(child).Error, "seed child raw_record")
}

// TestPipeline_ChildFold_ChildBeforeParent — a delegated child present when the parent runs
// is folded into the parent's measured row: estimated_cost_usd becomes own + Σ children,
// own_estimated_cost_usd keeps the parent's own cost, and subagent_usage carries the child
// breakdown. This is the child-arrives-before-parent ordering.
func TestPipeline_ChildFold_ChildBeforeParent(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	h := newHarness(defaultConfig(), readFixtureClean)
	dataDate := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	parentKey := "parent-" + uuid.NewString()[:8]

	// The child exists and is seen (skipped CHILD) before the parent is processed. Its own
	// tick settles the CHILD skip so the parent's later tick folds it in that first run,
	// rather than leaving a children_seq > folded_children_seq delta for a later re-fold.
	seedChildRawRecord(t, h, org, person, parentKey, dataDate, 1000, 5_000_000)
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)

	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, dataDate: dataDate, recordKey: parentKey})
	n, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	an := readAnalysis(t, rr)
	require.NotNil(t, an)
	require.NotNil(t, an.OwnEstimatedCostUsd, "own cost is stamped")
	require.NotNil(t, an.EstimatedCostUsd, "folded cost is stamped")
	assert.InDelta(t, *an.OwnEstimatedCostUsd+5.0, *an.EstimatedCostUsd, 1e-9,
		"estimated_cost_usd = own + Σ children ($5.00 from the child)")
	require.NotNil(t, an.SubagentUsage, "the child breakdown lands on subagent_usage")
	assert.Equal(t, int32(1), an.SubagentUsage.GetChildCount())
	assert.Equal(t, int64(1000), an.SubagentUsage.GetTotalTokens())
	assert.InDelta(t, 5.0, an.SubagentUsage.GetTotalEstimatedCostUsd(), 1e-9)
}

// TestPipeline_ChildFold_ChildAfterParent — a delegated child that arrives AFTER the parent
// was judged own-only is picked up by the throttled child-refold: the child's sighting bumps
// the parent's children_seq, and once the refold interval has elapsed the parent re-folds
// (zero-LLM, the spine-only path) and estimated_cost_usd rises to own + child
// .
func TestPipeline_ChildFold_ChildAfterParent(t *testing.T) {
	org, person := uuid.NewString(), uuid.NewString()
	cleanupOrg(t, org)
	cfg := defaultConfig()
	cfg.childRefoldMin = 0 // any elapsed time re-admits, so the refold fires on the next tick
	h := newHarness(cfg, readFixtureClean)
	dataDate := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	parentKey := "parent-" + uuid.NewString()[:8]

	// Tick 1: the parent is judged with no children — own-only cost.
	rr, _ := h.seed(t, seedOptions{orgID: org, personID: person, dataDate: dataDate, recordKey: parentKey})
	_, err := h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	before := readAnalysis(t, rr)
	require.NotNil(t, before)
	require.NotNil(t, before.EstimatedCostUsd)
	require.Nil(t, before.SubagentUsage, "no children folded on the first judge")
	ownCost := *before.EstimatedCostUsd

	// The child arrives after the parent was judged.
	seedChildRawRecord(t, h, org, person, parentKey, dataDate, 1000, 5_000_000)

	// Tick 2: the child is skipped (CHILD) and bumps the parent's children_seq fence.
	_, err = h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	st := readState(t, rr)
	require.NotNil(t, st)
	require.Greater(t, st.ChildrenSeq, st.FoldedChildrenSeq, "the child sighting advanced the fence past the folded seq")

	// Tick 3: the child-refold re-admits the parent (throttle elapsed) and re-folds the child.
	_, err = h.proc.ProcessFlat(unlimitedCtx(person), org, 50)
	require.NoError(t, err)
	after := readAnalysis(t, rr)
	require.NotNil(t, after)
	require.NotNil(t, after.EstimatedCostUsd)
	assert.InDelta(t, ownCost+5.0, *after.EstimatedCostUsd, 1e-9, "the late child's cost is re-folded")
	require.NotNil(t, after.SubagentUsage, "the child breakdown is now present")
	assert.Equal(t, int32(1), after.SubagentUsage.GetChildCount())
	require.NotNil(t, after.JudgedAt, "the parent is re-stamped judged and served")
	stAfter := readState(t, rr)
	require.NotNil(t, stAfter)
	assert.Equal(t, stAfter.ChildrenSeq, stAfter.FoldedChildrenSeq, "the refold caught the fence up")
}
