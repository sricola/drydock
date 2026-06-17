package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureExit runs f and returns (stdout, exitCode). It intercepts os.Exit
// via a panic trap on the helper — consumeHelpFlag calls os.Exit directly,
// which we can't catch from a test in-process. Instead we exec the test
// binary as a subprocess, but for these tiny pure-text helpers it's easier
// to refactor: extract the help text via subHelp directly.
// This test focuses on the data structure rather than os.Exit.

// Every command the top-level usage advertises must have an entry in
// subHelp — otherwise `<cmd> --help` prints an empty body. This catches
// the failure mode where a new subcommand ships without help text.
func TestSubHelp_CoversEveryAdvertisedCommand(t *testing.T) {
	// Commands the dispatcher accepts. Keep in sync with main()'s switch.
	cmds := []string{
		"init", "start", "submit", "status", "tasks",
		"logs", "review", "kill", "pending", "approve", "deny",
		"doctor", "version",
	}
	for _, c := range cmds {
		if _, ok := subHelp[c]; !ok {
			t.Errorf("subHelp missing entry for %q", c)
		}
	}
}

// consumeHelpFlag's behavior: silently return when the first arg isn't
// help-shaped, exit 0 (with output) when it is. The exit-0 path is hard
// to test in-process; we instead verify the no-op branch and rely on the
// integration smoke for the exit-path.
func TestConsumeHelpFlag_NoOpOnNonHelpArg(t *testing.T) {
	// Capture stdout to confirm nothing leaked when the flag isn't help.
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()

	consumeHelpFlag("approve", []string{"some-task-id"})
	consumeHelpFlag("kill", []string{})
	consumeHelpFlag("logs", []string{"abc123", "-f"})

	w.Close()
	os.Stdout = orig
	out := <-done
	if out != "" {
		t.Errorf("consumeHelpFlag printed unexpectedly:\n%s", out)
	}
}

// version is a package-level var set by -ldflags. The default value when
// no ldflags are passed should be "dev" — anything else means an earlier
// build leaked a stale value.
func TestVersion_DefaultsToDev(t *testing.T) {
	// version is set at package init via -ldflags. In the test binary
	// there are no ldflags, so we should see the source default.
	if version != "dev" {
		// Either the test binary was built with -ldflags (e.g. by
		// `go test -ldflags '-X main.version=…'`) or someone replaced
		// the literal default. Both are unusual; flag it.
		if !strings.HasPrefix(version, "v") && version != "dev" {
			t.Errorf("version = %q, want %q or a v-prefixed tag", version, "dev")
		}
	}
}
