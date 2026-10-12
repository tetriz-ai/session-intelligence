// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package refllmclient

import "testing"

// The point of the discriminator is that reference-llm answers 502 for both a restarted pod
// and a response that will truncate identically forever. These cases pin which signal wins:
// the body's code decides, the status does not, and a peer that sends no code behaves as
// it did before the field existed.
func TestClassifyBody(t *testing.T) {
	tests := []struct {
		name string
		in   ErrorKind
		body string
		want ErrorKind
	}{
		{
			"the real 502 shape, detail-wrapped by FastAPI", KindServer,
			`{"detail":{"error":"Gemini response truncated at max_output_tokens","code":"OUTPUT_TRUNCATED","provider":"gemini","model":"gemini-2.5-flash","usage":{"completion_tokens":49138,"thoughts_tokens":16381}}}`,
			KindOutputTruncated,
		},
		{
			"an unwrapped detail is accepted too", KindServer,
			`{"code":"OUTPUT_TRUNCATED"}`, KindOutputTruncated,
		},

		// The cases that matter most: nothing else may be swept up as deterministic.
		{
			"a genuine transient 502 keeps KindServer", KindServer,
			`{"detail":{"error":"connection reset","code":"UNAVAILABLE"}}`, KindServer,
		},
		{
			"a peer predating the code field is unchanged", KindServer,
			`{"detail":{"error":"Gemini response truncated at max_output_tokens"}}`, KindServer,
		},
		{"an unparseable body is unchanged", KindServer, `<html>502 Bad Gateway</html>`, KindServer},
		{"an empty body is unchanged", KindServer, ``, KindServer},

		// Promotion is keyed on the code alone, so it does not depend on the status the
		// peer chose. reference-llm sends 502 today; that is not what makes this correct.
		{
			"promotion is by code, not by status", KindClient,
			`{"detail":{"code":"OUTPUT_TRUNCATED"}}`, KindOutputTruncated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyBody(tt.in, []byte(tt.body)); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
