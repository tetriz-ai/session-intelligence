// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

import "context"

//go:generate go tool mockgen -source=hook.go -destination=mocks/mock_hook.go -package=mocks

// SuccessHook is invoked after a successful single-shot call so the caller can emit the
// model analytics event. The hook fires only after a successful single-shot call (the
// level-1 emit-timing trade-off).
type SuccessHook interface {
	OnGenerateContent(ctx context.Context, req GenerateContentRequest, resp *GenerateContentResponse)
}
