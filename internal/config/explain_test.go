package config

import (
	"os"
	"path/filepath"
	"testing"
)

func fieldByName(fs []Field, name string) (Field, bool) {
	for _, f := range fs {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

func TestExplain_SourceAttribution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A yaml that sets task_timeout (yaml-only) and network (also env-overridable).
	yaml := "network: from-yaml\ngateway_ip: 1.2.3.4\ntask_timeout: 45m\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	// Env overrides gateway_ip with a valid value, and task_budget with an
	// INVALID value (<=0) that must fall through to the default.
	t.Setenv("DRYDOCK_GW_IP", "10.0.0.9")
	t.Setenv("DRYDOCK_TASK_BUDGET_USD", "-5") // fails the f>0 guard

	fields, cfg, err := Explain(path)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if cfg.GatewayIP != "10.0.0.9" {
		t.Errorf("resolved gateway = %q, want env value", cfg.GatewayIP)
	}
	check := func(name string, wantSrc Source, wantVal string) {
		f, ok := fieldByName(fields, name)
		if !ok {
			t.Fatalf("field %q missing from Explain", name)
		}
		if f.Source != wantSrc {
			t.Errorf("%s source = %q, want %q", name, f.Source, wantSrc)
		}
		if wantVal != "" && f.Value != wantVal {
			t.Errorf("%s value = %q, want %q", name, f.Value, wantVal)
		}
	}
	check("GatewayIP", SourceEnv, "10.0.0.9")
	check("Network", SourceYAML, "from-yaml")
	check("TaskTimeout", SourceYAML, "45m0s")
	check("TaskBudgetUSD", SourceDefault, "2") // env was invalid → default
	check("MaxConcurrent", SourceDefault, "")
}

// The provenance table must not silently drift from applyEnvOverrides: every
// env var handled there must be represented by exactly one Field.EnvVar.
func TestExplain_CoversEveryEnvOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range fields {
		if f.EnvVar != "" {
			got[f.EnvVar] = true
		}
	}
	// Keep this list identical to the env vars applyEnvOverrides reads.
	want := []string{
		"DRYDOCK_NETWORK", "DRYDOCK_GW_IP", "SANDBOX_IMAGE", "DRYDOCK_ANCHOR_IMAGE",
		"DRYDOCK_TASK_BUDGET_USD", "DRYDOCK_MAX_CONCURRENT_TASKS", "DRYDOCK_DEFAULT_MODEL",
		"DRYDOCK_DEFAULT_AGENT", "DRYDOCK_ANTHROPIC_AUTH", "DRYDOCK_OPENAI_AUTH",
		"DRYDOCK_TASK_MAX_REQUESTS", "DRYDOCK_TASK_MAX_INFLIGHT", "DRYDOCK_STAGE_QUOTA_GB",
		"DRYDOCK_AGGREGATE_BUDGET_USD", "DRYDOCK_MAX_REQUEST_COST_USD", "DRYDOCK_AGGREGATE_WINDOW",
		"DRYDOCK_PUSH_MAX_RETRIES", "DRYDOCK_PUSH_RETRY_BACKOFF", "DRYDOCK_PUSH_FRESH_BRANCH_TRIES",
		"STAGE_ROOT", "AUDIT_ROOT", "SQUID_RUN_DIR", "BROKER_SOCKET", "BROKER_ADDR",
		"DRYDOCK_NO_NOTIFY", "DRYDOCK_LOG_JSON", "DRYDOCK_STRICT_CONTAINER_VERSION",
	}
	for _, ev := range want {
		if !got[ev] {
			t.Errorf("env var %q handled by applyEnvOverrides but not in Explain provenance", ev)
		}
	}
}

func TestEffectiveHash_StableAndSensitive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f1, _, _ := Explain(filepath.Join(t.TempDir(), "m.yaml"))
	f2, _, _ := Explain(filepath.Join(t.TempDir(), "m.yaml"))
	if EffectiveHash(f1) != EffectiveHash(f2) {
		t.Error("hash not stable across identical loads")
	}
	if len(EffectiveHash(f1)) != 64 {
		t.Error("hash not 64 hex chars")
	}
	t.Setenv("DRYDOCK_NETWORK", "different")
	f3, _, _ := Explain(filepath.Join(t.TempDir(), "m.yaml"))
	if EffectiveHash(f3) == EffectiveHash(f1) {
		t.Error("hash unchanged after a value changed")
	}
}
