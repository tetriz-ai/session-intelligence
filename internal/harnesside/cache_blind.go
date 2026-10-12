// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package harnesside carries properties of the AI coding tools ("IDEs") that
// several harness data paths must agree on. A tool property is owned by neither the
// persistence layer nor any one processor, so it lives here and depends only on the
// common proto enums — rather than one reader importing the other's package for a
// fact that belongs to neither.
package harnesside

import (
	commonv1 "github.com/tetriz-ai/session-intelligence/gen/sessionintelligence/common/v1"
)

// A tool can omit cache detail two ways, and the two consequences do not coincide —
// so this is two sets, not one. The input-side sum is input + cacheCreate + cacheRead,
// while a derived cost prices those buckets at different rates with cacheRead the
// cheap one. A tool reporting NO cache buckets makes both wrong. A tool reporting
// cacheRead but not cacheCreate prices exactly (the discount is visible) yet its
// input-side SUM is still a floor, because the unreported cache-creation tokens are
// stored as zero and cannot be told apart from a real zero. One combined set is wrong
// for one consumer whichever membership it takes, so each derives from its own.

// CostBlindIdeTypes report no cache-token detail at all, cacheRead included, so a
// token-derived cost for one of them is priced from full-context input without the
// cheap cache-read discount — an approximation, not a measurement.
var CostBlindIdeTypes = []commonv1.IdeType{
	commonv1.IdeType_IDE_TYPE_CURSOR,
	commonv1.IdeType_IDE_TYPE_ANTIGRAVITY,
	commonv1.IdeType_IDE_TYPE_KIRO,
}

// InputSideBlindIdeTypes report no cache-CREATION detail, so an input-side token sum
// for one of them is a lower bound. Derived as a superset of CostBlindIdeTypes rather
// than re-listed, because reporting no cache buckets at all necessarily omits
// cache-creation too — the containment is an invariant, not a coincidence to maintain
// by hand. Codex is what makes this a distinct set: it reports cacheRead, so its cost
// prices exactly, but stores cacheCreate as zero-meaning-unreported.
var InputSideBlindIdeTypes = append(
	append([]commonv1.IdeType{}, CostBlindIdeTypes...),
	commonv1.IdeType_IDE_TYPE_CODEX,
)

// IsCostBlindIDE reports whether a token-derived cost for ide is an approximation.
func IsCostBlindIDE(ide commonv1.IdeType) bool { return contains(CostBlindIdeTypes, ide) }

// IsInputSideBlindIDE reports whether an input-side token sum for ide is a lower bound.
func IsInputSideBlindIDE(ide commonv1.IdeType) bool { return contains(InputSideBlindIdeTypes, ide) }

func contains(set []commonv1.IdeType, ide commonv1.IdeType) bool {
	for _, t := range set {
		if t == ide {
			return true
		}
	}
	return false
}
