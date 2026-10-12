// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"strings"

	sessionintelligencepb "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/v1"
	"github.com/tetriz-ai/session-intelligence/internal/refllmclient"
)

// The Go-side payload truncation caps, ported verbatim from the reference driver
// : P/Q/prior at 4000 chars,
// the assistant response at 6000. These bound what crosses to reference-llm; the feature
// extractor there applies its own tighter head/tail caps. Both live in shared code, so the
// corpus fit and the serve truncate identically.
const (
	roundTripPromptCharCap   = 4000
	roundTripResponseCharCap = 6000
)

// guardDrops counts each of the five validity-guard rejections for one session. It mirrors the
// reference driver's per-reason drop tally so the guard-drop rate is observable per reason
// rather than as one lumped number.
type guardDrops struct {
	noAgentResponse int
	pEmpty          int
	qEmpty          int
	qIdenticalToP   int
	duplicatePair   int
}

// enumerateGuardValidPairs enumerates every adjacent human-turn pair after the machine-turn
// filter and applies the five-part validity guard, returning the guard-valid P/A/Q triples and
// the per-reason drop tally. Enumeration is Go-side because it depends on the authorship stamp
// the spine already holds: it reuses the exact machine-turn-filtered, turn-grouped block set R
// reads (buildTurnBlocks + prefixHumanTurnFilter), so a pair's PromptIndex is P's raw
// whole-session turn ordinal — the same key read.Turns and the measured turn table use.
//
// The guard: at least one non-empty assistant turn between P and Q; both P and Q non-empty; Q
// not byte-identical to P; the (P,Q) pair not already seen this session. prior is the human
// turn before P ("" for the session's first pair), feeding the detector's has_prior signal.
func enumerateGuardValidPairs(orgID, rawRecordID string, sp *spineResult) ([]refllmclient.RoundTripPair, guardDrops) {
	groups := splitHumanTurns(sp.prompts)
	rows := buildMeasuredTurnRows(orgID, rawRecordID, sp)
	blocks := buildTurnBlocks(groups, rows, sp.usage.GetTurnUsage(), prefixHumanTurnFilter, 0, len(groups))

	var drops guardDrops
	pairs := make([]refllmclient.RoundTripPair, 0, len(blocks))
	seen := make(map[[2]string]struct{}, len(blocks))
	for i := 1; i < len(blocks); i++ {
		p := strings.TrimSpace(userTurnText(blocks[i-1]))
		q := strings.TrimSpace(userTurnText(blocks[i]))
		// The agent response is ALL assistant text strictly between P's raw turn and Q's raw
		// turn — including responses to any filtered machine turn between them — matching the
		// reference interval (detect_driver.py: assistant join over turns[pi+1:qi]). Reading
		// only P's own block would drop an intervening machine turn's response and skew the
		// cosine/jaccard/length/NLI features away from what the corpus fit computes.
		response := rawSpanAssistantText(groups, int(blocks[i-1].Index), int(blocks[i].Index))
		switch {
		case strings.TrimSpace(response) == "":
			drops.noAgentResponse++
			continue
		case p == "":
			drops.pEmpty++
			continue
		case q == "":
			drops.qEmpty++
			continue
		case p == q:
			drops.qIdenticalToP++
			continue
		}
		key := [2]string{p, q}
		if _, dup := seen[key]; dup {
			drops.duplicatePair++
			continue
		}
		seen[key] = struct{}{}

		prior := ""
		if i >= 2 {
			prior = strings.TrimSpace(userTurnText(blocks[i-2]))
		}
		pairs = append(pairs, refllmclient.RoundTripPair{
			PromptIndex:   blocks[i-1].Index,
			P:             capChars(userTurnText(blocks[i-1]), roundTripPromptCharCap),
			AgentResponse: capChars(response, roundTripResponseCharCap),
			Q:             capChars(userTurnText(blocks[i]), roundTripPromptCharCap),
			Prior:         capChars(prior, roundTripPromptCharCap),
		})
	}
	return pairs, drops
}

// rawSpanAssistantText concatenates the assistant text of every raw turn group whose index is
// in [startIdx, endIdx) — all assistant text strictly between P's raw turn (startIdx) and Q's
// raw turn (endIdx), across P's own group AND any filtered machine group between them, in order.
// groups is the UNFILTERED splitHumanTurns set, so a machine turn's assistant response is
// included here even though the machine turn itself is not a P or a Q.
func rawSpanAssistantText(groups [][]*sessionintelligencepb.Prompt, startIdx, endIdx int) string {
	var b strings.Builder
	for k := startIdx; k < endIdx && k < len(groups); k++ {
		b.WriteString(assistantTextOf(groups[k]))
	}
	return b.String()
}

// userTurnText returns the human prompt text of a turn block — its first ROLE_USER message's
// content (the block's other messages are the assistant/tool response to it).
func userTurnText(block *turnBlock) string {
	for _, message := range block.Messages {
		if message.GetRole() == sessionintelligencepb.Role_ROLE_USER {
			return message.GetContent()
		}
	}
	return ""
}

// assistantTextOf concatenates the assistant messages of one raw turn group — the agent's
// output within that group, in message order.
func assistantTextOf(messages []*sessionintelligencepb.Prompt) string {
	var b strings.Builder
	for _, message := range messages {
		if message.GetRole() == sessionintelligencepb.Role_ROLE_ASSISTANT {
			b.WriteString(message.GetContent())
		}
	}
	return b.String()
}

// capChars truncates s to at most n runes, so a multi-byte sequence is never sliced mid-rune.
func capChars(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
