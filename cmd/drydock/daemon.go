package main

import (
	"bytes"
	"os"
	"path/filepath"
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
			continue // openai-compat: its key env is loaded from api-keys.env below via APIKeyEnv when set
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
