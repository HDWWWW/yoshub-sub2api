# codex2api local prompt filter

Source: https://github.com/james-6-23/codex2api/tree/b6a827f9357a6cf1ac4c211854726d808406c44f/security/promptfilter

Upstream Go sources and tests are copied without logic changes (Git normalizes line endings to LF). `UPSTREAM.json` records their SHA-256 hashes. `sub2_evidence.go` only exposes existing signal evidence for Sub2 audit display; it does not change classification. No root license file was present in the source snapshot.

Sub2 uses the recommended local preset, with configurable thresholds, preset exclusions and custom patterns. External model review, session correlation, account punishment and proxy-level semantic integrations are not enabled by this adapter. Source classification, normalization, context discounts, scoring and the tool-output enforcement exception remain in the upstream pipeline.
