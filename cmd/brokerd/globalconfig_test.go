package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/creds"
)

// THE OFF-BY-DEFAULT IDENTITY (plan G7), asserted where it can actually break.
//
// The ceiling ships dark, and "dark" is a stronger claim than "the numbers are
// zero": a stock install must gain no ledger file, no global/ directory, and no
// boot-time audit walk. applyGlobalCeiling is the single place that decides,
// which is why the guard here is the same expression the ceiling's own
// globalCeilingOn() uses.
func TestApplyGlobalCeiling_StockConfigOpensNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.AuditRoot = t.TempDir()

	b := &broker.Broker{AuditRoot: cfg.AuditRoot}
	applyGlobalCeiling(b, cfg)

	if b.GlobalLedger != nil {
		t.Fatal("stock config opened a global ledger; the feature must ship dark")
	}
	// Nothing on disk: not the file, not even the directory that would hold it.
	if _, err := os.Stat(broker.GlobalLedgerPath(cfg.AuditRoot)); !os.IsNotExist(err) {
		t.Errorf("stat ledger file = %v, want IsNotExist", err)
	}
	if _, err := os.Stat(filepath.Dir(broker.GlobalLedgerPath(cfg.AuditRoot))); !os.IsNotExist(err) {
		t.Errorf("stat global/ dir = %v, want IsNotExist — a stock audit root gains nothing", err)
	}
	// And the reconcile stays a no-op with no store, so boot does not walk the
	// audit dir a second time.
	reconcileGlobalLedger(b, time.Now().UnixMilli())
	if _, err := os.Stat(filepath.Dir(broker.GlobalLedgerPath(cfg.AuditRoot))); !os.IsNotExist(err) {
		t.Errorf("reconcile created state on a ceiling-off install: %v", err)
	}
}

// The SHIPPED sample config (what `drydock init` / `make install` hand an
// operator) must resolve to off too.
func TestApplyGlobalCeiling_ShippedSampleShipsOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load("../../config/config.yaml")
	if err != nil {
		t.Fatalf("Load shipped config: %v", err)
	}
	if cfg.GlobalBudgetUSD != 0 || cfg.GlobalMaxTasks != 0 {
		t.Fatalf("the shipped config/config.yaml arms the global ceiling ($%v / %d tasks); it must ship off",
			cfg.GlobalBudgetUSD, cfg.GlobalMaxTasks)
	}
	cfg.AuditRoot = t.TempDir()
	b := &broker.Broker{AuditRoot: cfg.AuditRoot}
	applyGlobalCeiling(b, cfg)
	if b.GlobalLedger != nil {
		t.Error("the shipped config opened a ledger")
	}
}

// Either limb alone arms the store — the USD limb and the task limb are
// independent, and a task-limb-only install (the subscription case the whole
// feature exists for) must get a ledger.
func TestApplyGlobalCeiling_EitherLimbOpensTheLedger(t *testing.T) {
	for name, set := range map[string]func(*config.Config){
		"usd limb":  func(c *config.Config) { c.GlobalBudgetUSD = 25 },
		"task limb": func(c *config.Config) { c.GlobalMaxTasks = 10 },
		"both":      func(c *config.Config) { c.GlobalBudgetUSD = 25; c.GlobalMaxTasks = 10 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.AuditRoot = t.TempDir()
			set(cfg)
			b := &broker.Broker{AuditRoot: cfg.AuditRoot}
			applyGlobalCeiling(b, cfg)
			if b.GlobalLedger == nil {
				t.Fatal("no ledger opened with a limb configured")
			}
			if got := b.GlobalLedger.WindowMs(); got != (24 * time.Hour).Milliseconds() {
				t.Errorf("ledger window = %d ms, want the configured 24h default", got)
			}
			if _, err := os.Stat(filepath.Dir(b.GlobalLedger.Path())); err != nil {
				t.Errorf("the ledger directory was not created: %v", err)
			}
		})
	}
}

// global_window reaches the STORE, not just the config struct: the window is
// fixed at OpenGlobalLedger time, so a value that stopped here would leave the
// ceiling measuring 24h while `drydock policy explain` reported something else.
func TestApplyGlobalCeiling_WindowReachesTheLedger(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuditRoot = t.TempDir()
	cfg.GlobalMaxTasks = 3
	cfg.GlobalWindow = 90 * time.Minute
	b := &broker.Broker{AuditRoot: cfg.AuditRoot}
	applyGlobalCeiling(b, cfg)
	if got := b.GlobalLedger.WindowMs(); got != (90 * time.Minute).Milliseconds() {
		t.Errorf("ledger window = %d ms, want 90m", got)
	}

	// Total mode all the way through: 0 must stay 0 (no decay), not fall back
	// to a default.
	cfg2, _ := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	cfg2.AuditRoot = t.TempDir()
	cfg2.GlobalMaxTasks = 3
	cfg2.GlobalWindow = 0
	b2 := &broker.Broker{AuditRoot: cfg2.AuditRoot}
	applyGlobalCeiling(b2, cfg2)
	if got := b2.GlobalLedger.WindowMs(); got != 0 {
		t.Errorf("total-mode ledger window = %d ms, want 0", got)
	}
}

// FAIL-CLOSED ON AN OPEN FAILURE (G2). A ledger that could not be read is kept
// rather than dropped to nil, so the ceiling refuses with the real reason
// instead of silently having no store. Asserted through the ceiling's own
// answer, which is the thing that actually matters.
func TestApplyGlobalCeiling_KeepsADegradedStoreAndRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuditRoot = t.TempDir()
	cfg.GlobalMaxTasks = 5
	// Plant a symlink where the ledger goes: readGlobalLedgerLines opens
	// O_NOFOLLOW and refuses it, so the whole store loads degraded.
	p := broker.GlobalLedgerPath(cfg.AuditRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), p); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	b := &broker.Broker{AuditRoot: cfg.AuditRoot}
	applyGlobalCeiling(b, cfg)
	if b.GlobalLedger == nil {
		t.Fatal("a failed open dropped the store to nil; keep it so the refusal carries the real reason")
	}
	st := b.GlobalCeilingStatus()
	if !st.Blocked {
		t.Error("an unreadable ledger admits; the ceiling must fail closed (plan G2)")
	}
	if st.LoadError == "" {
		t.Error("the load error is not surfaced on the headroom")
	}
}

// CONFIG REACHES ENFORCEMENT — the link that was missing while the feature
// shipped dark. Everything else in this file proves config opens a store, and
// globalcap's tests prove a store produces refusals; this walks the whole path
// in one go, from a YAML file an operator could have written to a task start
// the ceiling refuses.
func TestApplyGlobalCeiling_ConfigReachesTheEnforcementDecision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	yaml := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yaml, []byte(
		"network: x\ngateway_ip: 1.2.3.4\nmax_concurrent_tasks: 2\n"+
			"global_max_tasks: 2\nglobal_budget_usd: 10\nglobal_window: 1h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(yaml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.AuditRoot = t.TempDir()

	b := &broker.Broker{
		AuditRoot: cfg.AuditRoot, DefaultAgent: "claude", MaxConcurrent: cfg.MaxConcurrent,
		// A resolvable agent: the ceiling fails closed on one it cannot resolve
		// (G2), so without a provider every verdict below would be "refused" for
		// the wrong reason and the test would prove nothing.
		Providers: map[string]creds.Provider{"anthropic": stubProvider{}},
	}
	applyGlobalCeiling(b, cfg)

	// The limbs came from the file, not from a literal somewhere else.
	st := b.GlobalCeilingStatus()
	if !st.Enabled || st.MaxTasks != 2 || st.BudgetUSD != 10 {
		t.Fatalf("status = %+v, want the configured 2 starts / $10", st)
	}
	if st.WindowMs != time.Hour.Milliseconds() {
		t.Errorf("window = %d ms, want the configured 1h", st.WindowMs)
	}
	if st.Blocked {
		t.Fatalf("blocked with an empty ledger: %s", st.Reason)
	}

	// Two terminals land in the ledger — what a live daemon's task path writes.
	now := time.Now().UnixMilli()
	for i := 0; i < 2; i++ {
		if err := b.GlobalLedger.Record(now, broker.GlobalEntry{
			Kind: "task", TaskID: fmt.Sprintf("%032x", i+1), EndedAtMs: now,
			Vendor: "anthropic", Agent: "claude", Auth: "api_key",
			Metered: true, USD: 0.5, USDTrusted: true, Outcome: "pushed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	st = b.GlobalCeilingStatus()
	if !st.Blocked {
		t.Fatal("the configured task limb of 2 did not refuse at 2 recorded starts — the feature is still dark")
	}
	if !strings.Contains(st.Reason, "global_max_tasks") {
		t.Errorf("refusal reason = %q, want it to name global_max_tasks", st.Reason)
	}
	if st.Starts != 2 || st.HeadroomStarts != 0 || st.SpentUSD != 1 {
		t.Errorf("status = %+v, want 2 starts / 0 headroom / $1 spent", st)
	}

	// And the ledger is DURABLE: a fresh process reading the same audit root
	// sees the same exhausted ceiling.
	b2 := &broker.Broker{
		AuditRoot: cfg.AuditRoot, DefaultAgent: "claude", MaxConcurrent: cfg.MaxConcurrent,
		Providers: map[string]creds.Provider{"anthropic": stubProvider{}},
	}
	applyGlobalCeiling(b2, cfg)
	if st2 := b2.GlobalCeilingStatus(); !st2.Blocked || st2.Starts != 2 {
		t.Errorf("after restart: blocked=%v starts=%d, want the ceiling to survive", st2.Blocked, st2.Starts)
	}
}

// stubProvider makes "claude" resolvable so the ceiling's fail-closed
// agent-resolution rule is not what produces the verdicts above. It never mints
// anything — no task is run in this file.
type stubProvider struct{}

func (stubProvider) Mint(float64) (creds.Grant, error) { return nil, fmt.Errorf("not used") }
