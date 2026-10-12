// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFileCapped checks the byte ceiling ReadConversationFile relies on: a file at or
// under the limit reads back whole, and one over it is rejected without being loaded.
func TestReadFileCapped(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, size int) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Repeat("a", size)), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	const limit = 16

	t.Run("at the limit reads whole", func(t *testing.T) {
		content, err := readFileCapped(write("at.json", limit), limit)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(content) != limit {
			t.Fatalf("got %d bytes, want %d", len(content), limit)
		}
	})

	t.Run("one byte over is rejected", func(t *testing.T) {
		_, err := readFileCapped(write("over.json", limit+1), limit)
		if err == nil {
			t.Fatal("expected an over-limit error, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error %q should name the size limit", err)
		}
	})

	t.Run("missing file surfaces the open error", func(t *testing.T) {
		_, err := readFileCapped(filepath.Join(dir, "nope.json"), limit)
		if err == nil {
			t.Fatal("expected an open error for a missing file")
		}
	})
}
