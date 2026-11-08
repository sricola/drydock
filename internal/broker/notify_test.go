package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notifyMac is the only place brokerd shells out on the operator's behalf at
// a gate. It must stay silent when notifications are off and must escape the
// text it interpolates into AppleScript (task text is untrusted).
func TestNotifyMac_HonoursOptOutAndEscapesText(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$2\" >> '" + log + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := &Broker{Notify: false}
	b.notifyMac("t", "b")
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("notifications off, yet osascript was invoked")
	}

	b.Notify = true
	b.notifyMac(`ti"tle`, `say "hi" \ bye`)
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("notifications on, osascript not invoked: %v", err)
	}
	want := `display notification "say \"hi\" \\ bye" with title "ti\"tle"`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("script = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}
