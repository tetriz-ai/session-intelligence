// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package goroutine runs a function in a background goroutine with panic recovery.
package goroutine

import (
	"context"

	"github.com/tetriz-ai/session-intelligence/internal/logger"
)

// RunWithCtx runs fn in a new goroutine, recovering and logging any panic so a
// background task cannot crash the process.
func RunWithCtx(ctx context.Context, fn func(ctx context.Context)) {
	go func() {
		defer logger.RecoverPanicAndError(ctx)
		fn(ctx)
	}()
}
