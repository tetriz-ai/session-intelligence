# vhs tape for the demo GIF. Render with `vhs .github/assets/demo.tape`.
# It types each example's documented command verbatim and shows its output, so the GIF and
# examples/README cannot disagree. Every command is fully offline: no database, no model and
# no network call. Nothing here drives a live model.
Output .github/assets/demo.gif
Set Width 1000
Set Height 700
Set FontSize 18
Env PS1 "$ "
# The module cache is warmed before recording (CI's demo.yml runs `go build ./examples/...`
# first; for a local render, run that once beforehand), so each `go run` shows only its output
# and frame 0 is a clean prompt — no hidden build/clear to leak into the recording.
Type "go run ./examples/analyze-raw-session" Sleep 500ms Enter
Sleep 2500ms
Type "go run ./examples/reconcile-security-flags" Sleep 500ms Enter
Sleep 2500ms
Type "go run ./examples/custom-pair-detector" Sleep 500ms Enter
Sleep 3s
