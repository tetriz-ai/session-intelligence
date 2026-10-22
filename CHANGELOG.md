# Changelog

All notable changes to this project are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- The reference LLM service locks certifi 2026.10.19, which distrusts the compromised eMudhra (emSign) intermediate CA keys. Its image pins `python:3.11-slim` by digest and installs uv 0.13.5.

## [0.1.0] - 2026-10-12

### Added

- Initial public release.
- Single-session analysis of coding-agent transcripts with prompting insights, turn and phase outcomes, quality signals, round-trip evidence and reconciled security flags.
- Direct input from Agent Desk's local-session-extractor records, plus a CLI and offline examples for analyzing and reviewing sessions.
- A Go API with custom model-provider and round-trip-detector interfaces, and a bundled Gemini reference service.
- Postgres batch processing with atomic analysis and security-flag persistence.

[Unreleased]: https://github.com/tetriz-ai/session-intelligence/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/tetriz-ai/session-intelligence/releases/tag/v0.1.0
