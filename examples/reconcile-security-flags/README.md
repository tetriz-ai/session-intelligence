# reconcile-security-flags

This example shows the security detector's reconcile half. A producer found security-layer issues
on one record — an API key twice and a database credential once — and the evidence pass found a
PII issue that the engine gates off here. Reconcile maps them to open security flags, collapsing
repeats to one flag per type at the highest severity seen. Reach for it to see how detected issues
become `security_flags`. It runs entirely in memory: an in-memory store stands in for the database,
so no database is touched and nothing is persisted, and each stored excerpt is masked as a backstop
(not relied on as redaction), so a raw secret is not written out.

```sh
go run ./examples/reconcile-security-flags
```

```text
open flags: 2
- api-key-leak (high): 2 occurrence(s)
- db-credential-leak (medium): 1 occurrence(s)
```
