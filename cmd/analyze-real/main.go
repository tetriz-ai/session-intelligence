// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command analyze-real runs the real-LLM analysis on a transcript you supply, against a
// running reference-llm booted with your own LLM__GEMINI_API_KEY. It is the turnkey
// "bring your own session and your own key" path over the public analyze.AnalyzeSession
// front door — no database, no config. Point --url at your reference-llm (docker-compose
// brings one up; see scripts/e2e-real.sh), pass a transcript in the Conversation protojson
// form or a record `local-session-extractor show` printed, and it prints the analysis as JSON.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tetriz-ai/session-intelligence/analyze"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: analyze-real [-url <reference-llm URL>] [-timeout <dur>] <transcript.json>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  <transcript.json>  a session transcript in the Conversation protojson form,")
		fmt.Fprintln(os.Stderr, "                     or a record from `local-session-extractor show <source> -- <id>`.")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	// main stays thin so run's deferred cancel always fires — an os.Exit here would skip it.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	url := flag.String("url", envOr("SESSIONINTEL_REAL_GEMINI_URL", "http://localhost:8000"),
		"base URL of a running reference-llm booted with your LLM__GEMINI_API_KEY")
	timeout := flag.Duration("timeout", 5*time.Minute,
		"hard wall-clock deadline for the whole analysis (R+E+D calls)")
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	conversation, err := analyze.ReadConversationFile(flag.Arg(0))
	if err != nil {
		return fmt.Errorf("reading transcript: %w", err)
	}

	// Bound the whole run: the R/E/D passes are sequential network calls, so without a deadline a
	// slow or hung reference-llm would block up to the client's per-call backstop (10m each).
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := analyze.AnalyzeSession(ctx, conversation, analyze.Options{
		Provider: analyze.NewReferenceLLMProvider(*url),
	})
	if err != nil {
		return fmt.Errorf("analyzing session: %w", err)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
