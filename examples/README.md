# Examples

| You want | Example |
|---|---|
| Turn a raw session transcript into structured analysis | [analyze-raw-session](analyze-raw-session/) |
| Map detected security issues to open flags | [reconcile-security-flags](reconcile-security-flags/) |
| Plug your own round-trip detector | [custom-pair-detector](custom-pair-detector/) |

Run them all with `scripts/run-examples.sh`, which runs each example's documented command from a
fresh clone and compares its output to the example's README. Every example runs offline with no
database, no model and no network call.
