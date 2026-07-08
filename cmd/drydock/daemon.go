package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"drydock/internal/config"
	"drydock/internal/provider"
)

// daemonLabel is the launchd job label; the plist file is named after it.
const daemonLabel = "so.sri.drydock.brokerd"

// Func vars (not consts) so the integration round-trip can point install at a
// throwaway label/paths and never touch a real installed daemon.
var (
	launchAgentsDir = func() string {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(h, "Library", "LaunchAgents")
	}
	daemonLogPath = func() string { return filepath.Join(config.Dir(), "logs", "brokerd.log") }
)

func daemonPlistPath() string { return filepath.Join(launchAgentsDir(), daemonLabel+".plist") }

// Paths and labels never contain XML metacharacters (&<>), so text/template
// without escaping is safe here; a home dir containing them would break far
// more than this plist.
var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BrokerdPath}}</string>
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
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
	<key>WorkingDirectory</key>
	<string>{{.Home}}</string>
</dict>
</plist>
`))

func renderPlist(label, brokerdPath, logPath, home string) ([]byte, error) {
	var b bytes.Buffer
	err := plistTmpl.Execute(&b, struct{ Label, BrokerdPath, LogPath, Home string }{label, brokerdPath, logPath, home})
	return b.Bytes(), err
}

// launchdCredentialAvailable checks the credential sources brokerd will see
// UNDER LAUNCHD — which never inherits the shell env. Passing requires a key
// in ~/.drydock/api-keys.env or, for a vendor in subscription mode, its OAuth
// file on disk. shellOnly names an env var that would have satisfied
// `drydock start` but is invisible to launchd, so install's error message can
// point at the exact fix.
func launchdCredentialAvailable(cfg *config.Config, fileKeys map[string]string, oauthExists func(filename string) bool, getenv func(string) string) (ok bool, shellOnly string) {
	for _, p := range provider.Registry {
		if p.ConfigBuilt {
			continue // config-built providers (openai-compat) have no registry credential; brokerd reads their key from the openai_compat config block
		}
		if cfg.AuthMode(p.Vendor) == "subscription" {
			if p.OAuthFile != "" && oauthExists(p.OAuthFile) {
				return true, ""
			}
			continue
		}
		if fileKeys[p.APIKeyEnv] != "" {
			return true, ""
		}
		if getenv(p.APIKeyEnv) != "" {
			shellOnly = p.APIKeyEnv
		}
	}
	return false, shellOnly
}

// launchdState is what `drydock daemon status` needs from `launchctl print`.
// Parsing is best-effort: launchctl's output is not a stable API, so unknown
// formats degrade to zero values rather than errors (status shows raw state).
type launchdState struct {
	Running  bool
	PID      string
	LastExit string
}

func parseLaunchdState(out string) launchdState {
	var s launchdState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "state = running":
			s.Running = true
		case strings.HasPrefix(line, "pid = "):
			s.PID = strings.TrimPrefix(line, "pid = ")
		case strings.HasPrefix(line, "last exit code = "):
			s.LastExit = strings.TrimPrefix(line, "last exit code = ")
		}
	}
	return s
}

// gui domain target for the current user, e.g. "gui/501".
func launchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func launchctlBootstrap(plistPath string) error {
	out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// launchctlBootout unloads the job. "No such process"-style failures are not
// errors: bootout is used as "make sure it isn't loaded" before bootstrap and
// during uninstall, both of which are happy with an already-absent job.
func launchctlBootout(label string) error {
	out, err := exec.Command("launchctl", "bootout", launchdDomain()+"/"+label).CombinedOutput()
	if err == nil {
		return nil
	}
	s := string(out)
	if strings.Contains(s, "No such process") || strings.Contains(s, "not find") || strings.Contains(s, "3: ") {
		return nil
	}
	return fmt.Errorf("launchctl bootout: %s", strings.TrimSpace(s))
}

func launchctlKickstart(label string) error {
	out, err := exec.Command("launchctl", "kickstart", launchdDomain()+"/"+label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func launchctlPrint(label string) (string, error) {
	out, err := exec.Command("launchctl", "print", launchdDomain()+"/"+label).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl print: job %s not loaded", label)
	}
	return string(out), nil
}
