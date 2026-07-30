package imagescripts

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// entrypoint.sh cannot be executed end-to-end in this harness: it installs
// the egress firewall as root and exec's the agent binary. But the plan-mode
// preamble is a self-contained env-gated shell block between the PROMPT read
// and the agent dispatch, so we extract exactly that block from the shipped
// script and run it under bash — the real logic, not a copy — asserting the
// prompt rewrite behavior with and without DRYDOCK_MODE=plan.

const entrypointRel = "../../image/entrypoint.sh"

// extractPlanBlock returns the `if [ "${DRYDOCK_MODE:-}" = "plan" ]; then …
// fi` block from entrypoint.sh, failing the test if it's absent or placed
// outside the PROMPT-read → agent-case window (the preamble must run after
// the prompt exists and before any agent consumes it).
func extractPlanBlock(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(entrypointRel)
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	src := string(b)

	promptRead := strings.Index(src, `PROMPT="$(cat /work/.task/prompt.txt)"`)
	caseDispatch := strings.Index(src, `case "$AGENT" in`)
	blockStart := strings.Index(src, `if [ "${DRYDOCK_MODE:-}" = "plan" ]; then`)
	if promptRead < 0 || caseDispatch < 0 {
		t.Fatalf("entrypoint.sh anchors missing (prompt read at %d, case at %d)", promptRead, caseDispatch)
	}
	if blockStart < 0 {
		t.Fatal("entrypoint.sh has no DRYDOCK_MODE=plan block")
	}
	if blockStart < promptRead || blockStart > caseDispatch {
		t.Fatalf("plan block at %d must sit after the PROMPT read (%d) and before the agent case (%d)",
			blockStart, promptRead, caseDispatch)
	}
	blockEnd := strings.Index(src[blockStart:], "\nfi\n")
	if blockEnd < 0 {
		t.Fatal("plan block has no closing fi")
	}
	return src[blockStart : blockStart+blockEnd+len("\nfi\n")]
}

// runPlanBlock executes the extracted block under bash with PROMPT preset,
// returning the resulting PROMPT. env is appended to a minimal environment.
func runPlanBlock(t *testing.T, block string, extraEnv ...string) string {
	t.Helper()
	script := "set -euo pipefail\nPROMPT=\"original task text\"\n" + block + "\nprintf %s \"$PROMPT\"\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plan block failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestEntrypoint_PlanModePrependsPreamble(t *testing.T) {
	block := extractPlanBlock(t)
	got := runPlanBlock(t, block, "DRYDOCK_MODE=plan")
	if !strings.HasPrefix(got, "PLAN MODE:") {
		t.Errorf("PROMPT does not start with the plan preamble:\n%s", got)
	}
	for _, want := range []string{
		"Do NOT create, modify, or delete any repository files",
		"/work/.task/plan.md",
		"Make no code changes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preamble missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "original task text") {
		t.Errorf("original prompt not preserved after the preamble:\n%s", got)
	}
}

func TestEntrypoint_NoPlanModeLeavesPromptUntouched(t *testing.T) {
	block := extractPlanBlock(t)
	// DRYDOCK_MODE unset entirely — the ${DRYDOCK_MODE:-} default must keep
	// the block a no-op under set -u.
	if got := runPlanBlock(t, block); got != "original task text" {
		t.Errorf("PROMPT modified without DRYDOCK_MODE=plan:\n%s", got)
	}
	// A non-plan value must not trigger the preamble either.
	if got := runPlanBlock(t, block, "DRYDOCK_MODE=other"); got != "original task text" {
		t.Errorf("PROMPT modified with DRYDOCK_MODE=other:\n%s", got)
	}
}
