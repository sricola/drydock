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
		"SandboxImage":  "drydock-sandbox:latest",
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

func TestEnvOverride_TaskMaxRequests_IgnoresNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("task_max_requests: 7\n"), 0o644)
	t.Setenv("DRYDOCK_TASK_MAX_REQUESTS", "-5")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TaskMaxRequests != 7 {
		t.Errorf("negative DRYDOCK_TASK_MAX_REQUESTS should be ignored; got %d, want the file value 7", cfg.TaskMaxRequests)
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := map[string]string{
		"network: \"\"\ngateway_ip: 1.2.3.4\n":                       "network",
		"network: x\ngateway_ip: \"\"\n":                             "gateway_ip",
		"network: x\ngateway_ip: 1.2.3.4\nmax_concurrent_tasks: 0\n": "max_concurrent_tasks",
		"network: x\ngateway_ip: 1.2.3.4\ntask_budget_usd: 0\n":      "task_budget_usd",
		"network: x\ngateway_ip: 1.2.3.4\ntask_timeout: 0s\n":        "task_timeout",
		"network: x\ngateway_ip: 1.2.3.4\ntask_max_requests: -1\n":   "task_max_requests",
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

func TestEnvOverrides_AllOperatorKnobs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	values := map[string]string{
		"DRYDOCK_NETWORK":                  "env-network",
		"DRYDOCK_GW_IP":                    "10.20.30.1",
		"SANDBOX_IMAGE":                    "sandbox:test",
		"DRYDOCK_ANCHOR_IMAGE":             "anchor:test",
		"DRYDOCK_TASK_BUDGET_USD":          "3.25",
		"DRYDOCK_MAX_CONCURRENT_TASKS":     "7",
		"DRYDOCK_DEFAULT_MODEL":            "model-x",
		"DRYDOCK_DEFAULT_AGENT":            "codex",
		"DRYDOCK_ANTHROPIC_AUTH":           "subscription",
		"DRYDOCK_OPENAI_AUTH":              "subscription",
		"DRYDOCK_TASK_MAX_REQUESTS":        "42",
		"DRYDOCK_AGGREGATE_BUDGET_USD":     "9.5",
		"DRYDOCK_MAX_REQUEST_COST_USD":     "0.75",
		"DRYDOCK_AGGREGATE_WINDOW":         "6h",
		"DRYDOCK_PUSH_MAX_RETRIES":         "5",
		"DRYDOCK_PUSH_RETRY_BACKOFF":       "250ms",
		"DRYDOCK_PUSH_FRESH_BRANCH_TRIES":  "4",
		"STAGE_ROOT":                       "/tmp/test-stage",
		"AUDIT_ROOT":                       "/tmp/test-audit",
		"SQUID_RUN_DIR":                    "/tmp/test-squid",
		"BROKER_SOCKET":                    "/tmp/test-broker.sock",
		"BROKER_ADDR":                      "127.0.0.1:8765",
		"DRYDOCK_NO_NOTIFY":                "1",
		"DRYDOCK_LOG_JSON":                 "1",
		"DRYDOCK_STRICT_CONTAINER_VERSION": "1",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}

	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Network != "env-network" || c.GatewayIP != "10.20.30.1" ||
		c.SandboxImage != "sandbox:test" || c.AnchorImage != "anchor:test" {
		t.Errorf("runtime env overrides not applied: %+v", c)
	}
	if c.TaskBudgetUSD != 3.25 || c.MaxConcurrent != 7 || c.TaskMaxRequests != 42 ||
		c.MaxRequestCostUSD != 0.75 || c.AggregateBudgetUSD != 9.5 || c.AggregateWindow != 6*time.Hour {
		t.Errorf("budget/concurrency env overrides not applied: %+v", c)
	}
	if c.DefaultModel != "model-x" || c.DefaultAgent != "codex" ||
		c.AnthropicAuth != "subscription" || c.OpenAIAuth != "subscription" {
		t.Errorf("agent/auth env overrides not applied: %+v", c)
	}
	if c.PushMaxRetries != 5 || c.PushRetryBackoff != 250*time.Millisecond || c.PushFreshBranchTries != 4 {
		t.Errorf("push recovery env overrides not applied: %+v", c)
	}
	if c.StageRoot != "/tmp/test-stage" || c.AuditRoot != "/tmp/test-audit" || c.SquidRunDir != "/tmp/test-squid" ||
		c.Broker.Socket != "/tmp/test-broker.sock" || c.Broker.Addr != "127.0.0.1:8765" {
		t.Errorf("state/listener env overrides not applied: %+v", c)
	}
	if c.Notifications || !c.LogJSON || !c.StrictContainerVersion {
		t.Errorf("boolean env overrides not applied: %+v", c)
	}
}

func TestValidate_RejectsNegativePrice(t *testing.T) {
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"openai_compat:\n" +
		"  base_url: https://api.example.com\n" +
		"  api_key_env: FOO_KEY\n" +
		"  model: m\n" +
		"  prices:\n" +
		"    m: {input: 1.0, output: -2.0}\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte(yaml), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("want negative-price rejection, got %v", err)
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

// expandHome must rewrite the YAML-loaded "~/…" placeholders into real
// paths. Without this, brokerd creates a literal directory named "~".
func TestExpandHome_RewritesTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	yaml := []byte(`network: drydock-egress
gateway_ip: 192.168.66.1
sandbox_image: drydock-sandbox:latest
anchor_image: drydock-anchor:latest
task_budget_usd: 2.0
max_concurrent_tasks: 2
task_timeout: 30m
stage_root: ~/.drydock/stage
audit_root: ~/.drydock/audit
squid_run_dir: ~/.drydock/squid
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, yaml, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, ".drydock", "audit")
	if cfg.AuditRoot != want {
		t.Errorf("AuditRoot = %q, want %q (tilde must expand at load time)", cfg.AuditRoot, want)
	}
	if cfg.StageRoot != filepath.Join(home, ".drydock", "stage") {
		t.Errorf("StageRoot = %q, want ~/.drydock/stage expanded", cfg.StageRoot)
	}
	if cfg.SquidRunDir != filepath.Join(home, ".drydock", "squid") {
		t.Errorf("SquidRunDir = %q, want ~/.drydock/squid expanded", cfg.SquidRunDir)
	}
}

// Defaults() must point under the user's home dir, not /tmp. Audit history
// surviving across reboots and OS housekeeping is the whole point of the
// move; if the default regresses to /tmp this test will catch it.
func TestDefaults_StateDirsUnderHomeNotTmp(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir on this host")
	}
	d := Defaults()
	for _, p := range []string{d.StageRoot, d.AuditRoot, d.SquidRunDir} {
		if !strings.HasPrefix(p, home) {
			t.Errorf("default %q is outside %q — audit history won't survive /tmp cleanup", p, home)
		}
	}
}

// The on-disk template (shipped to $PREFIX/share/drydock/config/config.yaml
// by `make install` and to share/drydock/config/config.yaml in the brew
// tarball) MUST match the embedded SeedTemplate that `WriteSeed` writes
// when the share dir is unreachable. Otherwise an operator who edits the
// on-disk template, deletes their ~/.drydock/config.yaml, and re-runs
// `drydock init` on a machine without share-dir reachability gets a
// different file than they had before — silent drift.
func TestSeedTemplate_MatchesOnDiskTemplate(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for d := dir; d != "/"; d = filepath.Dir(d) {
		if _, gerr := os.Stat(filepath.Join(d, "go.mod")); gerr == nil {
			path = filepath.Join(d, "config", "config.yaml")
			break
		}
	}
	if path == "" {
		t.Skip("could not locate module root; on-disk template can't be checked from this CWD")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != SeedTemplate {
		t.Errorf("config/config.yaml has drifted from SeedTemplate.\n"+
			"Each is a copy of the other; update both together.\n\n"+
			"on disk (first 200 chars): %q\n"+
			"SeedTemplate (first 200): %q",
			truncate200(string(b)), truncate200(SeedTemplate))
	}
}

func truncate200(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}

func TestDefaultAgent_DefaultsToClaude(t *testing.T) {
	if got := Defaults().DefaultAgent; got != "claude" {
		t.Errorf("DefaultAgent default = %q, want claude", got)
	}
}

func TestValidate_RejectsBadAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\ndefault_agent: gpt\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "default_agent") {
		t.Errorf("want default_agent validation error, got %v", err)
	}
}

func TestConfig_AnthropicAuthAndMaxRequests(t *testing.T) {
	t.Setenv("DRYDOCK_ANTHROPIC_AUTH", "subscription")
	t.Setenv("DRYDOCK_TASK_MAX_REQUESTS", "150")
	c, err := Load("/nonexistent.yaml") // defaults + env
	if err != nil {
		t.Fatal(err)
	}
	if c.AnthropicAuth != "subscription" {
		t.Errorf("AnthropicAuth=%q", c.AnthropicAuth)
	}
	if c.TaskMaxRequests != 150 {
		t.Errorf("TaskMaxRequests=%d", c.TaskMaxRequests)
	}
}

func TestConfig_AnthropicAuthDefaultsToApiKey(t *testing.T) {
	c, _ := Load("/nonexistent.yaml")
	if c.AnthropicAuth != "api_key" {
		t.Errorf("default AnthropicAuth=%q, want api_key", c.AnthropicAuth)
	}
}

func TestConfig_OpenAIAuth(t *testing.T) {
	t.Setenv("DRYDOCK_OPENAI_AUTH", "subscription")
	c, err := Load("/nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenAIAuth != "subscription" {
		t.Errorf("OpenAIAuth=%q", c.OpenAIAuth)
	}
}

func TestConfig_OpenAIAuthDefaultsToApiKey(t *testing.T) {
	c, _ := Load("/nonexistent.yaml")
	if c.OpenAIAuth != "api_key" {
		t.Errorf("default OpenAIAuth=%q, want api_key", c.OpenAIAuth)
	}
}

func TestConfig_OpenAIAuthRejectsGarbage(t *testing.T) {
	t.Setenv("DRYDOCK_OPENAI_AUTH", "bogus")
	if _, err := Load("/nonexistent.yaml"); err == nil {
		t.Error("want validate error for openai_auth=bogus")
	}
}

func TestAuthMode(t *testing.T) {
	c := &Config{AnthropicAuth: "subscription", OpenAIAuth: "api_key"}
	if c.AuthMode("anthropic") != "subscription" {
		t.Errorf("anthropic = %q", c.AuthMode("anthropic"))
	}
	if c.AuthMode("openai") != "api_key" {
		t.Errorf("openai = %q", c.AuthMode("openai"))
	}
	if c.AuthMode("nope") != "" {
		t.Errorf("unknown vendor should be empty, got %q", c.AuthMode("nope"))
	}
}

func TestValidate_OpenAICompat(t *testing.T) {
	base := Defaults()
	// Unconfigured (empty base_url) is valid — provider just inactive.
	if err := base.validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	// base_url set but no api_key_env / model -> error.
	c := Defaults()
	c.OpenAICompat.BaseURL = "https://example.test"
	if err := c.validate(); err == nil {
		t.Error("base_url without api_key_env+model must error")
	}
	// non-https base_url (non-localhost) -> error.
	c = Defaults()
	c.OpenAICompat.BaseURL = "http://example.test"
	c.OpenAICompat.APIKeyEnv = "X_KEY"
	c.OpenAICompat.Model = "m"
	if err := c.validate(); err == nil {
		t.Error("non-https non-localhost base_url must error")
	}
	// fully configured https -> ok.
	c.OpenAICompat.BaseURL = "https://example.test"
	if err := c.validate(); err != nil {
		t.Errorf("configured openai_compat must validate: %v", err)
	}
	// localhost http -> ok (http is allowed for localhost).
	c = Defaults()
	c.OpenAICompat.BaseURL = "http://localhost:8080"
	c.OpenAICompat.APIKeyEnv = "X_KEY"
	c.OpenAICompat.Model = "m"
	if err := c.validate(); err != nil {
		t.Errorf("localhost http openai_compat must validate: %v", err)
	}
	// 127.0.0.1 http -> ok (http is allowed for loopback).
	c = Defaults()
	c.OpenAICompat.BaseURL = "http://127.0.0.1:8080"
	c.OpenAICompat.APIKeyEnv = "X_KEY"
	c.OpenAICompat.Model = "m"
	if err := c.validate(); err != nil {
		t.Errorf("127.0.0.1 http openai_compat must validate: %v", err)
	}
}

func TestLockPath(t *testing.T) {
	got := LockPath()
	if got == "" {
		t.Skip("home dir unresolvable in this environment")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("LockPath() = %q, want an absolute path", got)
	}
	if filepath.Base(got) != "brokerd.lock" {
		t.Errorf("LockPath() base = %q, want brokerd.lock", filepath.Base(got))
	}
}

func TestValidate_RejectsNegativeAggregate(t *testing.T) {
	for _, yaml := range []string{
		"network: x\ngateway_ip: 1.2.3.4\naggregate_budget_usd: -1\n",
		"network: x\ngateway_ip: 1.2.3.4\naggregate_window: -5m\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte(yaml), 0o644)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "aggregate") {
			t.Errorf("yaml=%q want aggregate rejection, got %v", yaml, err)
		}
	}
}

func TestAggregateDefaults(t *testing.T) {
	d := Defaults()
	if d.AggregateBudgetUSD != 0 {
		t.Errorf("aggregate_budget_usd default = %v, want 0 (disabled)", d.AggregateBudgetUSD)
	}
	if d.AggregateWindow != 24*time.Hour {
		t.Errorf("aggregate_window default = %v, want 24h", d.AggregateWindow)
	}
}

func TestMaxRequestCost_DefaultAndValidation(t *testing.T) {
	if d := Defaults(); d.MaxRequestCostUSD != 0 {
		t.Errorf("max_request_cost_usd default = %v, want 0 (disabled)", d.MaxRequestCostUSD)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\nmax_request_cost_usd: -1\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_request_cost_usd") {
		t.Errorf("negative max_request_cost_usd should be rejected, got %v", err)
	}
}

func TestPushRetryDefaultsAndValidation(t *testing.T) {
	d := Defaults()
	if d.PushMaxRetries != 3 || d.PushRetryBackoff != time.Second || d.PushFreshBranchTries != 2 {
		t.Errorf("push defaults = %d/%v/%d, want 3/1s/2", d.PushMaxRetries, d.PushRetryBackoff, d.PushFreshBranchTries)
	}
	for _, y := range []string{
		"network: x\ngateway_ip: 1.2.3.4\npush_max_retries: -1\n",
		"network: x\ngateway_ip: 1.2.3.4\npush_retry_backoff: -5s\n",
		"network: x\ngateway_ip: 1.2.3.4\npush_fresh_branch_tries: -2\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte(y), 0o644)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "push_") {
			t.Errorf("yaml=%q want push_ rejection, got %v", y, err)
		}
	}
}
