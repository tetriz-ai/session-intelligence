// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package s3 defines the object-read seam the engine uses to fetch a session's
// raw conversation transcript. The engine only ever reads, so the interface is a
// single method; adopters back it with S3, GCS, a local directory, or a fake.
package s3

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// S3Client is the read seam. path is an opaque object key.
type S3Client interface {
	// ReadObject returns the bytes at path, or an error if it is absent.
	ReadObject(ctx context.Context, path string) ([]byte, error)
}

// DirReader is a reference S3Client that reads objects from a local directory,
// treating the object key as a path relative to Root. It lets the engine run
// against on-disk transcripts with no cloud dependency.
type DirReader struct{ Root string }

// ReadObject reads Root/path from the local filesystem.
func (d DirReader) ReadObject(_ context.Context, path string) ([]byte, error) {
	full := filepath.Join(d.Root, filepath.Clean("/"+path))
	content, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read object %q: %w", path, err)
	}
	return content, nil
}
