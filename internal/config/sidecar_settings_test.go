package config

import "testing"

func TestEffectiveEgressMode(t *testing.T) {
	cases := map[string]string{
		"":        "log", // unset → default
		"log":     "log",
		"off":     "off",
		"redact":  "redact",
		"REDACT":  "log", // unknown/invalid → default (case-sensitive)
		"bogus":   "log",
		"disable": "log",
	}
	for in, want := range cases {
		if got := (SidecarSettings{EgressMode: in}).EffectiveEgressMode(); got != want {
			t.Errorf("EffectiveEgressMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEffectiveMaxOutputTokens(t *testing.T) {
	if got := (SidecarSettings{}).EffectiveMaxOutputTokens(); got != defaultMaxOutputTokens {
		t.Errorf("unset MaxOutputTokens = %d, want default %d", got, defaultMaxOutputTokens)
	}
	if got := (SidecarSettings{MaxOutputTokens: 64000}).EffectiveMaxOutputTokens(); got != 64000 {
		t.Errorf("configured MaxOutputTokens = %d, want 64000", got)
	}
	if got := (SidecarSettings{MaxOutputTokens: 10}).EffectiveMaxOutputTokens(); got != defaultMaxOutputTokens {
		t.Errorf("below-floor MaxOutputTokens = %d, want default %d", got, defaultMaxOutputTokens)
	}
}
