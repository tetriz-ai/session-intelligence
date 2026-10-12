// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package sessionanalysis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAnonymizePaths guards the pre-send path transform: unambiguous file paths are hashed keeping
// extension + directory depth, while prose that merely contains a slash or a dot is left untouched
// (a false positive would mangle the transcript and degrade the model's judgment).
func TestAnonymizePaths(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// wantChanged: whether the output differs from the input.
		wantChanged bool
		// assertions run against the output.
		assert func(t *testing.T, out string)
	}{
		{
			name:        "a real path is hashed, extension and depth preserved",
			in:          "edited src/auth/login.go here",
			wantChanged: true,
			assert: func(t *testing.T, out string) {
				assert.NotContains(t, out, "login", "the file name is hashed away")
				assert.NotContains(t, out, "auth", "the directory name is hashed away")
				assert.Contains(t, out, ".go", "the extension survives")
				assert.Equal(t, 2, strings.Count(out, "/"), "the directory depth (two slashes) survives")
				assert.True(t, strings.HasPrefix(strings.Fields(out)[1], "p"), "each segment is a p<hash> token")
			},
		},
		{
			name:        "a deep path keeps its full depth and extension",
			in:          "a/b/c/d/config.yaml",
			wantChanged: true,
			assert: func(t *testing.T, out string) {
				assert.Contains(t, out, ".yaml")
				assert.Equal(t, 4, strings.Count(out, "/"), "four slashes → five segments")
			},
		},
		// Prose that must NOT be touched — the whole reason for requiring slash AND extension.
		{name: "slash prose with no extension is untouched", in: "read/write and TCP/IP", wantChanged: false},
		{name: "dotted prose with no slash is untouched", in: "e.g. see version 1.2.3 etc.", wantChanged: false},
		{name: "a library name (dot, no slash) is untouched", in: "we use Node.js and README.md", wantChanged: false},
		{name: "plain prose is untouched", in: "the prompt was clear and specific", wantChanged: false},
		{
			name:        "relative markers keep depth true",
			in:          "./pkg/util/helpers.ts",
			wantChanged: true,
			assert: func(t *testing.T, out string) {
				assert.True(t, strings.HasPrefix(out, "./"), "the leading ./ relative marker is preserved")
				assert.Contains(t, out, ".ts")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := anonymizePaths(tt.in)
			if tt.wantChanged {
				assert.NotEqual(t, tt.in, out, "the path-shaped input should be transformed")
			} else {
				assert.Equal(t, tt.in, out, "non-path text must be left exactly as-is")
			}
			if tt.assert != nil {
				tt.assert(t, out)
			}
		})
	}
}

// TestAnonymizePaths_Deterministic pins that the same path hashes to the same token, so the model
// can cross-reference the same file within a transcript without seeing its name.
func TestAnonymizePaths_Deterministic(t *testing.T) {
	first := anonymizePaths("touch src/auth/login.go")
	second := anonymizePaths("also src/auth/login.go again")
	// Extract the hashed path token from each (the token containing ".go").
	tokenOf := func(s string) string {
		for _, f := range strings.Fields(s) {
			if strings.Contains(f, ".go") {
				return f
			}
		}
		return ""
	}
	assert.Equal(t, tokenOf(first), tokenOf(second), "the same path hashes identically")
	assert.NotEmpty(t, tokenOf(first))
}
