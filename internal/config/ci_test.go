package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The whole point of the ci: block in B1 is that it ships DARK. A stock
// install — no config file, no env — must leave the watch off and every
// retry knob at zero, so brokerd starts no watch goroutine and makes no gh
// call on a timer.
func TestCI_OffByDefault(t *testing.T) {
	d := Defaults()
	if d.CI.Watch {
		t.Error("ci.watch defaults to true; the CI watch must ship off")
	}
	if d.CI.MaxAttempts != 0 {
		t.Errorf("ci.max_attempts default = %d, want 0 (retry off)", d.CI.MaxAttempts)
	}
	if d.CI.PollInterval != DefaultCIPollInterval {
		t.Errorf("ci.poll_interval default = %v, want %v", d.CI.PollInterval, DefaultCIPollInterval)
	}
	if d.CI.WatchTimeout != DefaultCIWatchTimeout {
		t.Errorf("ci.watch_timeout default = %v, want %v", d.CI.WatchTimeout, DefaultCIWatchTimeout)
	}

	// And the same through a real Load with no file and no env.
	t.Setenv("HOME", t.TempDir())
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CI.Watch || c.CI.MaxAttempts != 0 {
		t.Errorf("a stock Load enabled CI: %+v", c.CI)
	}
}

func TestCI_LoadsFromYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	// approval_timeout is mandatory alongside max_attempts > 0 (an unattended
	// retry must not be able to park at a gate forever holding a slot).
	body := "network: x\ngateway_ip: 1.2.3.4\napproval_timeout: 2h\n" +
		"ci:\n  watch: true\n  poll_interval: 45s\n  watch_timeout: 3h\n  max_attempts: 2\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.CI.Watch || c.CI.PollInterval != 45*time.Second ||
		c.CI.WatchTimeout != 3*time.Hour || c.CI.MaxAttempts != 2 {
		t.Errorf("ci block not decoded: %+v", c.CI)
	}
}

func TestCI_EnvOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "network: x\ngateway_ip: 1.2.3.4\napproval_timeout: 2h\n" +
		"ci:\n  watch: false\n  poll_interval: 45s\n  watch_timeout: 3h\n  max_attempts: 1\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DRYDOCK_CI_WATCH", "1")
	t.Setenv("DRYDOCK_CI_POLL_INTERVAL", "90s")
	t.Setenv("DRYDOCK_CI_WATCH_TIMEOUT", "6h")
	t.Setenv("DRYDOCK_CI_MAX_ATTEMPTS", "3")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.CI.Watch || c.CI.PollInterval != 90*time.Second ||
		c.CI.WatchTimeout != 6*time.Hour || c.CI.MaxAttempts != 3 {
		t.Errorf("ci env overrides not applied: %+v", c.CI)
	}
}

// DRYDOCK_CI_WATCH follows the file's exactly-"1" bool idiom: anything else is
// NOT a way to turn the watch on. "true"/"yes"/"0" must all leave it off, so a
// half-remembered spelling can never silently arm a credentialed poll loop.
func TestCI_WatchEnvOnlyExactlyOneEnables(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "yes", "on", "0", "2", " 1", "1 "} {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("DRYDOCK_CI_WATCH", v)
		c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
		if err != nil {
			t.Fatalf("Load(%q): %v", v, err)
		}
		if c.CI.Watch {
			t.Errorf("DRYDOCK_CI_WATCH=%q enabled the watch; only \"1\" may", v)
		}
	}
}

// A set-but-invalid env var must fall through to the yaml/default value rather
// than clobbering it — the same guard shape every other duration/int knob uses.
func TestCI_InvalidEnvFallsThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "network: x\ngateway_ip: 1.2.3.4\napproval_timeout: 2h\n" +
		"ci:\n  watch: true\n  poll_interval: 45s\n  watch_timeout: 3h\n  max_attempts: 2\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DRYDOCK_CI_POLL_INTERVAL", "not-a-duration")
	t.Setenv("DRYDOCK_CI_WATCH_TIMEOUT", "-5m")
	t.Setenv("DRYDOCK_CI_MAX_ATTEMPTS", "-1")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CI.PollInterval != 45*time.Second || c.CI.WatchTimeout != 3*time.Hour || c.CI.MaxAttempts != 2 {
		t.Errorf("invalid ci env vars clobbered the yaml values: %+v", c.CI)
	}
}

func TestCI_ValidateRejects(t *testing.T) {
	cases := map[string]string{
		// negative durations
		"ci:\n  poll_interval: -1s\n": "ci.poll_interval",
		"ci:\n  watch_timeout: -1s\n": "ci.watch_timeout",
		// too-frequent poll: a credentialed loop against the host's gh token
		"ci:\n  poll_interval: 1s\n": "ci.poll_interval",
		// a watch window shorter than a single poll can never conclude
		"ci:\n  watch_timeout: 5s\n": "ci.watch_timeout",
		// negative attempt count
		"ci:\n  max_attempts: -1\n": "ci.max_attempts",
		// the upper bound: a typo must not authorize a 10000-deep retry chain,
		// each attempt of which mints a fresh full task_budget_usd.
		"ci:\n  max_attempts: 10000\n": "ci.max_attempts",
		"ci:\n  max_attempts: 11\n":    "ci.max_attempts",
		// CROSS-FIELD: each value is individually legal and the PAIR is not.
		// The deadline is checked before the dispatch floor, so a timeout at or
		// under the floor makes `no_checks` and `passed` unreachable and every
		// watched task dead-letter. At runtime that is indistinguishable from CI
		// itself being broken, so load is the only place it can be caught.
		"ci:\n  poll_interval: 10m\n  watch_timeout: 5m\n": "ci.watch_timeout",
		// The floor's wall-clock term bites even at the fastest legal poll.
		"ci:\n  poll_interval: 10s\n  watch_timeout: 1m\n": "dispatch floor",
		// Exactly AT the floor is still no good: the deadline check wins ties.
		"ci:\n  poll_interval: 10s\n  watch_timeout: 5m\n": "dispatch floor",
		// And with poll_interval left to its default.
		"ci:\n  watch_timeout: 4m\n": "dispatch floor",
		// CROSS-FIELD, the expensive one: a bounded retry is the daemon's only
		// UNATTENDED task author, and it holds a concurrency slot across the
		// human diff gate it always re-poses. With approval_timeout at its
		// default 0 ("wait forever") an overnight retry parks at a gate nobody
		// is at, for good, and every task submitted after it sits queued. Each
		// value is individually legal; the pair is not.
		"ci:\n  max_attempts: 1\n":                        "approval_timeout",
		"approval_timeout: 0s\nci:\n  max_attempts: 10\n": "approval_timeout",
		// And it bites with the watch off too: max_attempts is inert without
		// ci.watch, but a config that arms the hazard the moment someone flips
		// one bool is not one to accept quietly.
		"ci:\n  watch: false\n  max_attempts: 2\n": "approval_timeout",
	}
	for body, want := range cases {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte("network: x\ngateway_ip: 1.2.3.4\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("yaml=%q\n  want err containing %q, got %v", body, want, err)
		}
	}
}

func TestCI_ValidateAccepts(t *testing.T) {
	bodies := []string{
		// 0 everywhere = "use the built-in default" / "off"
		"ci:\n  watch: false\n  poll_interval: 0s\n  watch_timeout: 0s\n  max_attempts: 0\n",
		// The fastest legal poll with a timeout comfortably past the floor.
		// max_attempts > 0 needs approval_timeout, so it rides along.
		"approval_timeout: 2h\nci:\n  watch: true\n  poll_interval: 10s\n  watch_timeout: 6m\n  max_attempts: 1\n",
		// A slow poll: the floor is then 2 × 5m, still far inside 24h.
		"ci:\n  watch: true\n  poll_interval: 5m\n  watch_timeout: 24h\n",
		"approval_timeout: 30m\nci:\n  max_attempts: 10\n",
	}
	for _, body := range bodies {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte("network: x\ngateway_ip: 1.2.3.4\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err != nil {
			t.Errorf("yaml=%q rejected: %v", body, err)
		}
	}
}

// A misspelled ci key must be a hard error, not a silent no-op — the same
// KnownFields(true) contract every other security control gets. Typing
// `ci: {wach: true}` must not leave the operator believing the watch is on.
func TestCI_RejectsMisspelledKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("ci:\n  wach: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("Load accepted a misspelled ci key (wach); want a parse error")
	}
}

// The seed template `drydock init` writes must carry the block, and the values
// it documents must be the values Defaults() actually applies.
func TestCI_SeedTemplateMatchesDefaults(t *testing.T) {
	if !strings.Contains(SeedTemplate, "\nci:\n") {
		t.Fatal("SeedTemplate has no ci: block")
	}
	p := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := WriteSeed(p); err != nil {
		t.Fatalf("WriteSeed: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("seeded file failed to load: %v", err)
	}
	d := Defaults()
	if c.CI != d.CI {
		t.Errorf("seeded ci block %+v != Defaults() %+v", c.CI, d.CI)
	}
	if c.CI.Watch {
		t.Error("the seeded config enables the CI watch; it must ship off")
	}
}

// Every ci field must carry a provenanceTable entry whose guard mirrors
// applyEnvOverrides exactly. If a guard diverges, `drydock policy explain`
// silently reports the wrong source for a security control.
func TestExplain_CIProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "network: x\ngateway_ip: 1.2.3.4\napproval_timeout: 2h\n" +
		"ci:\n  watch: true\n  poll_interval: 45s\n  watch_timeout: 3h\n  max_attempts: 2\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// One env var valid (wins), one env var INVALID (must fall through to yaml).
	t.Setenv("DRYDOCK_CI_MAX_ATTEMPTS", "3")
	t.Setenv("DRYDOCK_CI_POLL_INTERVAL", "nonsense")

	fields, _, err := Explain(p)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	want := []struct {
		name    string
		yamlKey string
		envVar  string
		value   string
		src     Source
	}{
		{"CI.Watch", "ci.watch", "DRYDOCK_CI_WATCH", "true", SourceYAML},
		{"CI.PollInterval", "ci.poll_interval", "DRYDOCK_CI_POLL_INTERVAL", "45s", SourceYAML},
		{"CI.WatchTimeout", "ci.watch_timeout", "DRYDOCK_CI_WATCH_TIMEOUT", "3h0m0s", SourceYAML},
		{"CI.MaxAttempts", "ci.max_attempts", "DRYDOCK_CI_MAX_ATTEMPTS", "3", SourceEnv},
	}
	for _, w := range want {
		f, ok := fieldByName(fields, w.name)
		if !ok {
			t.Errorf("field %q missing from Explain — `drydock policy explain` would not show it", w.name)
			continue
		}
		if f.YAMLKey != w.yamlKey || f.EnvVar != w.envVar {
			t.Errorf("%s = key %q env %q, want %q / %q", w.name, f.YAMLKey, f.EnvVar, w.yamlKey, w.envVar)
		}
		if f.Value != w.value {
			t.Errorf("%s value = %q, want %q", w.name, f.Value, w.value)
		}
		if f.Source != w.src {
			t.Errorf("%s source = %q, want %q", w.name, f.Source, w.src)
		}
	}
}

// With nothing set, every ci field must attribute to default — otherwise the
// explain table would claim an operator turned something on that they did not.
func TestExplain_CIDefaultProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, wantVal := range map[string]string{
		"CI.Watch":        "false",
		"CI.PollInterval": DefaultCIPollInterval.String(),
		"CI.WatchTimeout": DefaultCIWatchTimeout.String(),
		"CI.MaxAttempts":  "0",
	} {
		f, ok := fieldByName(fields, name)
		if !ok {
			t.Errorf("field %q missing from Explain", name)
			continue
		}
		if f.Source != SourceDefault {
			t.Errorf("%s source = %q, want %q", name, f.Source, SourceDefault)
		}
		if f.Value != wantVal {
			t.Errorf("%s value = %q, want %q", name, f.Value, wantVal)
		}
	}
}

// The bool guard must be the exactly-"1" shape, not "non-empty": otherwise
// DRYDOCK_CI_WATCH=0 would report SourceEnv while applyEnvOverrides ignored it,
// i.e. explain would claim the watch came from the environment when it did not.
func TestExplain_CIWatchGuardMirrorsEnvOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DRYDOCK_CI_WATCH", "0")
	fields, cfg, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CI.Watch {
		t.Fatal("DRYDOCK_CI_WATCH=0 enabled the watch")
	}
	f, _ := fieldByName(fields, "CI.Watch")
	if f.Source != SourceDefault {
		t.Errorf("CI.Watch source = %q with DRYDOCK_CI_WATCH=0, want %q", f.Source, SourceDefault)
	}
}
