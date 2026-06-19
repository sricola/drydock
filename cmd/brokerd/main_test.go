package main

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestChooseLogHandler(t *testing.T) {
	cases := []struct {
		name       string
		jsonForced bool
		isTTY      bool
		want       string // "*slog.JSONHandler" or "*slog.TextHandler"
	}{
		{"json forced on a tty wins", true, true, "*slog.JSONHandler"},
		{"non-tty defaults to json", false, false, "*slog.JSONHandler"},
		{"tty without force gets text", false, true, "*slog.TextHandler"},
		{"json forced off a tty", true, false, "*slog.JSONHandler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := chooseLogHandler(&bytes.Buffer{}, tc.jsonForced, tc.isTTY)
			if got := typeName(h); got != tc.want {
				t.Errorf("chooseLogHandler(json=%v, tty=%v) = %s, want %s",
					tc.jsonForced, tc.isTTY, got, tc.want)
			}
		})
	}
}

func typeName(h slog.Handler) string {
	switch h.(type) {
	case *slog.JSONHandler:
		return "*slog.JSONHandler"
	case *slog.TextHandler:
		return "*slog.TextHandler"
	default:
		return "unknown"
	}
}
