package main

import "testing"

// Strata documents only none/low/medium/high on /v1/chat/completions, so
// aliases must fold into those four and anything else must be rejected rather
// than sent verbatim and answered with a 400.
func TestNormalizeReasoningEffort(t *testing.T) {
	for in, want := range map[string]string{
		"high": "high", "HIGH": "high", " xhigh ": "high", "max": "high",
		"med": "medium", "mid": "medium", "off": "none", "false": "none",
		"none": "none", "minimal": "low", "min": "low", "low": "low",
		"": "", "default": "", "reset": "", "clear": "",
	} {
		got, ok := normalizeReasoningEffort(in)
		if !ok || got != want {
			t.Errorf("normalizeReasoningEffort(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"turbo", "highest", "1", "very high", "auto"} {
		if _, ok := normalizeReasoningEffort(bad); ok {
			t.Errorf("normalizeReasoningEffort(%q) should be rejected", bad)
		}
	}
}
