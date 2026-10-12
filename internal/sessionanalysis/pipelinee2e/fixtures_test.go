// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package pipelinee2e

// The recorded reference-llm analyze_read responses this suite feeds through the REAL
// sessionanalysis.LLMClient parser. Both mirror W's AnalyzeReadResponse VALID_READ shape
// (the reference-llm schema tests) — every enum a LOWERCASE
// StrEnum value, turns/phases/session_analysis — so the parser-contract (P1's
// parseReadResponse against W's schema) is exercised end-to-end. A real-Gemini run swaps
// the fixture generator for a live refllmclient.Client and asserts the same grains
// (see eval_test.go); the responses below are a fixed transcript, not a model output.

// readFixtureClean is an ordinary session: two judged turns, one phase, a full session
// verdict with input-quality scores, no security issue. score_overall (8) is a plain int
// with no enum mapping, so it is the robust proof the verdict threaded through the parser.
const readFixtureClean = `{
  "turns": [
    {
      "prompt_index": 0, "prompt_type": "creation", "user_steering": "active",
      "prompt_issues": [{"slug": "vague-scope", "severity": "medium", "note": "scope was broad"}],
      "prompt_objective_met": "met", "agent_helpfulness": "helpful", "was_retry": "fresh"
    },
    {
      "prompt_index": 1, "prompt_type": "trivial", "user_steering": null,
      "prompt_issues": [],
      "prompt_objective_met": "not_applicable", "agent_helpfulness": "unclear", "was_retry": null
    }
  ],
  "phases": [
    {
      "prompt_range": {"start_index": 0, "end_index": 1}, "phase_theme": "creation",
      "task_complexity": "moderate", "phase_objective": "ship the parser"
    }
  ],
  "session_analysis": {
    "underlying_goal": "build a parser", "session_steering": "mostly_active",
    "goal_achieved": "fully", "smoothness": "smooth", "friction_cause": "none",
    "speed": "fast", "token_efficiency": "efficient", "goal_is_coding_work": true,
    "scores": {"overall": 8, "clarity": 9, "specificity": 7, "context_provision": 8,
               "actionability": 8, "completeness": 7, "efficiency": 9}
  }
}`

// readFixtureSecurity is the same session with a security-layer prompt issue on turn 0
// (a pasted secret): slug pii-injection, severity high. The parser lifts the three
// security slugs (api-key-leak / db-credential-leak / pii-injection) into SecurityIssues,
// which the commit reconciles into security_flags.
const readFixtureSecurity = `{
  "turns": [
    {
      "prompt_index": 0, "prompt_type": "creation", "user_steering": "active",
      "prompt_issues": [{"slug": "pii-injection", "severity": "high", "note": "email address pasted into the prompt"}],
      "prompt_objective_met": "met", "agent_helpfulness": "helpful", "was_retry": "fresh"
    },
    {
      "prompt_index": 1, "prompt_type": "trivial", "user_steering": null,
      "prompt_issues": [],
      "prompt_objective_met": "not_applicable", "agent_helpfulness": "unclear", "was_retry": null
    }
  ],
  "phases": [
    {
      "prompt_range": {"start_index": 0, "end_index": 1}, "phase_theme": "creation",
      "task_complexity": "moderate", "phase_objective": "ship the parser"
    }
  ],
  "session_analysis": {
    "underlying_goal": "build a parser", "session_steering": "mostly_active",
    "goal_achieved": "fully", "smoothness": "smooth", "friction_cause": "none",
    "speed": "fast", "token_efficiency": "efficient", "goal_is_coding_work": true,
    "scores": {"overall": 8, "clarity": 9, "specificity": 7, "context_provision": 8,
               "actionability": 8, "completeness": 7, "efficiency": 9}
  }
}`
