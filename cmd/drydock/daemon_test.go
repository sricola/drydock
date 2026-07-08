package main

import (
	"strings"
	"testing"
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
