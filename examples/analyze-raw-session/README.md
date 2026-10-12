# analyze-raw-session

This example runs the public single-session front door — [`analyze.AnalyzeSession`](https://pkg.go.dev/github.com/tetriz-ai/session-intelligence/analyze#AnalyzeSession) —
over a raw session transcript (the JSON a coding-agent session is captured as) and prints the
**structured analysis**: the session verdict, the input-quality scores, and the judged turn and
phase counts.

It is self-contained and offline. Instead of a real `reference-llm`/Gemini provider it supplies a tiny
in-process fake `Provider` that returns a fixed read-pass result, so the example runs with no
network, no key and no database. Point [`analyze.NewReferenceLLMProvider`](https://pkg.go.dev/github.com/tetriz-ai/session-intelligence/analyze#NewReferenceLLMProvider)
at a service booted with your own `LLM__GEMINI_API_KEY` to run the real pipeline — see
[`scripts/e2e-real.sh`](../../scripts/e2e-real.sh) and the top-level README.

```sh
go run ./examples/analyze-raw-session
```

```text
session: example-session (CLAUDE_CODE)
goal: Add retry with backoff and a unit test
coding work: true
scores: overall 8, clarity 7
turns: 2 judged, phases: 1
security flags: 0
```
