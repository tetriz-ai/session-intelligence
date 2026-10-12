// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package analyze

import (
	"context"
	"testing"
)

type fakeDetector struct{}

func (fakeDetector) Detect(_ context.Context, _, _ string, _ []RoundTripPair, _, _ int) (*DetectorResult, error) {
	return &DetectorResult{}, nil
}

// TestIsNilDetector covers the guard that keeps a typed-nil Detector (a non-nil interface wrapping
// a nil pointer) from reaching the engine and panicking: both an untyped nil and a typed nil read
// as "no detector", while a real detector (value or pointer) does not.
func TestIsNilDetector(t *testing.T) {
	var typedNil *fakeDetector
	cases := []struct {
		name string
		d    RoundTripDetector
		want bool
	}{
		{"untyped nil", nil, true},
		{"typed nil pointer", typedNil, true},
		{"real detector value", fakeDetector{}, false},
		{"real detector pointer", &fakeDetector{}, false},
	}
	for _, c := range cases {
		if got := isNilDetector(c.d); got != c.want {
			t.Errorf("%s: isNilDetector = %v, want %v", c.name, got, c.want)
		}
	}
}
