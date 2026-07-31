package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"drydock/internal/runner"
)

// THE ADVERSARIAL SET (plan Task 5). globalstorm_test.go proves the ceiling
// bounds the loop it was built for; this file attacks the ceiling itself.
//
// The four questions, one section each:
//
//	(A) CAN A HOSTILE LEDGER MAKE IT ADMIT? Every state a ledger file can be
//	    left in by a crash, a disk, or a person with an editor — and for each,
//	    the answer the ceiling actually gives, asserted rather than assumed.
//	(B) CAN AN AGENT REACH THE LEDGER AT ALL? Structural, not behavioural: the
//	    directory is never mounted into a VM, and no broker-authored row can
//	    source its cost from anything but the gateway lease.
//	(C) DO THE EDGES HOLD? The window's exact cutoff, and a restart in both
//	    window modes.
//	(D) DOES A CRASH CONVERGE? A task that starts and never terminals, and a
//	    clock that moves while the daemon is down.
//
// Where a property is already pinned elsewhere the test here says so and tests
// the part that is NOT — the enforcement-level view of a store-level property,
// or the case the existing fixture deliberately does not cover.

// ---------------------------------------------------------------------------
// (A) Hostile ledger states.
// ---------------------------------------------------------------------------

// TestGlobalCeiling_HostileLedgerStatesRefuseRatherThanAdmit is G2 as a table
// of the states an attacker, a crash or a disk can actually produce.
//
// The per-limb column is the point, and it is deliberate rather than a
// weakness: a quarantined line is still worth exactly one task START (the line
// exists because a task ran), so refusing the start limb on it would let one
// corrupt byte brick an install that is not even using the USD limb —
// permanently, in total mode. Only losses that genuinely cost us STARTS degrade
// the start limb.
//
// The ABSENT-file row is the one that admits, and it is not a hole: an empty
// ledger is a FACT (an install that has never run a task), not an "I don't
// know". It is asserted here so the documentation cannot drift into claiming
// otherwise — which it had.
func TestGlobalCeiling_HostileLedgerStatesRefuseRatherThanAdmit(t *testing.T) {
	cases := []struct {
		name string
		// plant returns the ledger the broker will be given.
		plant func(t *testing.T, root string) *GlobalLedger
		// wantUSD / wantStarts: does that limb refuse, on its own?
		wantUSD    bool
		wantStarts bool
		// wantReason is a fragment the refusal must carry.
		wantReason string
	}{
		{
			name: "an ABSENT ledger file admits: empty is a fact, not an unknown",
			plant: func(t *testing.T, root string) *GlobalLedger {
				l := mustOpenGL(t, root, 24*time.Hour, capNow)
				if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
					t.Fatalf("a ledger nothing has recorded to should not exist yet (stat err = %v)", err)
				}
				return l
			},
			wantUSD: false, wantStarts: false,
		},
		{
			name: "NO STORE AT ALL refuses both limbs",
			plant: func(t *testing.T, root string) *GlobalLedger {
				return nil // a configured limb with nothing to measure it
			},
			wantUSD: true, wantStarts: true,
			wantReason: "fail-closed",
		},
		{
			name: "a SYMLINKED ledger refuses both limbs",
			plant: func(t *testing.T, root string) *GlobalLedger {
				return plantUnreadableLedger(t, root, 24*time.Hour)
			},
			wantUSD: true, wantStarts: true,
		},
		{
			name: "an UNREADABLE-BY-PERMISSION ledger refuses both limbs",
			plant: func(t *testing.T, root string) *GlobalLedger {
				if os.Geteuid() == 0 {
					t.Skip("running as root: mode 0000 is still readable")
				}
				p := GlobalLedgerPath(root)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
				l, err := OpenGlobalLedger(root, 24*time.Hour, capNow)
				if err == nil {
					t.Fatal("OpenGlobalLedger read a mode-0000 ledger")
				}
				return l
			},
			wantUSD: true, wantStarts: true,
		},
		{
			name: "a GARBAGE line refuses the USD limb and keeps the start",
			plant: func(t *testing.T, root string) *GlobalLedger {
				return plantDamagedLedger(t, root, 24*time.Hour)
			},
			wantUSD: true, wantStarts: false,
			wantReason: "cannot be trusted",
		},
		{
			name: "damage that cannot be identified refuses BOTH limbs",
			plant: func(t *testing.T, root string) *GlobalLedger {
				return plantUnidentifiableDamagedLedger(t, root, 24*time.Hour)
			},
			wantUSD: true, wantStarts: true,
			wantReason: "lower bound",
		},
		{
			name: "a hand-edited NEGATIVE usd is damage, not a credit (M10)",
			plant: func(t *testing.T, root string) *GlobalLedger {
				l := mustOpenGL(t, root, 24*time.Hour, capNow)
				mustRecord(t, l, capNow, glEntry(glTaskID(1), capNow, 5))
				// The edit an attacker would actually make: buy headroom back.
				handEdit(t, l.Path(), func(e *GlobalEntry) bool {
					if e.Kind == GlobalEntryTask {
						e.USD = -1000
						return true
					}
					return false
				})
				l2 := mustOpenGL(t, root, 24*time.Hour, capNow)
				if u := l2.Usage(capNow); u.USD < 0 {
					t.Fatalf("a negative hand-edit SUBTRACTED from the limb: USD = %v", u.USD)
				}
				return l2
			},
			wantUSD: true, wantStarts: false,
		},
		{
			name: "a hand-edited HUGE usd only ever tightens the ceiling",
			plant: func(t *testing.T, root string) *GlobalLedger {
				l := mustOpenGL(t, root, 24*time.Hour, capNow)
				mustRecord(t, l, capNow, glEntry(glTaskID(1), capNow, 5))
				handEdit(t, l.Path(), func(e *GlobalEntry) bool {
					if e.Kind == GlobalEntryTask {
						e.USD = 1e6
						return true
					}
					return false
				})
				return mustOpenGL(t, root, 24*time.Hour, capNow)
			},
			// Believed (a positive, finite figure is data), and the direction is
			// the safe one: an inflated ledger refuses MORE, never less.
			wantUSD: true, wantStarts: false,
			wantReason: "global_budget_usd is exhausted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each limb is exercised ALONE, because the whole point of the
			// per-limb gating is that one limb's loss must not become the
			// other's.
			for _, limb := range []struct {
				name string
				arm  func(b *Broker)
				want bool
			}{
				{"global_budget_usd alone", func(b *Broker) { b.GlobalBudgetUSD = 100 }, tc.wantUSD},
				{"global_max_tasks alone", func(b *Broker) { b.GlobalMaxTasks = 100 }, tc.wantStarts},
			} {
				t.Run(limb.name, func(t *testing.T) {
					b := capBroker(t)
					limb.arm(b)
					b.GlobalLedger = tc.plant(t, b.AuditRoot)
					blocked, why := b.globalCeilingExceeded("claude")
					if blocked != limb.want {
						t.Fatalf("blocked = %v, want %v (reason %q)", blocked, limb.want, why)
					}
					if !blocked {
						return
					}
					if !strings.HasPrefix(why, globalCeilingPrefix) {
						t.Errorf("refusal %q does not carry the ceiling's own prefix", why)
					}
					if tc.wantReason != "" && !strings.Contains(why, tc.wantReason) {
						t.Errorf("refusal %q does not carry %q", why, tc.wantReason)
					}
					// A refusal an operator cannot act on is a wedged daemon.
					if len(why) < 40 {
						t.Errorf("refusal %q is too terse to act on", why)
					}
				})
			}
		})
	}
}

// handEdit rewrites the ledger file in place, applying mutate to each decoded
// entry and keeping the ones it reports changed. It is the "someone opened the
// file in an editor" fixture: the bytes stay well-formed JSON, so nothing but
// the ledger's own validation can catch the edit.
func handEdit(t *testing.T, path string, mutate func(*GlobalEntry) bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var out bytes.Buffer
	edited := false
	for _, ln := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var e GlobalEntry
		if err := json.Unmarshal(ln, &e); err != nil {
			out.Write(ln)
			out.WriteByte('\n')
			continue
		}
		if mutate(&e) {
			edited = true
			b, merr := json.Marshal(e)
			if merr != nil {
				t.Fatalf("marshal edited entry: %v", merr)
			}
			out.Write(b)
		} else {
			out.Write(ln)
		}
		out.WriteByte('\n')
	}
	if !edited {
		t.Fatal("the hand-edit fixture matched nothing; it would prove nothing")
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

// TestGlobalCeiling_AnInflatedCheckpointCountRefuses.
//
// The I1 shape, taken to its adversarial end. TestGlobalLedgerCheckpointRejects-
// ATamperedCount edits ONE copy, so a surviving copy restores the truth. Here
// EVERY copy is edited to the same inflated count — the state a person with an
// editor produces, since they would not leave two copies disagreeing — and the
// checksum is the only thing standing between the ceiling and a number nobody
// computed.
//
// Inflation is the SAFE direction if believed, so this test is about which
// failure mode we get, not about whether money escapes: the answer must be a
// refusal (a start count that is not ours is not a measurement), never silent
// acceptance and never a silent RESET to the smaller real count.
func TestGlobalCeiling_AnInflatedCheckpointCountRefuses(t *testing.T) {
	root := t.TempDir()
	l := glFoldedLedger(t, root, globalLedgerTotalKeep+600, 1)
	before := l.Usage(capNow)

	path := GlobalLedgerPath(root)
	lines := rawLines(t, path)
	var buf bytes.Buffer
	edited := 0
	for _, ln := range lines {
		var e GlobalEntry
		if json.Unmarshal(ln, &e) == nil && e.Kind == GlobalEntryCheckpoint {
			e.Starts = 10_000_000 // the hand-edit, in every copy
			b, _ := json.Marshal(e)
			ln = b
			edited++
		}
		buf.Write(ln)
		buf.WriteByte('\n')
	}
	if edited < globalLedgerCheckpointCopies {
		t.Fatalf("edited %d checkpoint copies, want all %d", edited, globalLedgerCheckpointCopies)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	u := mustOpenGL(t, root, 0, capNow).Usage(capNow)
	if !u.StartsDegraded {
		t.Fatalf("an inflated, unverifiable checkpoint count was accepted as fact: Starts = %d (was %d)",
			u.Starts, before.Starts)
	}
	if u.Starts >= 10_000_000 {
		t.Errorf("the ledger believed a hand-written start count of %d", u.Starts)
	}
	// And the ceiling turns that into a refusal on the limb that lost the
	// information — not on the other one.
	b := capBroker(t)
	b.GlobalMaxTasks = 100
	b.GlobalLedger = mustOpenGL(t, root, 0, capNow)
	blocked, why := b.globalCeilingExceeded("claude")
	if !blocked {
		t.Fatal("the start limb admitted against a checkpoint whose count could not be verified")
	}
	if !strings.Contains(why, "lower bound") {
		t.Errorf("refusal = %q, want it to say the count is a lower bound", why)
	}
	// Total mode does not age out, so the remedy must name the file rather than
	// implying the refusal clears on its own.
	if !strings.Contains(why, "does NOT age out") {
		t.Errorf("the total-mode refusal %q does not say the damage will not age out", why)
	}
}

// TestGlobalCeiling_ALedgerDirectoryLeftWideOpenIsTightened. The file is written
// 0600 by construction; a DIRECTORY that already exists keeps whatever mode it
// has, because MkdirAll only applies its mode to directories it creates. The
// ledger is host-only (G4), so the open tightens it.
func TestGlobalCeiling_ALedgerDirectoryLeftWideOpenIsTightened(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Dir(GlobalLedgerPath(root))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // MkdirAll applies umask
		t.Fatal(err)
	}
	l := mustOpenGL(t, root, time.Hour, capNow)
	mustRecord(t, l, capNow, glEntry(glTaskID(1), capNow, 1))

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("a pre-existing ledger directory kept mode %o; the ceiling's record is host-only", perm)
	}
	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("ledger file mode = %o, want 0600", perm)
	}
}

// ---------------------------------------------------------------------------
// (B) The ledger is HOST-ONLY, structurally (G4).
// ---------------------------------------------------------------------------

// TestGlobalCeiling_TheLedgerIsNeverMountedIntoASandboxVM.
//
// The ceiling's whole trust argument is that an agent cannot touch its inputs.
// "We never added such a mount" is a claim about the past; this is a claim about
// the argv the runner actually builds, and it fails if anyone ever adds a fourth
// mount without thinking about it.
//
// The audit root — which CONTAINS the ledger directory — is deliberately used as
// the source of the stage/cache/verify paths' PARENT, so a mount that leaked the
// audit root, or anything at or above the ledger, is caught rather than merely a
// mount that named the ledger file exactly.
func TestGlobalCeiling_TheLedgerIsNeverMountedIntoASandboxVM(t *testing.T) {
	auditRoot := t.TempDir()
	ledgerPath := GlobalLedgerPath(auditRoot)
	ledgerDir := filepath.Dir(ledgerPath)
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stage, cache, verify := t.TempDir(), t.TempDir(), t.TempDir()

	argvs := map[string][]string{
		"agent": runner.BuildRunArgs(runner.Spec{
			TaskID: "0123456789abcdef0123456789abcdef", Network: "n", ImageRef: "img",
			Env:      []string{"ANTHROPIC_AUTH_TOKEN=tok", "DRYDOCK_GW_IP=10.0.0.1"},
			StageDir: stage, CacheDir: cache, PromptFile: "/work/.drydock/prompt",
			MemoryGB: 4, CPUs: 2,
		}),
		"setup": runner.BuildSetupArgs(runner.SetupSpec{
			TaskID: "0123456789abcdef0123456789abcdef", Network: "n", ImageRef: "img",
			StageDir: stage, CacheDir: cache, Env: []string{"DRYDOCK_GW_IP=10.0.0.1"},
			Argv: []string{"npm", "ci"}, MemoryGB: 4, CPUs: 2,
		}),
		"verify": runner.BuildVerifyArgs(runner.VerifySpec{
			TaskID: "0123456789abcdef0123456789abcdef", Network: "n", ImageRef: "img",
			VerifyDir: verify, Argv: []string{"go", "test", "./..."}, MemoryGB: 4, CPUs: 2,
		}),
	}
	// Exactly these host paths may be bound into a VM. A new one is a decision
	// somebody has to make deliberately, which is what this set forces.
	allowed := map[string]bool{stage: true, cache: true, verify: true}

	for name, argv := range argvs {
		mounts := 0
		for i, a := range argv {
			if a != "--mount" {
				continue
			}
			mounts++
			if i+1 >= len(argv) {
				t.Fatalf("%s: --mount with no spec", name)
			}
			src := mountSource(argv[i+1])
			if src == "" {
				t.Fatalf("%s: mount spec %q has no source=", name, argv[i+1])
			}
			if !allowed[src] {
				t.Errorf("%s: mounts an unexpected host path %q (spec %q); the allowed set is stage/cache/verify",
					name, src, argv[i+1])
			}
		}
		if mounts == 0 {
			t.Errorf("%s: no --mount found at all; the test would pass vacuously", name)
		}
		// Belt and braces: nothing anywhere in the argv (mount, env, or a
		// positional) may name the audit root or the ledger.
		joined := strings.Join(argv, "\x00")
		for _, forbidden := range []string{ledgerPath, ledgerDir, auditRoot} {
			if strings.Contains(joined, forbidden) {
				t.Errorf("%s: the VM argv references %q; the global ledger is host-only (plan G4)", name, forbidden)
			}
		}
	}
}

// TestGlobalCeiling_NoBrokerAuthoredRowCanSourceItsCostFromTheAgent.
//
// Task 4 found SIX terminal paths filling a row stamped src:"broker" from
// audit.TotalCost, which does not filter Src — laundering an agent-printed
// number into the exact field the aggregate-cap restart seed, boot
// reconciliation, a resumed task's spend recovery and every downstream
// src=="broker" filter trust. A src filter downstream is worth nothing if the
// broker itself will copy an agent's number into a broker row.
//
// That class cannot be prevented by a downstream test, so it is prevented HERE,
// structurally: every fmt.Fprintf in this package whose format both stamps
// src:"broker" AND carries a total_cost_usd must take that figure from the
// gateway lease — tr.meteredCostUSD() or tr.grant.Spent() — and from nothing
// else. The package is PARSED rather than grepped, so a comment discussing the
// old shortcut cannot make it pass.
func TestGlobalCeiling_NoBrokerAuthoredRowCanSourceItsCostFromTheAgent(t *testing.T) {
	fset, files := brokerPackageFiles(t)
	// The only expressions allowed to become a broker-authored row's cost.
	allowed := map[string]bool{
		"meteredCostUSD": true, // the lease, or a resumed task's own broker row
		"Spent":          true, // tr.grant.Spent(): the live lease itself
	}

	found := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 3 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Fprintf" {
					return true
				}
				format, ok := literalString(call.Args[1])
				if !ok || !strings.Contains(format, `"src":"broker"`) {
					return true
				}
				idx := strings.Index(format, "total_cost_usd")
				if idx < 0 {
					return true
				}
				found++
				argN := 2 + countVerbs(format[:idx])
				if argN >= len(call.Args) {
					t.Errorf("%s: could not locate the cost argument", fset.Position(call.Pos()))
					return true
				}
				expr := resolveToExpr(fn.Body, call.Args[argN])
				name := calleeName(expr)
				if !allowed[name] {
					t.Errorf("%s: a src:\"broker\" result row takes its total_cost_usd from %s, "+
						"which is not the gateway lease. Allowed: tr.meteredCostUSD() or tr.grant.Spent(). "+
						"Anything else can launder an agent-authored figure into a broker-stamped row (plan G4).",
						fset.Position(call.Pos()), describeExpr(expr))
				}
				return true
			})
		}
	}
	// THE SECOND SHAPE, and the net used to miss it entirely: a broker-authored
	// row built as a STRUCT LITERAL rather than an Fprintf format. The metrics
	// row is one (metrics.go's audit.Metrics{Type: "metrics", Src: "broker",
	// ..., CostUSD: ...}), and it carries a dollar figure into the same
	// operator-facing surfaces. A net that matches only format strings is a net
	// with a hole exactly the size of the next row someone writes idiomatically.
	structFound := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				var costField ast.Expr
				isBroker := false
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					if key.Name == "Src" {
						if v, ok := literalString(kv.Value); ok && v == "broker" {
							isBroker = true
						}
					}
					if strings.Contains(key.Name, "CostUSD") || strings.Contains(key.Name, "USD") {
						costField = kv.Value
					}
				}
				if !isBroker || costField == nil {
					return true
				}
				structFound++
				expr := resolveToExpr(fn.Body, costField)
				name := calleeName(expr)
				if !allowed[name] {
					t.Errorf("%s: a src:\"broker\" struct-literal row takes its dollar figure from %s, "+
						"which is not the gateway lease. Allowed: tr.meteredCostUSD() or tr.grant.Spent(). "+
						"Anything else can launder an agent-authored figure into a broker-stamped row (plan G4).",
						fset.Position(lit.Pos()), describeExpr(expr))
				}
				return true
			})
		}
	}
	if structFound < 1 {
		t.Errorf("matched no src:\"broker\" STRUCT-LITERAL rows carrying a dollar figure; metrics.go writes one, " +
			"so the struct-literal half of the net has stopped working")
	}

	// Task 4 converted six synthetic rows plus appendBrokerResult. Fewer than
	// that means the matcher stopped matching and the net is not catching
	// anything.
	if found < 7 {
		t.Fatalf("matched only %d broker-authored cost rows; the structural net has stopped working "+
			"(Task 4 converted six synthetic rows plus appendBrokerResult)", found)
	}
}

// TestGlobalCeiling_AuditTotalCostStaysDeleted. The shortcut that produced the
// laundering above was deleted outright rather than merely stopped being called,
// so that a future caller has to reintroduce the function — and read the
// tombstone comment explaining why it is gone — instead of reaching for a name
// that is still there.
func TestGlobalCeiling_AuditTotalCostStaysDeleted(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("../audit/*.go")
	if err != nil {
		t.Fatal(err)
	}
	parsed := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		parsed++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == "TotalCost" && fn.Recv == nil {
				t.Errorf("%s: audit.TotalCost is back. It returns the last result row WITHOUT "+
					"filtering Src, so an agent-authored total_cost_usd reaches every caller. "+
					"Use LastBrokerResult / LastRowsFile, and audit.Cost to display one.",
					fset.Position(fn.Pos()))
			}
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no files from ../audit; the test would be vacuous")
	}
}

// TestGlobalCeiling_NothingInTheCeilingReadsAnAgentWritableFile.
//
// The ledger's inputs must trace to broker-authored values only. The store, the
// enforcement and the headroom surface therefore touch the audit package NOT AT
// ALL — they have no business reading a <id>.jsonl, which carries the agent's
// stdout verbatim — and the one file that does (globalrecord.go, for a task
// resumed after a restart whose lease is long gone) may use only the
// src-FILTERED reader.
func TestGlobalCeiling_NothingInTheCeilingReadsAnAgentWritableFile(t *testing.T) {
	fset := token.NewFileSet()
	// The src-filtered readers. LastBrokerResult skips any row an agent wrote.
	allowedAuditCalls := map[string]bool{"LastBrokerResult": true}

	seen := 0
	for _, name := range []string{"globalledger.go", "globalcap.go", "globalheadroom.go", "globalrecord.go"} {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "audit" {
				return true
			}
			seen++
			if name != "globalrecord.go" {
				t.Errorf("%s: reads audit.%s. The ceiling's store, enforcement and headroom surface "+
					"must not touch a task trace at all — it carries the agent's own stdout.",
					fset.Position(sel.Pos()), sel.Sel.Name)
				return true
			}
			if !allowedAuditCalls[sel.Sel.Name] {
				t.Errorf("%s: reads audit.%s. Only the src-FILTERED readers may feed the ceiling; "+
					"an unfiltered one returns whatever the agent printed (plan G4).",
					fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	// Vacuity guard: globalrecord.go really does reach for a trace (the resumed
	// task's previous-process broker row), so the matcher above is live and a
	// pass means "only the filtered reader", not "the walk found nothing".
	if seen == 0 {
		t.Fatal("found no audit.* reference at all in the ceiling's files; the walk has stopped working")
	}
}

// --- small AST helpers for the two structural tests above ---

// literalString flattens a format expression that may be a single literal or a
// `"..." + "\n"` concatenation.
func literalString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, okL := literalString(v.X)
		r, okR := literalString(v.Y)
		if !okL || !okR {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// countVerbs counts the printf verbs in s, ignoring %%.
func countVerbs(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '%' {
			i++
			continue
		}
		for i++; i < len(s); i++ {
			c := s[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				break
			}
		}
		n++
	}
	return n
}

// resolveToExpr follows a bare identifier back to the expression it was
// assigned, so `cost := tr.meteredCostUSD(); Fprintf(..., cost)` is judged on
// what cost actually is.
func resolveToExpr(body *ast.BlockStmt, e ast.Expr) ast.Expr {
	id, ok := e.(*ast.Ident)
	if !ok {
		return e
	}
	var found ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		for _, lhs := range as.Lhs {
			if l, ok := lhs.(*ast.Ident); ok && l.Name == id.Name {
				found = as.Rhs[0]
			}
		}
		return true
	})
	if found != nil {
		return found
	}
	return e
}

// calleeName is the method name of a call expression like tr.meteredCostUSD().
func calleeName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func describeExpr(e ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// (C) The edges: the window's exact cutoff, and a restart.
// ---------------------------------------------------------------------------

// TestGlobalCeiling_WindowBoundaryAtTheCutoff, at the ENFORCEMENT level.
//
// globalledger_test.go pins the store's arithmetic. This asserts the thing an
// operator experiences, at the millisecond: the window is STRICTLY AFTER its
// cutoff, so a start sitting exactly ON the cutoff has aged out and the ceiling
// admits, while one a millisecond later still refuses.
//
// The exact convention matters more than which way it goes: gateway.spendLedger
// uses strictly-after too, so the per-vendor cap and the global ceiling can
// never disagree about a window of the same length — an operator comparing the
// two numbers is comparing like with like.
func TestGlobalCeiling_WindowBoundaryAtTheCutoff(t *testing.T) {
	const window = time.Hour
	windowMs := int64(window / time.Millisecond)

	for _, tc := range []struct {
		name    string
		endedAt int64
		want    bool
	}{
		{"one ms after the cutoff counts", capNow - windowMs + 1, true},
		{"exactly ON the cutoff does NOT count", capNow - windowMs, false},
		{"one ms before the cutoff does not count", capNow - windowMs - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := capBroker(t)
			b.GlobalMaxTasks = 1
			b.GlobalLedger = capLedgerAt(t, b.AuditRoot, window, 1, 0, tc.endedAt)
			blocked, why := b.globalCeilingExceeded("claude")
			if blocked != tc.want {
				t.Fatalf("blocked = %v, want %v (entry at now-%dms, window %dms) reason=%q",
					blocked, tc.want, capNow-tc.endedAt, windowMs, why)
			}
		})
	}
}

// TestGlobalCeiling_RefusalSurvivesARestartInBothWindowModes.
//
// G3's headline, at the enforcement level rather than the store's. The old
// aggregate cap is IN-MEMORY, and in total mode (aggregate_window: 0) it resets
// to $0 on every restart — so a crash loop resets the ceiling to zero, which is
// exactly the shape an unattended install fails in. This asserts the amnesia is
// gone in BOTH modes: the same exhausted ceiling, from a brand-new store over
// the same directory, still refuses.
func TestGlobalCeiling_RefusalSurvivesARestartInBothWindowModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
	}{
		{"windowed (24h)", 24 * time.Hour},
		{"total mode (global_window: 0)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()

			// --- life one: run the ceiling out ---
			b1 := capBroker(t)
			b1.AuditRoot = root
			b1.GlobalMaxTasks, b1.GlobalBudgetUSD = 3, 10
			b1.GlobalLedger = capLedgerAt(t, root, tc.window, 3, 4, capNow)
			blocked, why := b1.globalCeilingExceeded("claude")
			if !blocked {
				t.Fatal("the ceiling did not trip in the first process")
			}
			usageBefore := b1.GlobalLedger.Usage(capNow)

			// --- the restart: a brand-new store over the same directory ---
			b2 := capBroker(t)
			b2.AuditRoot = root
			b2.GlobalMaxTasks, b2.GlobalBudgetUSD = 3, 10
			l2, err := OpenGlobalLedger(root, tc.window, capNow)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			b2.GlobalLedger = l2

			blockedAfter, whyAfter := b2.globalCeilingExceeded("claude")
			if !blockedAfter {
				t.Fatalf("the ceiling ADMITTED after a restart; the old in-memory aggregate cap's "+
					"total-mode amnesia is exactly the hole this ledger exists to close (was %q)", why)
			}
			after := l2.Usage(capNow)
			if after.Starts != usageBefore.Starts || after.USD != usageBefore.USD {
				t.Errorf("usage after restart = {%d starts, $%.2f}, want {%d, $%.2f}",
					after.Starts, after.USD, usageBefore.Starts, usageBefore.USD)
			}
			if after.Degraded || after.StartsDegraded {
				t.Errorf("a clean restart reported degraded: %q / %q", after.DegradedReason, after.StartsDegradedReason)
			}
			if whyAfter == "" {
				t.Error("the post-restart refusal carries no reason")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (D) Crashes and clocks.
// ---------------------------------------------------------------------------

// TestGlobalCeiling_ACrashedTaskConvergesWithoutDoubleCounting.
//
// The messiest real shape: a task is admitted (so this process holds an
// in-flight claim), and then it never terminals — the daemon is killed, or the
// lifecycle dies somewhere that never reaches recordGlobalUsage. The audit trace
// exists; the ledger entry does not.
//
// Two independent mechanisms then converge on that task, and the danger is that
// they converge on TWO:
//
//	the in-flight claim  — counted while the ledger does not hold the id
//	boot reconciliation  — adds an entry for a task the audit has and the
//	                       ledger does not (over-counting on ambiguity, G3)
//
// The claim is keyed on task IDENTITY and excludes an id the ledger already has,
// which is what makes the sum exact in either order. A bare counter would have
// counted it twice — silently tightening the ceiling by up to
// max_concurrent_tasks, on every crash.
func TestGlobalCeiling_ACrashedTaskConvergesWithoutDoubleCounting(t *testing.T) {
	b := capBroker(t)
	b.GlobalMaxTasks = 10
	b.GlobalLedger = capLedgerAt(t, b.AuditRoot, 24*time.Hour, 0, 0, capNow)

	id := newID()
	if blocked, why := b.admitGlobalStart(id, "claude"); blocked {
		t.Fatalf("admission refused against an empty ledger: %s", why)
	}
	// It never terminals: no ledger entry, and the claim is never released.
	if got := ceilingStarts(t, b); got != 1 {
		t.Fatalf("an admitted, unrecorded task counts %d times, want 1", got)
	}

	// The recovery a restart performs, replayed in-process: an entry derived
	// from the audit trace for a task the ledger is missing.
	if err := b.GlobalLedger.Record(capNow, GlobalEntry{
		Kind: GlobalEntryTask, TaskID: id, EndedAtMs: capNow,
		Vendor: "anthropic", Agent: "claude", Auth: "api_key", Metered: true,
		USD: 0, USDTrusted: false, Outcome: "crashed", Src: GlobalSrcReconcile,
	}); err != nil {
		t.Fatalf("reconcile record: %v", err)
	}
	if got := ceilingStarts(t, b); got != 1 {
		t.Fatalf("after recovery the crashed task counts %d times, want exactly 1 "+
			"(the claim must exclude an id the ledger already holds)", got)
	}

	// Releasing the claim — the defer that would have run had the process
	// survived — must not drop it back to zero either.
	b.releaseGlobalStart(id)
	if got := ceilingStarts(t, b); got != 1 {
		t.Fatalf("releasing the claim of a recorded task left %d starts, want 1 "+
			"(the durable entry is the surviving record)", got)
	}

	// A second recovery pass (a second boot before anything ages out) is
	// suppressed by the id, so a crash loop cannot inflate the ceiling.
	if err := b.GlobalLedger.Record(capNow, GlobalEntry{
		Kind: GlobalEntryTask, TaskID: id, EndedAtMs: capNow, Src: GlobalSrcReconcile,
	}); err != nil {
		t.Fatalf("second reconcile record: %v", err)
	}
	if got := ceilingStarts(t, b); got != 1 {
		t.Fatalf("a replayed recovery counted the same task %d times", got)
	}
}

// ceilingStarts is what the ceiling actually compares against global_max_tasks:
// recorded starts plus the in-flight claims the ledger has not counted yet.
func ceilingStarts(t *testing.T, b *Broker) int {
	t.Helper()
	b.capMu.Lock()
	defer b.capMu.Unlock()
	usage, inflight := b.GlobalLedger.UsageWithClaims(b.ceilingNowMs(), b.capInFlight, "")
	return usage.Starts + inflight
}

// TestGlobalCeiling_AClockChangeAcrossARestartIsLoggedNotSilent.
//
// I3's residual, pinned so it cannot change silently. An in-process forward
// clock jump is DETECTED (globalcap.go anchors against monotonic time, which the
// wall clock cannot corrupt) and the window is corrected — that is
// TestGlobalCap_AForwardClockJumpCannotZeroTheLimbs.
//
// A clock change while the daemon is DOWN cannot be detected: a monotonic
// reading does not survive a reboot, and every timestamp on disk came from the
// same wall clock that moved. It is deliberately LOGGED rather than refused,
// because "every entry is older than the window" is the same picture as a
// legitimately idle install, and an unattended daemon that cannot start a task
// after a quiet week is a worse failure than a one-window reset.
//
// So the documented behaviour is: the ceiling reads zero and ADMITS, and the
// operator is told. Both halves are asserted, including that the warning names
// both possibilities rather than accusing the clock.
func TestGlobalCeiling_AClockChangeAcrossARestartIsLoggedNotSilent(t *testing.T) {
	const window = time.Hour
	root := t.TempDir()

	l := mustOpenGL(t, root, window, capNow)
	for i := 0; i < 5; i++ {
		mustRecord(t, l, capNow, glEntry(glTaskID(i), capNow, 10))
	}

	// The daemon goes down; the wall clock moves forward by a day; it comes up.
	jumped := capNow + int64(24*time.Hour/time.Millisecond)
	logs := captureSlog(t)
	l2, err := OpenGlobalLedger(root, window, jumped)
	if err != nil {
		t.Fatalf("reopen after a clock change: %v", err)
	}

	u := l2.Usage(jumped)
	if u.Starts != 0 || u.USD != 0 {
		t.Fatalf("usage after a forward clock change across a restart = {%d starts, $%.2f}; "+
			"the DOCUMENTED behaviour is that the window reads empty. If this changed, the "+
			"THREAT_MODEL residual and the code comments must change with it", u.Starts, u.USD)
	}
	// It is not refused — and that is deliberate. Refusing here would refuse
	// every legitimately idle install too.
	b := capBroker(t)
	b.GlobalMaxTasks, b.GlobalBudgetUSD = 3, 5
	b.GlobalLedger = l2
	b.now = func() int64 { return jumped }
	if blocked, why := b.globalCeilingExceeded("claude"); blocked {
		t.Fatalf("an idle-or-clock-changed install was refused: %q. That would also refuse every "+
			"daemon that was simply quiet for longer than its window", why)
	}
	// But it is NOT silent.
	got := logs.String()
	if !strings.Contains(got, "older than the rolling window at boot") {
		t.Fatalf("no boot warning for a ledger whose every entry is out of window; the one case "+
			"the ceiling cannot detect must at least be visible. Log was:\n%s", got)
	}
	for _, want := range []string{"idle", "clock"} {
		if !strings.Contains(got, want) {
			t.Errorf("the boot warning does not mention %q; it must name BOTH possibilities "+
				"rather than accusing one. Log was:\n%s", want, got)
		}
	}
	// The entries are still THERE — pruning anchors to the ledger's own newest
	// event, so a clock jump can reset the WINDOW but never destroy the record.
	if lines := rawLines(t, l2.Path()); len(lines) != 5 {
		t.Errorf("the ledger file holds %d lines after a forward clock change, want 5; "+
			"a jumped clock must not delete the durable record", len(lines))
	}
}

// captureSlog redirects the default logger into a buffer for the duration of a
// test. Not parallel-safe, and none of these tests are parallel.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
