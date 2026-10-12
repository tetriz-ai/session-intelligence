// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

// The turn table's jsonb columns are read back by STORED generated columns that
// index into the protojson blob by lowerCamel key (e.g. token_breakdown->>'inputTokens').
// gen_sql serializes the typed proto with protojson, which lowerCamels field
// names — so these keys are a wire contract between the serializer and the DDL.
// A field rename or a snake_case regression would silently NULL every generated
// column, so pin the exact keys here.
func TestTurnStorageProtos_SerializeLowerCamelKeys(t *testing.T) {
	tests := []struct {
		name     string
		marshal  func() ([]byte, error)
		wantKeys []string
		wantVals []string
	}{
		{
			name: "AnalysisTokenBreakdown — the four summed keys the generated columns read",
			marshal: func() ([]byte, error) {
				return protojson.Marshal(&sessionintelligencepb.AnalysisTokenBreakdown{
					InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2,
				})
			},
			wantKeys: []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens"},
		},
		{
			name: "FileTouchList — read/edited touch kind, lowerCamel key + enum name",
			marshal: func() ([]byte, error) {
				return protojson.Marshal(&sessionintelligencepb.FileTouchList{Items: []*sessionintelligencepb.FileTouch{
					{Path: "a/b.go", TouchKind: sessionintelligencepb.TouchKind_TOUCH_KIND_EDITED},
				}})
			},
			wantKeys: []string{"items", "path", "touchKind"},
			wantVals: []string{"TOUCH_KIND_EDITED"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := tt.marshal()
			require.NoError(t, err)
			s := string(b)
			for _, k := range tt.wantKeys {
				require.Contains(t, s, `"`+k+`"`, "expected lowerCamel key %q in %s", k, s)
			}
			for _, v := range tt.wantVals {
				require.Contains(t, s, v, "expected value %q in %s", v, s)
			}
		})
	}
}

// A PromptMetadata payload predating token_fidelity must decode to the honest
// UNSPECIFIED, never a measured-looking tier — the extractor stamps it, so an
// old client leaves it unset.
func TestPromptMetadata_MissingTokenFidelityIsUnspecified(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "no token_fidelity key", payload: `{"model":"m","inputTokens":10}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var md sessionintelligencepb.PromptMetadata
			require.NoError(t, protojson.Unmarshal([]byte(tt.payload), &md))
			require.Equal(t, sessionintelligencepb.TokenFidelity_TOKEN_FIDELITY_UNSPECIFIED, md.GetTokenFidelity())
		})
	}
}
