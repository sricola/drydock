package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults_MatchV01EnvFallbacks(t *testing.T) {
	c := Defaults()
	cases := map[string]any{
		"Network":       c.Network,
		"GatewayIP":     c.GatewayIP,
		"SandboxImage":  c.SandboxImage,
		"AnchorImage":   c.AnchorImage,
		"TaskBudgetUSD": c.TaskBudgetUSD,
		"MaxConcurrent": c.MaxConcurrent,
		"TaskTimeout":   c.TaskTimeout,
	}
	wants := map[string]any{
		"Network":       "drydock-egress",
		"GatewayIP":     "192.168.66.1",
		"SandboxImage":  "claude-sandbox:latest",
		"AnchorImage":   "drydock-anchor:latest",
		"TaskBudgetUSD": 2.0,
		"MaxConcurrent": 2,
		"TaskTimeout":   30 * time.Minute,
	}
	for k, want := range wants {
		if cases[k] != want {
			t.Errorf("default %s = %v, want %v", k, cases[k], want)
		}
	}
}

func TestLoad_MissingFile_OK(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.Network != "drydock-egress" {
		t.Errorf("missing file should give defaults; got Network=%q", cfg.Network)
	}
}

func TestLoad_FromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	yaml := `network: alt-net
gateway_ip: 10.0.0.1
task_budget_usd: 4.5
max_concurrent_tasks: 3
notifications: false
log_json: true
broker:
  addr: 127.0.0.1:9000
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Network != "alt-net" || cfg.GatewayIP != "10.0.0.1" {
		t.Errorf("YAML not applied: %+v", cfg)
	}
	if cfg.TaskBudgetUSD != 4.5 || cfg.MaxConcurrent != 3 {
		t.Errorf("numeric YAML fields not applied: budget=%v max=%v", cfg.TaskBudgetUSD, cfg.MaxConcurrent)
	}
	if cfg.Notifications {
		t.Errorf("notifications should be false from YAML")
	}
	if !cfg.LogJSON {
		t.Errorf("log_json should be true from YAML")
	}
	if cfg.Broker.Addr != "127.0.0.1:9000" {
		t.Errorf("broker.addr = %q", cfg.Broker.Addr)
	}
}

func TestEnvOverridesWinOverFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: from-file\n"), 0o644)

	t.Setenv("DRYDOCK_NETWORK", "from-env")
	t.Setenv("DRYDOCK_NO_NOTIFY", "1")
	t.Setenv("DRYDOCK_LOG_JSON", "1")
	t.Setenv("DRYDOCK_STRICT_CONTAINER_VERSION", "1")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network != "from-env" {
		t.Errorf("env should win over file; got Network=%q", cfg.Network)
	}
	if cfg.Notifications {
		t.Errorf("DRYDOCK_NO_NOTIFY=1 should turn off notifications")
	}
	if !cfg.LogJSON {
		t.Errorf("DRYDOCK_LOG_JSON=1 should be respected")
	}
	if !cfg.StrictContainerVersion {
		t.Errorf("DRYDOCK_STRICT_CONTAINER_VERSION=1 should be respected")
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := map[string]string{
		"network: \"\"\ngateway_ip: 1.2.3.4\n":                                 "network",
		"network: x\ngateway_ip: \"\"\n":                                       "gateway_ip",
		"network: x\ngateway_ip: 1.2.3.4\nmax_concurrent_tasks: 0\n":           "max_concurrent_tasks",
		"network: x\ngateway_ip: 1.2.3.4\ntask_budget_usd: 0\n":                "task_budget_usd",
		"network: x\ngateway_ip: 1.2.3.4\ntask_timeout: 0s\n":                  "task_timeout",
	}
	for yaml, wantSubstr := range cases {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte(yaml), 0o644)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("yaml=%q\n  want err containing %q, got %v", yaml, wantSubstr, err)
		}
	}
}

func TestWriteSeed_ValidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := WriteSeed(path); err != nil {
		t.Fatalf("WriteSeed: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("seeded file failed to load: %v", err)
	}
	if cfg.Network != "drydock-egress" {
		t.Errorf("seeded config defaults mismatch: Network=%q", cfg.Network)
	}
	// parent dir at 0700 — defense-in-depth so the file isn't world-readable
	info, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0o700 {
		t.Errorf("parent dir mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestDefaultPath_PointsAtHomeDotDrydock(t *testing.T) {
	p := DefaultPath()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".drydock", "config.yaml")
	if p != want {
		t.Errorf("DefaultPath() = %q, want %q", p, want)
	}
}
