// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package pipelinee2e is the session-intelligence pipeline end-to-end matrix.
// It drives the REAL SessionAnalysisProcessor through its full path —
// actionable scan, claim, spine, R producer, commit, security-flag reconcile, and the
// state machine — against a real local Postgres leased from the committed
// latest.sql snapshot, with real DAOs and no
// DAO mocks. It seeds the edge-case matrix the pipeline design names and
// asserts the expected outcome per case (state-machine landing + grain outcomes), plus
// the publication guard (aggregate-only, no cross-vocabulary trend).
//
// The LLM leg has two interchangeable backends behind one seam. The processor calls the
// REAL sessionanalysis.LLMClient (the real parseReadResponse parser), which talks to a
// refllmclient.ContentGenerator. In this suite that generator is a fixture returning
// a recorded analyze_read response (the AnalyzeReadResponse VALID_READ shape), so the
// parser-contract and the grain writes are exercised end-to-end without a live model.
// The real-Gemini leg (start reference-llm with the repo-root key, swap the fixture for a
// real refllmclient.Client) is the eval-bar run — see eval_test.go for the exact
// steps; it is NOT run here and nothing in this suite claims a real-Gemini result.
//
// This package deliberately runs as an ordinary real-processor E2E against the local test
// database rather than through a separate heavyweight integration framework (one that would
// drive the whole running backend via the scheduler and could not control the state machine
// deterministically). It carries no integration build tag and gates on database
// reachability instead.
package pipelinee2e
