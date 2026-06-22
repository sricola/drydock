package main

import (
	"strings"
	"testing"
)

func TestPromptChoice(t *testing.T) {
	opts := []string{"Claude Code", "OpenAI Codex", "both"}
	cases := []struct {
		in   string
		want int
	}{
		{"2\n", 2},       // explicit
		{"\n", 1},        // empty → default
		{"x\n5\n3\n", 3}, // invalid, out-of-range, then valid
	}
	for _, c := range cases {
		var out strings.Builder
		if got := promptChoice(strings.NewReader(c.in), &out, "Which agent?", opts, 1); got != c.want {
			t.Errorf("in=%q → %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPromptYesNo(t *testing.T) {
	cases := []struct {
		in   string
		dflt bool
		want bool
	}{
		{"y\n", false, true},
		{"n\n", true, false},
		{"\n", true, true}, // empty → default
		{"\n", false, false},
	}
	for _, c := range cases {
		var out strings.Builder
		if got := promptYesNo(strings.NewReader(c.in), &out, "ok?", c.dflt); got != c.want {
			t.Errorf("in=%q dflt=%v → %v, want %v", c.in, c.dflt, got, c.want)
		}
	}
}
