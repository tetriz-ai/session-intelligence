// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
)

func pairUserTurn(content string) *sessionintelligencepb.Prompt {
	return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_USER, Content: content, Metadata: &sessionintelligencepb.PromptMetadata{}}
}

func pairAsstTurn(content string) *sessionintelligencepb.Prompt {
	return &sessionintelligencepb.Prompt{Role: sessionintelligencepb.Role_ROLE_ASSISTANT, Content: content, Metadata: &sessionintelligencepb.PromptMetadata{}}
}

func pairSpine(prompts ...*sessionintelligencepb.Prompt) *spineResult {
	return &spineResult{prompts: prompts, usage: &sessionintelligencepb.UsageMetadata{}}
}

// TestEnumerateGuardValidPairs_DropReasons pins that each of the five validity-guard rejections
// is counted distinctly and drops the pair, and that a clean session yields the guard-valid
// P/A/Q triples keyed by P's raw whole-session turn ordinal.
func TestEnumerateGuardValidPairs_DropReasons(t *testing.T) {
	tests := []struct {
		name      string
		prompts   []*sessionintelligencepb.Prompt
		wantPairs int
		wantDrops guardDrops
	}{
		{
			name:      "no assistant turn between P and Q → no_agent_response",
			prompts:   []*sessionintelligencepb.Prompt{pairUserTurn("p"), pairUserTurn("q")},
			wantPairs: 0,
			wantDrops: guardDrops{noAgentResponse: 1},
		},
		{
			name:      "P empty after trim → p_empty",
			prompts:   []*sessionintelligencepb.Prompt{pairUserTurn("   "), pairAsstTurn("r"), pairUserTurn("q")},
			wantPairs: 0,
			wantDrops: guardDrops{pEmpty: 1},
		},
		{
			name:      "Q empty after trim → q_empty",
			prompts:   []*sessionintelligencepb.Prompt{pairUserTurn("p"), pairAsstTurn("r"), pairUserTurn("   ")},
			wantPairs: 0,
			wantDrops: guardDrops{qEmpty: 1},
		},
		{
			name:      "Q byte-identical to P → q_identical_to_p",
			prompts:   []*sessionintelligencepb.Prompt{pairUserTurn("same"), pairAsstTurn("r"), pairUserTurn("same")},
			wantPairs: 0,
			wantDrops: guardDrops{qIdenticalToP: 1},
		},
		{
			name: "repeated (P,Q) in one session → duplicate_pair",
			prompts: []*sessionintelligencepb.Prompt{
				pairUserTurn("a"), pairAsstTurn("r"),
				pairUserTurn("b"), pairAsstTurn("r"),
				pairUserTurn("a"), pairAsstTurn("r"),
				pairUserTurn("b"),
			},
			wantPairs: 2, // (a,b) and (b,a); the third (a,b) is the duplicate
			wantDrops: guardDrops{duplicatePair: 1},
		},
		{
			name: "clean session → guard-valid pairs, no drops",
			prompts: []*sessionintelligencepb.Prompt{
				pairUserTurn("first ask"), pairAsstTurn("response one"),
				pairUserTurn("second ask"), pairAsstTurn("response two"),
				pairUserTurn("third ask"),
			},
			wantPairs: 2,
			wantDrops: guardDrops{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pairs, drops := enumerateGuardValidPairs("org-1", "raw-1", pairSpine(tt.prompts...))
			assert.Len(t, pairs, tt.wantPairs)
			assert.Equal(t, tt.wantDrops, drops)
		})
	}
}

// TestEnumerateGuardValidPairs_PairShape pins that a pair carries P's raw whole-session turn
// ordinal, the between-turns assistant response, and the prior human turn ("" for the first).
func TestEnumerateGuardValidPairs_PairShape(t *testing.T) {
	sp := pairSpine(
		pairUserTurn("first ask"), pairAsstTurn("response one"),
		pairUserTurn("second ask"), pairAsstTurn("response two"),
		pairUserTurn("third ask"),
	)
	pairs, drops := enumerateGuardValidPairs("org-1", "raw-1", sp)
	require.Len(t, pairs, 2)
	assert.Equal(t, guardDrops{}, drops)

	assert.Equal(t, int32(0), pairs[0].PromptIndex)
	assert.Equal(t, "first ask", pairs[0].P)
	assert.Equal(t, "response one", pairs[0].AgentResponse)
	assert.Equal(t, "second ask", pairs[0].Q)
	assert.Empty(t, pairs[0].Prior)

	assert.Equal(t, int32(1), pairs[1].PromptIndex)
	assert.Equal(t, "second ask", pairs[1].P)
	assert.Equal(t, "response two", pairs[1].AgentResponse)
	assert.Equal(t, "third ask", pairs[1].Q)
	assert.Equal(t, "first ask", pairs[1].Prior)
}

// TestEnumerateGuardValidPairs_MachineTurnFilter pins that a delegated-agent turn (folded into
// the user role) is excluded from enumeration — a machine turn is never scored as a person
// correcting the agent.
func TestEnumerateGuardValidPairs_MachineTurnFilter(t *testing.T) {
	machineTurn := &sessionintelligencepb.Prompt{
		Role:     sessionintelligencepb.Role_ROLE_USER,
		Content:  "delegated agent instruction",
		Metadata: &sessionintelligencepb.PromptMetadata{IsSubAgent: true},
	}
	sp := pairSpine(
		pairUserTurn("human ask"), pairAsstTurn("response"),
		machineTurn, pairAsstTurn("agent work"),
		pairUserTurn("human follow-up"),
	)
	pairs, _ := enumerateGuardValidPairs("org-1", "raw-1", sp)
	// The machine turn is filtered out, so the only adjacent HUMAN pair is (human ask, human
	// follow-up) — the machine turn is neither a P nor a Q.
	require.Len(t, pairs, 1)
	assert.Equal(t, "human ask", pairs[0].P)
	assert.Equal(t, "human follow-up", pairs[0].Q)
}

func TestCapChars(t *testing.T) {
	assert.Equal(t, "abc", capChars("abc", 5))
	assert.Equal(t, "ab", capChars("abcde", 2))
	// Multi-byte runes are never sliced mid-rune.
	assert.Equal(t, "é", capChars("éé", 1))
}
