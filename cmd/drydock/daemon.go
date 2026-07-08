package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"drydock/internal/brokerclient"
	"drydock/internal/config"
	"drydock/internal/provider"
)

// daemonLabel is the launchd job label; the plist file is named after it.
const daemonLabel = "so.sri.drydock.brokerd"

// Func vars (not consts) so tests can override path resolution if ever
// needed; the integration round-trip instead passes throwaway label/paths
// directly to renderPlist and the launchctl wrappers.
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
			continue // config-built providers (openai-compat): the config only names a key env var; if that var is one of the registry-known names it's checked via its own row, and custom names can't be stored host-side — fail closed here
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
	if strings.Contains(s, "No such process") || strings.Contains(s, "not find") {
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
		return string(out), fmt.Errorf("launchctl print %s: %s", label, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// waitBrokerHealthy polls GET /healthz until 200 or the deadline.
func waitBrokerHealthy(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		c, base := brokerclient.New(nil, 2*time.Second)
		if resp, err := c.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log at " + path + ")"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func runDaemon(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: drydock daemon install|uninstall|status")
		os.Exit(2)
	}
	switch args[0] {
	case "install":
		daemonInstall()
	case "uninstall":
		daemonUninstall()
	case "status":
		daemonStatus()
	default:
		fmt.Fprintf(os.Stderr, "drydock daemon: unknown subcommand %q (install|uninstall|status)\n", args[0])
		os.Exit(2)
	}
}

func daemonInstall() {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil || cfg == nil {
		cfg = config.Defaults()
	}
	fileKeys, _ := config.LoadAPIKeys(config.APIKeysPath())
	oauthExists := func(name string) bool {
		_, err := os.Stat(filepath.Join(config.Dir(), name))
		return err == nil
	}
	ok, shellOnly := launchdCredentialAvailable(cfg, fileKeys, oauthExists, os.Getenv)
	if !ok {
		fmt.Fprintln(os.Stderr, "drydock daemon install: no credential brokerd can see under launchd.")
		fmt.Fprintln(os.Stderr, "launchd does not inherit your shell env — keys must live host-side:")
		if shellOnly != "" {
			fmt.Fprintf(os.Stderr, "  %s is set in your shell but invisible to launchd.\n", shellOnly)
		}
		fmt.Fprintln(os.Stderr, "  fix: run `drydock setup` to store keys in ~/.drydock/api-keys.env (0600),")
		fmt.Fprintln(os.Stderr, "  or `drydock auth claude|codex` for subscription mode.")
		os.Exit(1)
	}

	brokerd, err := findBrokerd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: %v\n  build it: make build && make install\n", err)
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: resolve home: %v\n", err)
		os.Exit(1)
	}
	logPath := daemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: create log dir: %v\n", err)
		os.Exit(1)
	}
	plist, err := renderPlist(daemonLabel, brokerd, logPath, home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: render plist: %v\n", err)
		os.Exit(1)
	}
	pp := daemonPlistPath()
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(pp, plist, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: write plist: %v\n", err)
		os.Exit(1)
	}

	// bootout first so re-install is upgrade/restart; absent job is fine.
	if err := launchctlBootout(daemonLabel); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: %v\n", err)
		os.Exit(1)
	}
	if err := launchctlBootstrap(pp); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: %v\n", err)
		os.Exit(1)
	}
	if err := launchctlKickstart(daemonLabel); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon install: %v\n", err)
		os.Exit(1)
	}

	if !waitBrokerHealthy(15 * time.Second) {
		fmt.Fprintln(os.Stderr, "drydock daemon install: brokerd did not become healthy within 15s.")
		fmt.Fprintln(os.Stderr, "launchd will keep retrying (KeepAlive). Last log lines:")
		fmt.Fprintln(os.Stderr, tailFile(logPath, 20))
		os.Exit(1)
	}
	// Socket is healthy — cross-check that the launchd job itself is running so
	// a foreground brokerd holding the flock can't mask a crash-looping daemon.
	launchdRunning := false
	{
		deadline := time.Now().Add(3 * time.Second)
		for {
			if out, err := launchctlPrint(daemonLabel); err == nil {
				if parseLaunchdState(out).Running {
					launchdRunning = true
					break
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !launchdRunning {
		fmt.Fprintln(os.Stderr, "drydock daemon install: the broker socket is healthy but the launchd job is not running —")
		fmt.Fprintln(os.Stderr, "another brokerd (a foreground `drydock start`?) is holding the lock. The daemon will keep")
		fmt.Fprintln(os.Stderr, "retrying (KeepAlive) and take over when that process exits. Stop it with ^C, then re-run")
		fmt.Fprintln(os.Stderr, "`drydock daemon status` to confirm the daemon took over.")
		os.Exit(1)
	}
	fmt.Printf("brokerd installed and running (label %s)\n  socket: %s\n  logs:   %s\n", daemonLabel, brokerclient.ResolveSocketPath(), logPath)
	fmt.Println("NOTE: no aggregate spend cap yet — per-task budgets only. See the daemon docs.")
}

func daemonUninstall() {
	pp := daemonPlistPath()
	if _, err := os.Stat(pp); os.IsNotExist(err) {
		fmt.Println("drydock daemon: not installed (no plist at " + pp + ")")
		return
	}
	if err := launchctlBootout(daemonLabel); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon uninstall: %v\n", err)
		os.Exit(1)
	}
	if err := os.Remove(pp); err != nil {
		fmt.Fprintf(os.Stderr, "drydock daemon uninstall: remove plist: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("brokerd LaunchAgent removed. A foreground `drydock start` still works as before.")
}

func daemonStatus() {
	pp := daemonPlistPath()
	if _, err := os.Stat(pp); os.IsNotExist(err) {
		fmt.Println("daemon: not installed — run `drydock daemon install`")
		os.Exit(1)
	}
	out, err := launchctlPrint(daemonLabel)
	if err != nil {
		fmt.Printf("daemon: plist present but job not loaded (%v)\n  re-run `drydock daemon install`\n", err)
		os.Exit(1)
	}
	st := parseLaunchdState(out)
	switch {
	case st.Running:
		fmt.Printf("daemon: running (pid %s)\n", st.PID)
	case st.LastExit != "":
		fmt.Printf("daemon: not running (last exit code %s)\n", st.LastExit)
	default:
		fmt.Println("daemon: loaded, state unknown — raw launchctl output:")
		fmt.Println(out)
	}
	if waitBrokerHealthy(2 * time.Second) {
		fmt.Println("broker: healthy on " + brokerclient.ResolveSocketPath())
	} else {
		fmt.Println("broker: NOT responding on " + brokerclient.ResolveSocketPath())
	}
	fmt.Println("logs:   " + daemonLogPath())
}
