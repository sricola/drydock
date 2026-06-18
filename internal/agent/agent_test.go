package agent

import "testing"

func TestVendor(t *testing.T) {
	cases := map[string]struct {
		vendor string
		ok     bool
	}{
		"":       {"anthropic", true},
		"claude": {"anthropic", true},
		"codex":  {"openai", true},
		"bogus":  {"", false},
	}
	for in, want := range cases {
		v, ok := Vendor(in)
		if v != want.vendor || ok != want.ok {
			t.Errorf("Vendor(%q) = (%q,%v), want (%q,%v)", in, v, ok, want.vendor, want.ok)
		}
	}
}
