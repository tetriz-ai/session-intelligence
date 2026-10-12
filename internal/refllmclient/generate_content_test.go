// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient_test

import (
	"encoding/json"
	"strings"
	"testing"

	wpc "github.com/tetriz-ai/session-intelligence/internal/refllmclient"
)

// TestGenerateContentRequest_UseSystemPromptCacheAlwaysOnWire pins that the request always
// serializes use_system_prompt_cache, including an explicit false. reference-llm reads the field
// as a tri-state (absent → per-task default, which is ON for the session-analysis producers), so
// omitempty on this bool would drop a false and silently re-enable caching — the caching
// kill-switch would be inert. Both an explicit false and an explicit true must reach the wire.
func TestGenerateContentRequest_UseSystemPromptCacheAlwaysOnWire(t *testing.T) {
	tests := []struct {
		name string
		flag bool
		want string
	}{
		{name: "explicit false transmits", flag: false, want: `"use_system_prompt_cache":false`},
		{name: "explicit true transmits", flag: true, want: `"use_system_prompt_cache":true`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(wpc.GenerateContentRequest{
				TaskType:             wpc.TaskTypeAnalyzeRead,
				OrgID:                "org-1",
				UseSystemPromptCache: tt.flag,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(body), tt.want) {
				t.Errorf("request body %s does not carry %s", body, tt.want)
			}
		})
	}
}
