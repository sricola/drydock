package main

import "testing"

func TestClaudeVersionLine_StripsContainerProgress(t *testing.T) {
	noisy := `[0/6] [0s]
[1/6] Fetching image [0s]
[2/6] Unpacking image [0s]
[3/6] Fetching kernel [0s]
[4/6] Fetching init image [0s]
[5/6] Unpacking init image [0s]
[6/6] Starting container [0s]
[6/6] Starting container [0s]
2.1.177 (Claude Code)
`
	got := claudeVersionLine(noisy)
	want := "2.1.177 (Claude Code)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestClaudeVersionLine_PassesThroughCleanOutput(t *testing.T) {
	got := claudeVersionLine("2.1.177 (Claude Code)\n")
	if got != "2.1.177 (Claude Code)" {
		t.Errorf("got %q", got)
	}
}

func TestClaudeVersionLine_NoMatchFallsBackToTrimmed(t *testing.T) {
	got := claudeVersionLine("  some other output  \n")
	if got != "some other output" {
		t.Errorf("got %q", got)
	}
}
