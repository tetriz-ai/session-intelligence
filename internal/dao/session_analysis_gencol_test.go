// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao_test

// Real-DB test for the session_meta_raw STORED generated columns
// (session-analyses-serving-parity design). Only Postgres materializes a GENERATED ALWAYS column,
// so this exercises the real schema loaded from latest.sql (dao-test-schema rule) — a mock cannot
// prove the generation expression. Covers the S3a hoists (mcp_server_names, tool_errors) and the
// three added here (session_type, cost_is_estimated, cost_is_input_only) plus the
// user_message_timestamps StringList hoist.

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/dao"
)

// TestSessionAnalysisRepo_SessionMetaRawGenCols asserts the STORED generated columns materialize
// from session_meta_raw on write and read back through the read-only model fields: the
// mcpServerNames / userMessageTimestamps arrays land in the StringList {"values":[...]} shape,
// toolErrors lands as text, sessionType as text, and the two cost flags as booleans — while an
// absent field (a proto default the protojson blob omits, or a nil blob) yields a NULL column
// (a nil StringList / nil *string / nil *bool), never an error. The cost flags exercise the
// proto3-omits-false NULL-collapse the migration comment documents.
func TestSessionAnalysisRepo_SessionMetaRawGenCols(t *testing.T) {
	if testDB == nil {
		t.Skip("no test DB")
	}
	t.Parallel()

	tests := []struct {
		name         string
		meta         *sessionintelligencepb.UsageMetadata
		wantMCPNames []string // nil => the gen-col must be SQL NULL (nil StringList)
		wantToolErrs *string  // nil => SQL NULL
		wantType     *string  // nil => SQL NULL
		wantCostEst  *bool    // nil => SQL NULL (proto3 omits false)
		wantCostIn   *bool    // nil => SQL NULL
		wantUserTS   []string // nil => SQL NULL
	}{
		{
			name: "populated blob materializes every hoist",
			meta: &sessionintelligencepb.UsageMetadata{
				McpServerNames:        []string{"srv-a", "srv-b"},
				ToolErrors:            3,
				Entrypoint:            "cli",
				SessionType:           "debugging",
				CostIsEstimated:       true,
				CostIsInputOnly:       true,
				UserMessageTimestamps: []string{"2026-05-10T10:00:00Z", "2026-05-10T10:05:00Z"},
			},
			wantMCPNames: []string{"srv-a", "srv-b"},
			wantToolErrs: strPtr("3"),
			wantType:     strPtr("debugging"),
			wantCostEst:  ptr(true),
			wantCostIn:   ptr(true),
			wantUserTS:   []string{"2026-05-10T10:00:00Z", "2026-05-10T10:05:00Z"},
		},
		{
			name: "proto defaults are omitted by protojson, so those hoists are NULL",
			// ToolErrors 0, false cost flags, an empty sessionType and empty arrays are proto3
			// defaults protojson drops, so every dependent gen-col evaluates to SQL NULL. The
			// false cost flags becoming NULL (not false) is the documented NULL-collapse.
			meta:         &sessionintelligencepb.UsageMetadata{Entrypoint: "cli"},
			wantMCPNames: nil,
			wantToolErrs: nil,
			wantType:     nil,
			wantCostEst:  nil,
			wantCostIn:   nil,
			wantUserTS:   nil,
		},
		{
			name: "nil session_meta_raw yields NULL hoists",
			meta: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestTx(t)
			orgID := seedOrg(t, tx)
			person := uuid.NewString()
			rawRecordID := uuid.NewString()
			ctx := loopTestCtx(person)
			repo := dao.NewSessionAnalysisRepo(tx)

			row := newSessionAnalysis(orgID, person, rawRecordID)
			row.SessionMetaRaw = tt.meta
			require.NoError(t, repo.Create(ctx, row))

			got, err := repo.GetByRawRecordID(ctx, rawRecordID)
			require.NoError(t, err)

			assertStringListHoist(t, "mcp_server_names", tt.wantMCPNames, got.McpServerNames)
			assertStringListHoist(t, "user_message_timestamps", tt.wantUserTS, got.UserMessageTimestamps)
			assert.Equal(t, tt.wantToolErrs, got.ToolErrors, "tool_errors")
			assert.Equal(t, tt.wantType, got.SessionType, "session_type")
			assert.Equal(t, tt.wantCostEst, got.CostIsEstimated, "cost_is_estimated")
			assert.Equal(t, tt.wantCostIn, got.CostIsInputOnly, "cost_is_input_only")
		})
	}
}

// assertStringListHoist checks a StringList-shaped jsonb gen-col: nil want => SQL NULL (nil
// proto), else the materialized {"values":[...]} mirrors the source array.
func assertStringListHoist(t *testing.T, col string, want []string, got *sessionintelligencepb.StringList) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got, "%s: an absent source must leave the gen-col NULL", col)
		return
	}
	require.NotNil(t, got, "%s: the hoisted array must materialize", col)
	assert.Equal(t, want, got.GetValues(), "%s must mirror the source array", col)
}
