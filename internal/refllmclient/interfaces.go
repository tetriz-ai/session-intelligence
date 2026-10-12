// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

import "context"

//go:generate go tool mockgen -source=interfaces.go -destination=mocks/mock_client.go -package=mocks

// The narrow consumer interfaces below let each caller depend only on the endpoint it
// uses and mock it; *Client satisfies both.

// ContentGenerator is the generate-content surface.
type ContentGenerator interface {
	GenerateContent(ctx context.Context, req GenerateContentRequest) (*GenerateContentResponse, error)
}

// RoundTripDetector is the detect-round-trips surface (session-analysis PAIRS detector).
type RoundTripDetector interface {
	DetectRoundTrips(ctx context.Context, req DetectRoundTripsRequest) (*DetectRoundTripsResponse, error)
}

var (
	_ ContentGenerator  = (*Client)(nil)
	_ RoundTripDetector = (*Client)(nil)
)
