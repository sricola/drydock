package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/config"
)

func TestRenderPlist_Golden(t *testing.T) {
	got, err := renderPlist("test.label", "/usr/local/bin/brokerd", "/Users/x/.drydock/logs/brokerd.log", "/Users/x")
	if err != nil {
		t.Fatal(err)
	}
	const want = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>test.label</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/brokerd</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>StandardOutPath</key>
	<string>/Users/x/.drydock/logs/brokerd.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/x/.drydock/logs/brokerd.log</string>
	<key>WorkingDirectory</key>
	<string>/Users/x</string>
</dict>
</plist>
`
	if string(got) != want {
		t.Errorf("plist mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestDaemonPlistPath_UsesLabel(t *testing.T) {
	p := daemonPlistPath()
	if !strings.HasSuffix(p, "/Library/LaunchAgents/"+daemonLabel+".plist") {
		t.Errorf("plist path %q must end with /Library/LaunchAgents/%s.plist", p, daemonLabel)
	}
}

func TestLaunchdCredentialAvailable(t *testing.T) {
	noEnv := func(string) string { return "" }
	noFile := func(string) bool { return false }
	cases := []struct {
		name      string
		fileKeys  map[string]string
		oauth     func(string) bool
		getenv    func(string) string
		subs      bool // set anthropic_auth: subscription
		wantOK    bool
		wantShell string
	}{
		{"nothing anywhere", map[string]string{}, noFile, noEnv, false, false, ""},
		{"file key present", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"}, noFile, noEnv, false, true, ""},
		{"subscription with oauth file", map[string]string{}, func(f string) bool { return f == "claude-oauth.json" }, noEnv, true, true, ""},
		{"subscription set but oauth file missing", map[string]string{}, noFile, noEnv, true, false, ""},
		{"env-only key fails and is named", map[string]string{}, noFile,
			func(k string) string {
				if k == "ANTHROPIC_API_KEY" {
					return "sk-ant-x"
				}
				return ""
			}, false, false, "ANTHROPIC_API_KEY"},
		{"file key for second provider passes despite env-only first",
			map[string]string{"OPENAI_API_KEY": "sk-openai-x"}, noFile,
			func(k string) string {
				if k == "ANTHROPIC_API_KEY" {
					return "sk-ant-x"
				}
				return ""
			}, false, true, ""},
		{"gemini file key passes", map[string]string{"GEMINI_API_KEY": "g-x"}, noFile, noEnv, false, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Defaults()
			if c.subs {
				cfg.AnthropicAuth = "subscription"
			}
			ok, shell := launchdCredentialAvailable(cfg, c.fileKeys, c.oauth, c.getenv)
			if ok != c.wantOK || shell != c.wantShell {
				t.Errorf("got (ok=%v, shell=%q), want (ok=%v, shell=%q)", ok, shell, c.wantOK, c.wantShell)
			}
		})
	}
}

const launchctlRunningFixture = `so.sri.drydock.brokerd = {
	active count = 1
	path = /Users/x/Library/LaunchAgents/so.sri.drydock.brokerd.plist
	type = LaunchAgent
	state = running

	program = /usr/local/bin/brokerd
	pid = 4242

	spawn type = daemon (3)
}`

const launchctlStoppedFixture = `so.sri.drydock.brokerd = {
	active count = 0
	path = /Users/x/Library/LaunchAgents/so.sri.drydock.brokerd.plist
	state = not running

	last exit code = 1
}`

func TestParseLaunchdState(t *testing.T) {
	got := parseLaunchdState(launchctlRunningFixture)
	if !got.Running || got.PID != "4242" {
		t.Errorf("running fixture: got %+v, want Running=true PID=4242", got)
	}
	got = parseLaunchdState(launchctlStoppedFixture)
	if got.Running || got.LastExit != "1" {
		t.Errorf("stopped fixture: got %+v, want Running=false LastExit=1", got)
	}
	// Garbage in → best-effort zero values, no panic.
	got = parseLaunchdState("launchctl print format changed entirely")
	if got.Running || got.PID != "" || got.LastExit != "" {
		t.Errorf("garbage: got %+v, want zero-value state", got)
	}
}

func TestWaitBrokerHealthy_Timeout(t *testing.T) {
	// Nothing listening → must return false at the deadline, not hang.
	t.Setenv("BROKER_SOCKET", filepath.Join(t.TempDir(), "nope.sock"))
	start := time.Now()
	if waitBrokerHealthy(600 * time.Millisecond) {
		t.Fatal("no broker is listening; want false")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("returned far past the deadline")
	}
}
