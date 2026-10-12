# custom-pair-detector

This example shows the round-trip detection extension point. `analyze.AnalyzeSession` accepts any
`analyze.RoundTripDetector`; the bundled one calls a reference-llm ML service (an uncalibrated
placeholder), but you can plug your own. This detector scores a pair by a trivial keyword rule and
returns the top-N candidates above a threshold, enough to see the interface a real detector
implements. Reach for it when you want your own round-trip scoring. It runs with no service and no
model.

```sh
go run ./examples/custom-pair-detector
```

```text
round-trip candidates: 2 (truncated=false)
- prompt 1: score 0.9
- prompt 2: score 0.9
```
