package main

import (
	"errors"
	"testing"
)

func TestCodexPresent(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		err    error
		wantOK bool
	}{
		{"healthy version", "codex-cli 0.140.0", nil, true},
		{"healthy with progress preamble", "[6/6] Starting container [0s]\ncodex-cli 0.140.0\n", nil, true},
		{"not found (nonzero exit)", "/bin/sh: 1: codex: not found", errors.New("exit status 127"), false},
		{"not found (zero exit, defensive)", "/bin/sh: 1: codex: not found", nil, false},
		{"container run error", "", errors.New("container run failed"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexPresent(c.out, c.err); got != c.wantOK {
				t.Errorf("codexPresent(%q, %v) = %v, want %v", c.out, c.err, got, c.wantOK)
			}
		})
	}
}

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

func TestGeminiPresent(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		err    error
		wantOK bool
	}{
		{"healthy version", "0.49.0", nil, true},
		{"healthy with progress preamble", "[6/6] Starting container [0s]\n0.49.0\n", nil, true},
		{"not found (nonzero exit)", "", errors.New("exit status 127"), false},
		{"empty output zero exit", "", nil, false},
		{"container run error", "", errors.New("container run failed"), false},
		{"not found with zero exit (defensive)", "/bin/sh: 1: gemini: not found", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := geminiPresent(c.out, c.err); got != c.wantOK {
				t.Errorf("geminiPresent(%q, %v) = %v, want %v", c.out, c.err, got, c.wantOK)
			}
		})
	}
}

// TestLastLine covers the pure string helper used to extract a version string
// from `container run` output that may have multi-line preamble noise.
func TestLastLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single line no newline", "hello", "hello"},
		{"single line trailing newline", "hello\n", "hello"},
		{"multi-line returns last non-empty", "line1\nline2\nline3", "line3"},
		{"multi-line with trailing newline", "line1\nline2\n", "line2"},
		{"whitespace trimmed", "  spaced  \n", "spaced"},
		{"container-run preamble", "[6/6] Starting container [0s]\n0.49.0\n", "0.49.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lastLine(c.in)
			if got != c.want {
				t.Errorf("lastLine(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestAPIKeySource(t *testing.T) {
	file := map[string]string{"OPENAI_API_KEY": "sk-o"}

	t.Run("env", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
		if got := apiKeySource("ANTHROPIC_API_KEY", file); got != "env" {
			t.Errorf("got %q, want env", got)
		}
	})
	t.Run("file", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		if got := apiKeySource("OPENAI_API_KEY", file); got != "~/.drydock/api-keys.env" {
			t.Errorf("got %q, want file path", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := apiKeySource("ANTHROPIC_API_KEY", file); got != "none" {
			t.Errorf("got %q, want none", got)
		}
	})
}

// TestLoopbackOnlyDNS pins the parser behind doctor's vm-dns advisory: when
// the host's primary resolver is a loopback proxy (Cloudflare WARP, dnscrypt,
// some VPNs), Apple `container` VMs get a vmnet forwarder that cannot reach
// it, and every in-VM DNS lookup fails while raw egress still works.
func TestLoopbackOnlyDNS(t *testing.T) {
	warp := `DNS configuration

resolver #1
  nameserver[0] : 127.0.2.2
  nameserver[1] : 127.0.2.3
  flags    : Request A records
  reach    : 0x00030002 (Reachable,Local Address,Directly Reachable Address)

resolver #2
  domain   : local
  options  : mdns
`
	normal := `DNS configuration

resolver #1
  search domain[0] : lan
  nameserver[0] : 192.168.1.1
  flags    : Request A records

resolver #2
  domain   : local
  options  : mdns
`
	mixed := `DNS configuration

resolver #1
  nameserver[0] : 127.0.0.1
  nameserver[1] : 8.8.8.8
`
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"warp loopback-only", warp, true},
		{"router resolver", normal, false},
		{"loopback plus public fallback", mixed, false},
		{"empty output", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loopbackOnlyDNS(tc.out); got != tc.want {
				t.Errorf("loopbackOnlyDNS = %v, want %v", got, tc.want)
			}
		})
	}
}
