// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import "github.com/tetriz-ai/session-intelligence/internal/sessionanalysis"

// ErrNoTaggableTurns is returned when a conversation has no taggable (human) turns.
var ErrNoTaggableTurns = sessionanalysis.ErrNoTaggableTurns

// ErrTranscriptTooLarge is returned when the rendered transcript exceeds the single-call ceiling.
var ErrTranscriptTooLarge = sessionanalysis.ErrTranscriptTooLarge
