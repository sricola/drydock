package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/gateway"
)

func TestChooseLogHandler(t *testing.T) {
	cases := []struct {
		name       string
		jsonForced bool
		isTTY      bool
		want       string // "*slog.JSONHandler" or "*slog.TextHandler"
	}{
		{"json forced on a tty wins", true, true, "*slog.JSONHandler"},
		{"non-tty defaults to json", false, false, "*slog.JSONHandler"},
		{"tty without force gets text", false, true, "*slog.TextHandler"},
		{"json forced off a tty", true, false, "*slog.JSONHandler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := chooseLogHandler(&bytes.Buffer{}, tc.jsonForced, tc.isTTY)
			if got := typeName(h); got != tc.want {
				t.Errorf("chooseLogHandler(json=%v, tty=%v) = %s, want %s",
					tc.jsonForced, tc.isTTY, got, tc.want)
			}
		})
	}
}

func typeName(h slog.Handler) string {
	switch h.(type) {
	case *slog.JSONHandler:
		return "*slog.JSONHandler"
	case *slog.TextHandler:
		return "*slog.TextHandler"
	default:
		return "unknown"
	}
}

// TestListen_UnixSocketPermissions verifies that listen() creates a Unix socket
// with mode 0600 and ensures its parent directory is mode 0700. This pins the
// security boundary: no other local user can open the broker socket.
// The umask(0o077) + explicit chmod both defended here.
//
// Note: Unix socket path names are limited to 104 bytes on macOS (UNIX_PATH_MAX).
// We use /tmp directly (rather than os.TempDir() which expands to the long
// /var/folders/... form on macOS) to keep the path under that limit.
func TestListen_UnixSocketPermissions(t *testing.T) {
	// Use /tmp so the path stays under macOS's 104-byte UNIX_PATH_MAX limit.
	// EnsureParent creates "sub" at 0700, proving the wired call chain works.
	tmpDir, err := os.MkdirTemp("/tmp", "brd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	sockDir := filepath.Join(tmpDir, "sub")
	sock := filepath.Join(sockDir, "test.sock")

	cfg := config.Defaults()
	cfg.Broker.Addr = ""     // force Unix-socket path
	cfg.Broker.Socket = sock // override default per-uid path

	l, sockPath := listen(cfg, "127.0.0.1:18088", "127.0.0.1:13128")
	defer l.Close()

	if sockPath != sock {
		t.Fatalf("sockPath = %q, want %q", sockPath, sock)
	}

	// Socket must be mode 0600: no group/world read, write, or execute.
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %04o, want 0600 (owner-only r/w)", perm)
	}

	// Parent directory must be 0700 (EnsureParent guarantees this).
	dirFi, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if perm := dirFi.Mode().Perm(); perm != 0o700 {
		t.Errorf("parent dir perm = %04o, want 0700", perm)
	}
}

// TestPruneOrphanTasks_Ordering verifies that the exec calls inside
// pruneOrphanTasks happen in the documented order: container ls first, then
// container delete for each found task-* container, then pkill for squid.
// The reaper force-deletes orphan task VMs by their EXACT task-<32hex> name and
// no longer fuzzy-pkills squid (that's reapStaleSquid's precise, pidfile-based
// job at squid setup). This pins both: the real task name is deleted, an
// unrelated "task-"-substring token is not, and no pkill is issued.
func TestPruneOrphanTasks_AnchoredMatchNoPkill(t *testing.T) {
	const realTask = "task-6240b146d4a66db701f643b562048d41" // task- + 32 hex
	var calls []string
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })

	runCmd = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "container" && len(args) > 0 && args[0] == "ls" {
			// One real task VM plus a decoy whose name merely contains "task-".
			return []byte(`{"Names":["` + realTask + `","my-task-cache"]}`), nil
		}
		return nil, nil
	}

	pruneOrphanTasks(t.TempDir(), t.TempDir())

	if !strings.HasPrefix(calls[0], "container ls") {
		t.Errorf("call[0] = %q, want the listing first", calls[0])
	}
	deletedReal, deletedDecoy, pkilled := false, false, false
	for _, c := range calls {
		if strings.Contains(c, "container delete") && strings.Contains(c, realTask) {
			deletedReal = true
		}
		if strings.Contains(c, "container delete") && strings.Contains(c, "my-task-cache") {
			deletedDecoy = true
		}
		if strings.HasPrefix(c, "pkill") {
			pkilled = true
		}
	}
	if !deletedReal {
		t.Errorf("the real %s VM was not reaped; calls=%v", realTask, calls)
	}
	if deletedDecoy {
		t.Error("a 'task-'-substring decoy was force-deleted — the match isn't anchored")
	}
	if pkilled {
		t.Error("reaper still issues a fuzzy pkill; squid reap belongs to reapStaleSquid")
	}
}

func TestResolveAPIKey_Precedence(t *testing.T) {
	file := map[string]string{"ANTHROPIC_API_KEY": "from-file"}

	t.Run("non-empty env overrides file", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "from-env")
		if got := resolveAPIKey("ANTHROPIC_API_KEY", file); got != "from-env" {
			t.Errorf("got %q, want from-env", got)
		}
	})
	t.Run("empty env falls through to file", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := resolveAPIKey("ANTHROPIC_API_KEY", file); got != "from-file" {
			t.Errorf("got %q, want from-file", got)
		}
	})
	t.Run("unset env + no file entry yields empty", func(t *testing.T) {
		if got := resolveAPIKey("OPENAI_API_KEY", file); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// TestHardenedServer_Timeouts asserts the exact header/idle timeouts that
// harden the server against slow-loris and idle-keepalive abuse. It also
// asserts that ReadTimeout and WriteTimeout remain UNSET: POST /tasks blocks
// for the whole task run and WriteTimeout would sever streaming responses.
func TestHardenedServer_Timeouts(t *testing.T) {
	srv := hardenedServer(http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
	// Must NOT be set: a body/response timeout would kill long-running tasks.
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (unset — POST /tasks blocks for full task run)", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (unset — gateway streams long-lived responses)", srv.WriteTimeout)
	}
}

// TestFindEgressConfig_EnvVarTakesPrecedence verifies that EGRESS_CONFIG is
// tried before any other candidate. This is a security-relevant ordering: the
// explicit operator override must always win.
func TestFindEgressConfig_EnvVarTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	egress := filepath.Join(dir, "custom-egress.yaml")
	if err := os.WriteFile(egress, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EGRESS_CONFIG", egress)
	// Isolate from any real ~/.drydock/egress.yaml.
	t.Setenv("HOME", t.TempDir())

	got, err := findEgressConfig()
	if err != nil {
		t.Fatalf("findEgressConfig: %v", err)
	}
	if got != egress {
		t.Errorf("got %q, want %q (EGRESS_CONFIG env var)", got, egress)
	}
}

// TestFindEgressConfig_UserFileBeforeCWD verifies that the user-owned
// ~/.drydock/egress.yaml is found when it exists, and is chosen even when
// EGRESS_CONFIG is unset.
func TestFindEgressConfig_UserFileFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("EGRESS_CONFIG", "")

	drydockDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(drydockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	userEgress := filepath.Join(drydockDir, "egress.yaml")
	if err := os.WriteFile(userEgress, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findEgressConfig()
	if err != nil {
		t.Fatalf("findEgressConfig: %v", err)
	}
	if got != userEgress {
		t.Errorf("got %q, want %q (user file)", got, userEgress)
	}
}

// TestFindEgressConfig_NoneFound verifies that when no egress.yaml exists in
// any candidate location, findEgressConfig returns an error naming the paths it
// tried (so operators can diagnose the failure).
func TestFindEgressConfig_NoneFound(t *testing.T) {
	t.Setenv("EGRESS_CONFIG", "")
	t.Setenv("HOME", t.TempDir())            // no ~/.drydock/egress.yaml
	t.Setenv("HOMEBREW_PREFIX", t.TempDir()) // no share-dir copy

	// Chdir to a directory that has no config/egress.yaml.
	emptyDir := t.TempDir()
	prev, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(prev) }) //nolint:errcheck
	if err := os.Chdir(emptyDir); err != nil {
		t.Fatal(err)
	}

	_, err := findEgressConfig()
	if err == nil {
		t.Fatal("want error when no egress.yaml found anywhere")
	}
	// The error must list what was tried so operators can diagnose it.
	if !strings.Contains(err.Error(), "tried") {
		t.Errorf("error %q should name tried paths", err.Error())
	}
}

// TestCheckContainerVersion_ValidVersion logs "container CLI" at Info level and
// does not exit. This pins the happy-path parse-and-match branch.
func TestCheckContainerVersion_ValidVersion_LogsInfo(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	origRun := runCmd
	t.Cleanup(func() { runCmd = origRun })
	runCmd = func(name string, args ...string) ([]byte, error) {
		return []byte("container CLI version 1.2.3"), nil
	}

	checkContainerVersion(false)

	if !strings.Contains(buf.String(), "container CLI") {
		t.Errorf("expected 'container CLI' info log; got: %s", buf.String())
	}
	// The version string must be surfaced so the operator can see it.
	if !strings.Contains(buf.String(), "1.2.3") {
		t.Errorf("expected version '1.2.3' in log; got: %s", buf.String())
	}
}

// TestCheckContainerVersion_MajorMismatch_NonStrict logs a warning and does
// not exit. This pins the version-mismatch / non-strict branch.
func TestCheckContainerVersion_MajorMismatch_NonStrict_Warns(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	origRun := runCmd
	t.Cleanup(func() { runCmd = origRun })
	runCmd = func(name string, args ...string) ([]byte, error) {
		// Major 2 != supportedContainerMajor ("1") — triggers the mismatch path.
		return []byte("container CLI version 2.0.0"), nil
	}

	checkContainerVersion(false) // non-strict: must warn, not exit

	if !strings.Contains(buf.String(), "not in tested range") {
		t.Errorf("expected 'not in tested range' warning; got: %s", buf.String())
	}
}

// TestCheckContainerVersion_UnparseableOutput_NonStrict logs a "could not
// parse" warning and does not exit. This pins the unparseable-output / non-strict
// branch that fires when `container --version` changes its format.
func TestCheckContainerVersion_UnparseableOutput_NonStrict_Warns(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	origRun := runCmd
	t.Cleanup(func() { runCmd = origRun })
	runCmd = func(name string, args ...string) ([]byte, error) {
		return []byte("unexpected output — no version line here"), nil
	}

	checkContainerVersion(false) // non-strict: must warn, not exit

	if !strings.Contains(buf.String(), "could not parse") {
		t.Errorf("expected 'could not parse' warning; got: %s", buf.String())
	}
}

func TestEffectiveRequestCap(t *testing.T) {
	cases := []struct {
		uncapped   bool
		configured int
		want       int
	}{
		{true, 0, broker.DefaultUncappedRequestCap},  // uncapped + unlimited → fail closed to the default cap
		{true, -1, broker.DefaultUncappedRequestCap}, // negative treated as unset
		{true, 50, 50}, // uncapped but operator set a bound → honored
		{false, 0, 0},  // a USD budget bounds spend → 0 stays unlimited
		{false, 200, 200},
	}
	for _, c := range cases {
		if got := effectiveRequestCap(c.uncapped, c.configured); got != c.want {
			t.Errorf("effectiveRequestCap(%v, %d) = %d, want %d", c.uncapped, c.configured, got, c.want)
		}
	}
}

// TestIsUnmeteredVendor pins the single decision point shared by the
// lease-budget mint (math.MaxFloat64) and Broker.UnmeteredVendors: a vendor's
// lane is unmetered iff its auth mode is "subscription", or it's a
// config-built (openai-compat) lane with no prices configured.
func TestIsUnmeteredVendor(t *testing.T) {
	t.Run("subscription anthropic is unmetered", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.AnthropicAuth = "subscription"
		b := gateway.Backend{Vendor: gateway.AnthropicVendor()}
		if !isUnmeteredVendor(cfg, b) {
			t.Error("subscription anthropic lane should be unmetered")
		}
	})
	t.Run("api_key anthropic is metered", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.AnthropicAuth = "api_key"
		b := gateway.Backend{Vendor: gateway.AnthropicVendor()}
		if isUnmeteredVendor(cfg, b) {
			t.Error("api_key anthropic lane should be metered")
		}
	})
	t.Run("openai-compat with no prices is unmetered", func(t *testing.T) {
		cfg := config.Defaults()
		b := gateway.Backend{Vendor: gateway.OpenAICompatVendor("openai-compat", "https://up.test", "", nil)}
		if !isUnmeteredVendor(cfg, b) {
			t.Error("priceless openai-compat lane should be unmetered")
		}
	})
	t.Run("openai-compat with prices is metered", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.OpenAICompat.Prices = map[string]config.OpenAICompatPrice{"default": {Input: 1, Output: 2}}
		b := gateway.Backend{Vendor: gateway.OpenAICompatVendor("openai-compat", "https://up.test", "", nil)}
		if isUnmeteredVendor(cfg, b) {
			t.Error("openai-compat lane with prices configured should be metered")
		}
	})
}

func TestLoopbackHostPort(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8088":    true,
		"[::1]:8088":        true,
		"localhost:8088":    true,
		":8088":             false, // all interfaces — VM-reachable
		"0.0.0.0:8088":      false,
		"192.168.66.1:8088": false, // the vmnet gateway — the exact exposure
		"garbage":           false, // unparseable → fail closed
	}
	for addr, want := range cases {
		if got := loopbackHostPort(addr); got != want {
			t.Errorf("loopbackHostPort(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestTaskContainerRE(t *testing.T) {
	if !taskContainerRE.MatchString("task-6240b146d4a66db701f643b562048d41") {
		t.Error("real task-<32hex> name should match")
	}
	for _, bad := range []string{"task-", "task-short", "my-task-6240b146d4a66db701f643b562048d41", "task-6240b146d4a66db701f643b562048d41x", "drydock-anchor"} {
		if taskContainerRE.MatchString(bad) {
			t.Errorf("%q should NOT match the anchored task-name grammar", bad)
		}
	}
}

func TestPruneOrphanTasks_KeepsGatedStages(t *testing.T) {
	stageRoot := t.TempDir()
	auditRoot := t.TempDir()
	os.MkdirAll(filepath.Join(stageRoot, "gated"), 0o700)
	os.MkdirAll(filepath.Join(stageRoot, "orphan"), 0o700)
	// A live gate marker for "gated".
	os.WriteFile(filepath.Join(auditRoot, "gated.gate.json"), []byte(`{"repo_ref":"r"}`), 0o600)

	// Stub runCmd so the container ls call doesn't fail on CI.
	orig := runCmd
	t.Cleanup(func() { runCmd = orig })
	runCmd = func(name string, args ...string) ([]byte, error) {
		if name == "container" && len(args) > 0 && args[0] == "ls" {
			return []byte(`[]`), nil
		}
		return nil, nil
	}

	pruneOrphanTasks(stageRoot, auditRoot)

	if _, err := os.Stat(filepath.Join(stageRoot, "gated")); err != nil {
		t.Error("gated stage (with a live marker) must survive the reap")
	}
	if _, err := os.Stat(filepath.Join(stageRoot, "orphan")); !os.IsNotExist(err) {
		t.Error("orphan stage must be reaped")
	}
}

func TestSeedAggregateFromAudit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name, meta, task string, cost float64) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(meta+"\n"+task+"\n"+
			fmt.Sprintf(`{"type":"result","subtype":"success","total_cost_usd":%.4f,"src":"broker"}`, cost)+"\n"), 0o600)
		os.Chtimes(p, now, now) // in window
	}
	write("a.jsonl",
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`,
		`{"type":"drydock_task","agent":"claude"}`, 2.0)
	write("b.jsonl",
		`{"type":"drydock_meta","subscription":true,"sensitive":false}`, // subscription: excluded
		`{"type":"drydock_task","agent":"claude"}`, 9.0)

	gw, _ := gateway.New(gateway.Backend{Vendor: gateway.AnthropicVendor(), Cred: gateway.StaticKey("k")})
	gw.SetAggregateCap(100.0, 24*time.Hour, []string{"anthropic"})
	seedAggregateFromAudit(gw, dir, 24*time.Hour, "claude")

	if gw.AggregateExceeded("anthropic") { // 2.0 seeded, cap 100 -> not exceeded
		t.Fatal("unexpectedly exceeded")
	}
	gw.SeedAggregate("anthropic", 98.0, now) // total now 100 -> at cap
	if !gw.AggregateExceeded("anthropic") {
		t.Error("want exceeded after seeding to 100 (only the non-subscription 2.0 should have counted from audit)")
	}
}

// F-07: a compromised agent CLI can forge a `result` line. The aggregate seed
// must ignore a CLI-authored cost (no src:"broker"), so a forged low/zero cost
// cannot understate the rolling cap after a restart.
func TestSeedAggregateFromAudit_IgnoresForgedAgentCost(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p := filepath.Join(dir, "forged.jsonl")
	os.WriteFile(p, []byte(
		`{"type":"drydock_meta","subscription":false,"sensitive":false}`+"\n"+
			`{"type":"drydock_task","agent":"claude"}`+"\n"+
			// Forged agent result: huge cost, but NOT broker-authored (no src).
			`{"type":"result","subtype":"success","total_cost_usd":9999.0}`+"\n"), 0o600)
	os.Chtimes(p, now, now)

	gw, _ := gateway.New(gateway.Backend{Vendor: gateway.AnthropicVendor(), Cred: gateway.StaticKey("k")})
	gw.SetAggregateCap(100.0, 24*time.Hour, []string{"anthropic"})
	seedAggregateFromAudit(gw, dir, 24*time.Hour, "claude")

	if gw.AggregateExceeded("anthropic") {
		t.Error("a forged (non-broker) agent cost of 9999 seeded the ledger; the seed must trust only src:broker")
	}
}
