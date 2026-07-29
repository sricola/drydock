package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/config"
)

// servePolicyFromLocal wires the fake broker's /admin/policy to return a body
// computed by mutate over the CLI's own local resolution — config.Explain on
// the same HOME/env the test set up. Computing inside the handler (request
// time, not setup time) means the body always reflects the final env state.
// mutate == nil returns the local view verbatim, i.e. an in-sync daemon.
func servePolicyFromLocal(t *testing.T, mutate func(fields []config.Field, hash string) ([]config.Field, string)) {
	t.Helper()
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/policy" {
			t.Errorf("request = %s %s, want GET /admin/policy", r.Method, r.URL.Path)
		}
		fields, _, err := config.Explain(config.DefaultPath())
		if err != nil {
			t.Errorf("in-handler Explain: %v", err)
		}
		hash := config.EffectiveHash(fields)
		if mutate != nil {
			fields, hash = mutate(fields, hash)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"fields": fields, "hash": hash})
	}))
}

// writeHomeConfig writes ~/.drydock/config.yaml under the (test-tempdir) HOME.
func writeHomeConfig(t *testing.T, yaml string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".drydock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyExplain_LocalTableShowsSourcesAndValues(t *testing.T) {
	servePolicyFromLocal(t, nil)
	writeHomeConfig(t, "network: drydock-test-net\n")
	t.Setenv("DRYDOCK_GW_IP", "10.99.88.77")

	out := captureStdout(t, func() { runPolicy([]string{"explain"}) })

	for _, want := range []string{"SETTING", "VALUE", "SOURCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("table header missing %q:\n%s", want, out)
		}
	}
	// env-sourced row names the env var.
	if !strings.Contains(out, "GatewayIP") || !strings.Contains(out, "10.99.88.77") || !strings.Contains(out, "env:DRYDOCK_GW_IP") {
		t.Errorf("GatewayIP row should show value 10.99.88.77 sourced env:DRYDOCK_GW_IP:\n%s", out)
	}
	// yaml-sourced row (value differs from default, so attributed to config.yaml).
	if !strings.Contains(out, "drydock-test-net") || !strings.Contains(out, "config.yaml") {
		t.Errorf("Network row should show drydock-test-net sourced config.yaml:\n%s", out)
	}
	// untouched field falls through to default.
	if !strings.Contains(out, "MaxConcurrent") || !strings.Contains(out, "default") {
		t.Errorf("MaxConcurrent row should be sourced default:\n%s", out)
	}
}

func TestPolicyExplain_InSyncWhenHashMatches(t *testing.T) {
	servePolicyFromLocal(t, nil) // daemon reports exactly the local resolution

	out := captureStdout(t, func() { runPolicy([]string{"explain"}) })

	if !strings.Contains(out, "daemon: in sync") {
		t.Errorf("matching hashes should report in sync:\n%s", out)
	}
	if strings.Contains(out, "DIVERGENT") {
		t.Errorf("in-sync output must not claim divergence:\n%s", out)
	}
}

func TestPolicyExplain_DivergentWhenHashDiffers(t *testing.T) {
	servePolicyFromLocal(t, func(fields []config.Field, _ string) ([]config.Field, string) {
		live := make([]config.Field, len(fields))
		copy(live, fields)
		for i := range live {
			if live[i].Name == "Network" {
				live[i].Value = "stale-live-net"
			}
		}
		return live, config.EffectiveHash(live)
	})

	out := captureStdout(t, func() { runPolicy([]string{"explain"}) })

	if !strings.Contains(out, "daemon: DIVERGENT") {
		t.Errorf("differing hashes should report DIVERGENT:\n%s", out)
	}
	if !strings.Contains(out, "Network") || !strings.Contains(out, "stale-live-net") {
		t.Errorf("divergence should name the differing field and its live value:\n%s", out)
	}
	if !strings.Contains(out, "local=") || !strings.Contains(out, "live=") {
		t.Errorf("divergence rows should show local= and live= values:\n%s", out)
	}
}

func TestPolicyExplain_UnreachableDaemonShowsBanner(t *testing.T) {
	// No BROKER_ADDR, and a socket path that does not exist → brokerdDown.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BROKER_ADDR", "")
	t.Setenv("BROKER_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))

	// runPolicy returning normally (no die/os.Exit) is the exit-0 contract.
	out := captureStdout(t, func() { runPolicy([]string{"explain"}) })

	if !strings.Contains(out, "LIVE POLICY UNVERIFIED") {
		t.Errorf("unreachable brokerd should print the UNVERIFIED banner:\n%s", out)
	}
	if !strings.Contains(out, "SETTING") || !strings.Contains(out, "Network") {
		t.Errorf("local table should still render when brokerd is down:\n%s", out)
	}
	if strings.Contains(out, "in sync") || strings.Contains(out, "DIVERGENT") {
		t.Errorf("no sync verdict may be claimed when the daemon was never reached:\n%s", out)
	}
}

func TestPolicyExplain_JSON(t *testing.T) {
	servePolicyFromLocal(t, nil)

	out := captureStdout(t, func() { runPolicy([]string{"explain", "--json"}) })

	var got struct {
		Local struct {
			Fields []config.Field `json:"fields"`
			Hash   string         `json:"hash"`
		} `json:"local"`
		Live *struct {
			Fields []config.Field `json:"fields"`
			Hash   string         `json:"hash"`
		} `json:"live"`
		InSync *bool `json:"in_sync"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output does not decode: %v\n%s", err, out)
	}
	if len(got.Local.Fields) == 0 || got.Local.Hash == "" {
		t.Errorf("local side should carry fields and a hash: %+v", got.Local)
	}
	if got.Live == nil || got.Live.Hash != got.Local.Hash {
		t.Errorf("live side should be present and hash-equal: %+v", got.Live)
	}
	if got.InSync == nil || !*got.InSync {
		t.Errorf("in_sync should be true when hashes match, got %v", got.InSync)
	}
	// No field value may look like a credential — Field values are summaries
	// by construction; this pins that nothing key-shaped ever leaks through.
	for _, f := range got.Local.Fields {
		if strings.HasPrefix(f.Value, "sk-") || strings.Contains(f.Value, "sk-ant-") {
			t.Errorf("field %s value looks like a secret: %q", f.Name, f.Value)
		}
	}
}
