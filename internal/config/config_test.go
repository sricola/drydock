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
		"DRYDOCK_TASK_MAX_INFLIGHT":        "3",
		"DRYDOCK_AGGREGATE_BUDGET_USD":     "9.5",
		"DRYDOCK_MAX_REQUEST_COST_USD":     "0.75",
		"DRYDOCK_AGGREGATE_WINDOW":         "6h",
		"DRYDOCK_PUSH_MAX_RETRIES":         "5",
		"DRYDOCK_PUSH_RETRY_BACKOFF":       "250ms",
		"DRYDOCK_PUSH_FRESH_BRANCH_TRIES":  "4",
		"DRYDOCK_CACHE_QUOTA_GB":           "35",
		"STAGE_ROOT":                       "/tmp/test-stage",
		"AUDIT_ROOT":                       "/tmp/test-audit",
		"SQUID_RUN_DIR":                    "/tmp/test-squid",
		"DRYDOCK_CACHE_ROOT":               "/tmp/test-cache",
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
		c.TaskMaxInFlight != 3 ||
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
		c.CacheRoot != "/tmp/test-cache" ||
		c.Broker.Socket != "/tmp/test-broker.sock" || c.Broker.Addr != "127.0.0.1:8765" {
		t.Errorf("state/listener env overrides not applied: %+v", c)
	}
	if c.CacheQuotaGB != 35 {
		t.Errorf("DRYDOCK_CACHE_QUOTA_GB not applied: %d", c.CacheQuotaGB)
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
	for _, p := range []string{d.StageRoot, d.AuditRoot, d.SquidRunDir, d.CacheRoot} {
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

func TestLoad_RejectsTrailingYAMLDocument(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	// The second document carries a security-relevant field the operator
	// believes is active; silently ignoring it would fail open (F-08).
	body := "task_budget_usd: 2.0\n---\naggregate_budget_usd: 100\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("Load with trailing document: got %v, want trailing-document rejection", err)
	}
}

func TestStageQuotaGB_DefaultEnvValidate(t *testing.T) {
	if got := Defaults().StageQuotaGB; got != 8 {
		t.Errorf("Defaults().StageQuotaGB = %d, want 8", got)
	}
	t.Setenv("DRYDOCK_STAGE_QUOTA_GB", "16")
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StageQuotaGB != 16 {
		t.Errorf("env override: StageQuotaGB = %d, want 16", c.StageQuotaGB)
	}
	c.StageQuotaGB = -1
	if err := c.validate(); err == nil {
		t.Error("validate accepted stage_quota_gb: -1, want error")
	}
}

func TestCacheRoot_DefaultExpandEnv(t *testing.T) {
	home, _ := os.UserHomeDir()
	// Default: ~/.drydock/cache, same shape as stage/audit/squid.
	if got, want := Defaults().CacheRoot, filepath.Join(home, ".drydock", "cache"); got != want {
		t.Errorf("Defaults().CacheRoot = %q, want %q", got, want)
	}
	// A yaml "~/..." value must expand at load time.
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\ncache_root: ~/.drydock/cache\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, ".drydock", "cache"); c.CacheRoot != want {
		t.Errorf("CacheRoot = %q, want %q (tilde must expand at load time)", c.CacheRoot, want)
	}
	// Env override wins over file.
	t.Setenv("DRYDOCK_CACHE_ROOT", "/tmp/test-cache")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheRoot != "/tmp/test-cache" {
		t.Errorf("env override: CacheRoot = %q, want /tmp/test-cache", c.CacheRoot)
	}
}

func TestCacheQuotaGB_DefaultEnvValidate(t *testing.T) {
	if got := Defaults().CacheQuotaGB; got != 20 {
		t.Errorf("Defaults().CacheQuotaGB = %d, want 20", got)
	}
	// A negative yaml value must be rejected at Load (checked before the env
	// override below, which would win over the file value).
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("network: x\ngateway_ip: 1.2.3.4\ncache_quota_gb: -3\n"), 0o644)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "cache_quota_gb") {
		t.Errorf("want cache_quota_gb rejection, got %v", err)
	}
	t.Setenv("DRYDOCK_CACHE_QUOTA_GB", "40")
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheQuotaGB != 40 {
		t.Errorf("env override: CacheQuotaGB = %d, want 40", c.CacheQuotaGB)
	}
	c.CacheQuotaGB = -1
	if err := c.validate(); err == nil {
		t.Error("validate accepted cache_quota_gb: -1, want error")
	}
}

func TestVerifyConfig_LoadsAndValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"verify:\n  repos:\n    \"github.com/o/r\":\n" +
		"      commands:\n        - [\"go\", \"test\", \"./...\"]\n" +
		"      timeout: 5m\n      required: true\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vr, ok := c.Verify.Repos["github.com/o/r"]
	if !ok || !vr.Required || vr.Timeout != 5*time.Minute ||
		len(vr.Commands) != 1 || strings.Join(vr.Commands[0], " ") != "go test ./..." {
		t.Errorf("verify repo = %+v ok=%v", vr, ok)
	}
}

func TestVerifyConfig_Rejects(t *testing.T) {
	base := "network: x\ngateway_ip: 1.2.3.4\nverify:\n  repos:\n"
	cases := map[string]string{
		// Non-canonical key would silently never match a task — fail loudly at load.
		base + "    \"https://github.com/o/r.git\":\n      commands: [[\"go\", \"test\"]]\n": "verify",
		base + "    \"github.com/o/r\":\n      commands: []\n":                               "verify",
		base + "    \"github.com/o/r\":\n      commands: [[]]\n":                             "verify",
		base + "    \"github.com/o/r\":\n      commands: [[\"go\"]]\n      timeout: -1s\n":   "verify",
	}
	for yaml, wantSubstr := range cases {
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("Load(%q) err = %v, want %q", yaml, err, wantSubstr)
		}
	}
}

func TestDiffPolicy_LoadsAndValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"diff_policy:\n  max_files_changed: 50\n  max_lines_changed: 2000\n" +
		"  blocked_paths: [\"**/*.pem\", \".github/workflows/**\"]\n" +
		"  second_look_paths: [\"**/Dockerfile\"]\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dp := c.DiffPolicy
	if dp.MaxFilesChanged != 50 || dp.MaxLinesChanged != 2000 ||
		len(dp.BlockedPaths) != 2 || len(dp.SecondLookPaths) != 1 {
		t.Errorf("diff_policy = %+v", dp)
	}
}

func TestDiffPolicy_ZeroValueDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dp := c.DiffPolicy
	if dp.MaxFilesChanged != 0 || dp.MaxLinesChanged != 0 ||
		len(dp.BlockedPaths) != 0 || len(dp.SecondLookPaths) != 0 {
		t.Errorf("diff_policy zero value = %+v, want all-zero (disabled)", dp)
	}
}

func TestProfiles_LoadsAndValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"profiles:\n  repos:\n    \"github.com/o/r\":\n" +
		"      setup:\n        - [\"npm\", \"ci\"]\n" +
		"      readiness:\n        - [\"curl\", \"-fsS\", \"localhost:3000\"]\n" +
		"      timeout: 10m\n" +
		"      cache: true\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sp, ok := c.Profiles.Repos["github.com/o/r"]
	if !ok || sp.Timeout != 10*time.Minute ||
		len(sp.Setup) != 1 || strings.Join(sp.Setup[0], " ") != "npm ci" ||
		len(sp.Readiness) != 1 || strings.Join(sp.Readiness[0], " ") != "curl -fsS localhost:3000" {
		t.Errorf("profiles repo = %+v ok=%v", sp, ok)
	}
	if !sp.Cache {
		t.Errorf("profiles repo cache = %v, want true (per-repo opt-in must load)", sp.Cache)
	}
}

// The cache opt-in defaults to off: a profile that doesn't mention `cache`
// keeps today's from-scratch setup behavior.
func TestSetupProfile_CacheDefaultsOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"profiles:\n  repos:\n    \"github.com/o/r\":\n" +
		"      setup:\n        - [\"npm\", \"ci\"]\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Profiles.Repos["github.com/o/r"].Cache {
		t.Error("cache must default to false (opt-in)")
	}
}

func TestProfiles_ZeroValueDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Profiles.Repos) != 0 {
		t.Errorf("profiles zero value = %+v, want empty (disabled)", c.Profiles.Repos)
	}
}

func TestProfiles_Rejects(t *testing.T) {
	base := "network: x\ngateway_ip: 1.2.3.4\nprofiles:\n  repos:\n"
	cases := map[string]string{
		// Non-canonical key would silently never match a task — fail loudly at load.
		base + "    \"https://github.com/o/r.git\":\n      setup: [[\"npm\", \"ci\"]]\n": "profiles",
		// A profile with no setup commands is pointless — reject.
		base + "    \"github.com/o/r\":\n      setup: []\n":                                 "profiles",
		base + "    \"github.com/o/r\":\n      setup: [[]]\n":                               "profiles",
		base + "    \"github.com/o/r\":\n      setup: [[\"\"]]\n":                           "profiles",
		base + "    \"github.com/o/r\":\n      setup: [[\"npm\"]]\n      readiness: [[]]\n": "profiles",
		base + "    \"github.com/o/r\":\n      setup: [[\"npm\"]]\n      timeout: -1s\n":    "profiles",
	}
	for yaml, wantSubstr := range cases {
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("Load(%q) err = %v, want %q", yaml, err, wantSubstr)
		}
	}
}

func TestDiffPolicy_Rejects(t *testing.T) {
	base := "network: x\ngateway_ip: 1.2.3.4\ndiff_policy:\n"
	cases := map[string]string{
		base + "  max_files_changed: -1\n":       "diff_policy.max_files_changed",
		base + "  max_lines_changed: -1\n":       "diff_policy.max_lines_changed",
		base + "  blocked_paths: [\"a[b\"]\n":    "diff_policy.blocked_paths[0]",
		base + "  blocked_paths: [\"\"]\n":       "diff_policy.blocked_paths[0]",
		base + "  second_look_paths: [\"x[\"]\n": "diff_policy.second_look_paths[0]",
		// Trailing-slash patterns are silently inert in the matcher
		// ("dir/" never matches anything); they must be a loud config
		// error steering the operator to "dir/**".
		base + "  blocked_paths: [\"dir/\"]\n":   `dir/**`,
		base + "  second_look_paths: [\"a/\"]\n": `a/**`,
	}
	for yaml, want := range cases {
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) err=%v want substring %q", yaml, err, want)
		}
	}
}

// --- the global usage ceiling (plan G1/G7): config surface ---

// OFF BY DEFAULT is the identity claim the whole feature rests on (G7): a
// stock install must resolve both limbs to 0, so brokerd opens no ledger,
// creates no file, and behaves exactly as it did before the ceiling existed.
func TestGlobalCeiling_DefaultsAreOff(t *testing.T) {
	d := Defaults()
	if d.GlobalBudgetUSD != 0 {
		t.Errorf("global_budget_usd default = %v, want 0 (off)", d.GlobalBudgetUSD)
	}
	if d.GlobalMaxTasks != 0 {
		t.Errorf("global_max_tasks default = %d, want 0 (off)", d.GlobalMaxTasks)
	}
	if d.GlobalWindow != 24*time.Hour {
		t.Errorf("global_window default = %v, want 24h", d.GlobalWindow)
	}
}

// The shipped seed template must also resolve to "off": SeedTemplate is what
// `drydock init` writes, so a seeded value of anything but 0 would arm a
// financial control on a first-time install.
func TestGlobalCeiling_SeedTemplateShipsOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(SeedTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("seeded config rejected: %v", err)
	}
	if c.GlobalBudgetUSD != 0 || c.GlobalMaxTasks != 0 {
		t.Errorf("seeded ceiling = $%v / %d tasks, want 0/0 (the feature ships OFF)",
			c.GlobalBudgetUSD, c.GlobalMaxTasks)
	}
	if c.GlobalWindow != 24*time.Hour {
		t.Errorf("seeded global_window = %v, want 24h", c.GlobalWindow)
	}
}

func TestGlobalCeiling_LoadsFromYAML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"+
		"global_budget_usd: 25.5\nglobal_max_tasks: 40\nglobal_window: 6h\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GlobalBudgetUSD != 25.5 || c.GlobalMaxTasks != 40 || c.GlobalWindow != 6*time.Hour {
		t.Errorf("loaded ceiling = %v/%d/%v, want 25.5/40/6h",
			c.GlobalBudgetUSD, c.GlobalMaxTasks, c.GlobalWindow)
	}
}

func TestGlobalCeiling_EnvOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DRYDOCK_GLOBAL_BUDGET_USD", "12.25")
	t.Setenv("DRYDOCK_GLOBAL_MAX_TASKS", "9")
	t.Setenv("DRYDOCK_GLOBAL_WINDOW", "90m")
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"+
		"global_budget_usd: 1\nglobal_max_tasks: 2\nglobal_window: 1h\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GlobalBudgetUSD != 12.25 || c.GlobalMaxTasks != 9 || c.GlobalWindow != 90*time.Minute {
		t.Errorf("env-overridden ceiling = %v/%d/%v, want 12.25/9/90m",
			c.GlobalBudgetUSD, c.GlobalMaxTasks, c.GlobalWindow)
	}
}

// A set-but-INVALID env var must fall through to the yaml/default value rather
// than silently disabling the limb — the guards are >= 0 / parseable, exactly
// as applyEnvOverrides writes them.
func TestGlobalCeiling_EnvIgnoresInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DRYDOCK_GLOBAL_BUDGET_USD", "-5")
	t.Setenv("DRYDOCK_GLOBAL_MAX_TASKS", "notanumber")
	t.Setenv("DRYDOCK_GLOBAL_WINDOW", "24")
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"+
		"global_budget_usd: 7\nglobal_max_tasks: 8\nglobal_window: 3h\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GlobalBudgetUSD != 7 || c.GlobalMaxTasks != 8 || c.GlobalWindow != 3*time.Hour {
		t.Errorf("invalid env must not win: got %v/%d/%v, want the yaml 7/8/3h",
			c.GlobalBudgetUSD, c.GlobalMaxTasks, c.GlobalWindow)
	}
}

func TestGlobalCeiling_ValidateRejects(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"global_budget_usd: -1\n", "global_budget_usd"},
		{"global_max_tasks: -1\n", "global_max_tasks"},
		{"global_window: -5m\n", "global_window"},
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"+tc.yaml), 0o644)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("yaml=%q: want %q rejection, got %v", tc.yaml, tc.want, err)
		}
	}
}

// TOTAL mode (global_window: 0) is ACCEPTED, deliberately: the durable ledger
// implements it (it folds history into a checkpoint rather than decaying), and
// "a total ceiling that survives restart" is the semantic a crash-looping
// unattended install actually wants. The hazard — nothing ages out — is a boot
// warning and a documented behavior, not a load-time refusal.
func TestGlobalCeiling_TotalModeIsAccepted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"+
		"global_budget_usd: 100\nglobal_window: 0s\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("global_window: 0 (total mode) must load: %v", err)
	}
	if c.GlobalWindow != 0 {
		t.Errorf("GlobalWindow = %v, want 0 (total mode)", c.GlobalWindow)
	}
}

// The CROSS-FIELD check: a task limb below max_concurrent_tasks can never fill
// the configured concurrency and parks every later item until the window rolls.
// Refused at load, because at runtime it is indistinguishable from a wedged
// daemon.
func TestGlobalCeiling_RejectsLimbBelowConcurrency(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\n"+
		"max_concurrent_tasks: 4\nglobal_max_tasks: 3\n"), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "global_max_tasks") ||
		!strings.Contains(err.Error(), "max_concurrent_tasks") {
		t.Fatalf("want a cross-field rejection naming both keys, got %v", err)
	}
	// Equal is fine (exactly one dispatch pass fits), and so is off.
	for _, y := range []string{
		"max_concurrent_tasks: 4\nglobal_max_tasks: 4\n",
		"max_concurrent_tasks: 4\nglobal_max_tasks: 0\n",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(p, []byte("network: x\ngateway_ip: 1.2.3.4\n"+y), 0o644)
		if _, err := Load(p); err != nil {
			t.Errorf("yaml=%q must load, got %v", y, err)
		}
	}
}

// The env layer must be subject to the same cross-field check: an operator who
// exports DRYDOCK_GLOBAL_MAX_TASKS=1 on a 2-slot daemon gets the refusal, not a
// silently starving queue. (validate() runs after applyEnvOverrides.)
func TestGlobalCeiling_CrossFieldAppliesToEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DRYDOCK_GLOBAL_MAX_TASKS", "1")
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("network: x\ngateway_ip: 1.2.3.4\nmax_concurrent_tasks: 2\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "global_max_tasks") {
		t.Fatalf("want the cross-field rejection from the env layer, got %v", err)
	}
}
