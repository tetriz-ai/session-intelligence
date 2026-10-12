// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package sessionanalysis is the session-analysis engine: the deterministic spine, the
// read/evidence/discard producers, commit, and round-trip detection.
package sessionanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Anonymise identifiers on the way to the LLM, as a send-transform, not a storage change
//. File and artifact paths in the LLM-input payload the spine
// builds are hashed KEEPING extension and directory depth — judgment needs "a spec versus a test
// versus config", never the name — so `src/auth/login.go` becomes `<h>/<h>/<h>.go`. The stored
// session_turn_analyses.files_touched keeps raw, relative, username-redacted paths (as
// session_insights does); this transform touches ONLY the payload passed to the producers.
//
// Scope and honesty: this hashes tokens that are UNAMBIGUOUSLY file paths — a slash-separated run
// ending in a file extension (`dir/.../name.ext`) — deliberately, to keep the false-positive rate
// near zero. That excludes slash-joined prose ("read/write", "TCP/IP"), dotted prose ("e.g.",
// "1.2.3"), and bare extensionless directory names, none of which are path-shaped by this test, so
// the transform never mangles ordinary text or degrades the model's judgment. It is therefore a
// targeted REDUCTION of identifier exposure over the raw-send baseline (itself parity with the
// shipped Session pipeline), not a complete PII scrubber: a bare filename with no directory, a
// Windows backslash path, or an identifier only a human would recognise is not matched. Determinism
// (a stable per-value hash) is deliberate so the same path reads consistently within one transcript,
// letting the model cross-reference the same file without seeing its name.

// pathTokenRe matches an unambiguous file path: one or more `segment/` directory parts followed by
// a `name.ext` file part. Requiring BOTH a slash and a final extension is what keeps prose out.
var pathTokenRe = regexp.MustCompile(`(?:[A-Za-z0-9_.\-]+/)+[A-Za-z0-9_.\-]+\.[A-Za-z0-9]{1,8}`)

// anonymizePaths hashes every file-path token in content, preserving each token's directory depth
// and the final segment's file extension. Deterministic per token value.
func anonymizePaths(content string) string {
	return pathTokenRe.ReplaceAllStringFunc(content, hashPathToken)
}

// hashPathToken hashes one file-path token: each slash-separated segment is replaced by a short
// stable hash, and the last segment's extension is preserved. A token whose match spans a "//"
// (a URL authority that slipped past the scheme) is left untouched.
func hashPathToken(token string) string {
	if strings.Contains(token, "//") {
		return token
	}
	segments := strings.Split(token, "/")
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			continue // keep root/relative markers so depth reads true
		}
		segments[i] = hashSegment(segment, i == len(segments)-1)
	}
	return strings.Join(segments, "/")
}

// hashSegment replaces one path segment with a short stable hash. When keepExt is set (the final
// segment) and the segment has a file extension, the extension is preserved so the model still sees
// "a .go versus a .md versus a .yaml".
func hashSegment(segment string, keepExt bool) string {
	ext := ""
	if keepExt {
		if dot := strings.LastIndexByte(segment, '.'); dot > 0 && dot < len(segment)-1 {
			ext = segment[dot:]
		}
	}
	sum := sha256.Sum256([]byte(segment))
	return "p" + hex.EncodeToString(sum[:])[:8] + ext
}
