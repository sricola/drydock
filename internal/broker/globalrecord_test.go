package broker

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"drydock/internal/config"
	"drydock/internal/creds"
	"drydock/internal/trustbrief"
)

// These tests drive the GLOBAL CEILING's RECORDING half (globalrecord.go, plan
// Task 3): the durable ledger entry a task terminal writes.
//
// The properties, one angle each:
//
//	(a) EVERY TERMINAL PATH RECORDS, and that is proved STRUCTURALLY — by
//	    parsing the package — not by a list of paths someone has to remember to
//	    extend. Two independent structural nets, plus a live drive of every
//	    outcome the lifecycle can produce.
//	(b) THE USD COMES FROM THE LEASE, never from the audit's total_cost_usd
//	    (G4) — including the revoked-lease trap, where reading it one defer too
//	    late would silently record $0 for every task.
//	(c) AN UNMETERED LANE RECORDS A START whose dollars are declared
//	    untrustworthy, not a measured zero.
//	(d) RECORDING IS IDEMPOTENT and the in-flight claim outlives it.

// ---- harness ----

const recNow = int64(1_700_000_000_000)

// ledgerBroker wires testBroker's lifecycle seams to a real durable ledger and
// a deterministic clock, so a test can drive a whole task and then read what
// the terminal actually wrote.
func ledgerBroker(t *testing.T, st taskStage, grant *fakeGrant,
	run func(context.Context, []string, io.Writer, io.Writer) error) (*Broker, *GlobalLedger) {
	t.Helper()
	b := testBroker(t, "anthropic", st, grant, run)
	clk := &testClock{}
	clk.set(recNow)
	b.now = clk.now
	l, err := OpenGlobalLedger(b.AuditRoot, 24*time.Hour, recNow)
	if err != nil {
		t.Fatalf("OpenGlobalLedger: %v", err)
	}
	b.GlobalLedger = l
	return b, l
}

// onlyEntry returns the single ledger entry for a task id, failing when the
// ledger holds anything other than exactly one entry for it. "Exactly one" is
// the property under test on every terminal path: zero under-counts the
// ceiling, more than one over-counts it.
func onlyEntry(t *testing.T, l *GlobalLedger, id string) GlobalEntry {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []GlobalEntry
	for _, e := range l.entries {
		if e.TaskID == id {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("ledger holds %d entries for task %s, want exactly 1: %+v", len(found), id, found)
	}
	return found[0]
}

// submittedID pulls the task id out of the stream's `accepted` event.
func submittedID(t *testing.T, events []map[string]any) string {
	t.Helper()
	for _, ev := range events {
		if ev["event"] == "accepted" {
			if id, _ := ev["task_id"].(string); id != "" {
				return id
			}
		}
	}
	t.Fatal("no accepted event in the stream; the task never started")
	return ""
}

// brokerPackageFiles parses every non-test .go file of this package. The
// structural tests below PARSE rather than grep, so a comment that mentions a
// call cannot make them pass (the same discipline globalledger_test.go uses to
// prove there is no time.Now in the store).
func brokerPackageFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, f)
	}
	if len(files) < 10 {
		t.Fatalf("only %d package files parsed; the structural tests would be vacuous", len(files))
	}
	return fset, files
}

// selectorName renders a call's callee as "recv.Method" (or "Func"), which is
// all the structural tests need to match on.
func selectorName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name
		}
		return "." + fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

// funcFacts is what the structural tests need to know about one function.
type funcFacts struct {
	name             string
	defersRecord     bool
	defersMetrics    bool
	callsRunLifecyle bool
	buildsTaskRun    bool
	firstStmtRecord  bool
	outcomeLiterals  []string
	// Positions, so the ordering of a defer relative to a call can be checked:
	// a defer registered before a call fires after that call returns.
	releaseDeferPos token.Pos
	runLifecyclePos token.Pos
}

func scanBrokerFuncs(t *testing.T) map[string]*funcFacts {
	t.Helper()
	_, files := brokerPackageFiles(t)
	out := map[string]*funcFacts{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ff := &funcFacts{name: fn.Name.Name}
			if len(fn.Body.List) > 0 {
				if d, ok := fn.Body.List[0].(*ast.DeferStmt); ok &&
					strings.HasSuffix(selectorName(d.Call), ".recordGlobalUsage") {
					ff.firstStmtRecord = true
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.DeferStmt:
					switch {
					case strings.HasSuffix(selectorName(x.Call), ".recordGlobalUsage"):
						ff.defersRecord = true
					case strings.HasSuffix(selectorName(x.Call), ".appendMetrics"):
						ff.defersMetrics = true
					case strings.HasSuffix(selectorName(x.Call), ".releaseGlobalStart"):
						if !ff.releaseDeferPos.IsValid() {
							ff.releaseDeferPos = x.Pos()
						}
					}
				case *ast.CallExpr:
					if strings.HasSuffix(selectorName(x), ".runLifecycle") {
						ff.callsRunLifecyle = true
						ff.runLifecyclePos = x.Pos()
					}
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "taskRun" {
						ff.buildsTaskRun = true
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "outcome" || i >= len(x.Rhs) {
							continue
						}
						if lit, ok := x.Rhs[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil && v != "" {
								ff.outcomeLiterals = append(ff.outcomeLiterals, v)
							}
						}
					}
				}
				return true
			})
			out[ff.name] = ff
		}
	}
	return out
}

// ---- (a) every terminal path records, STRUCTURALLY ----

// TestGlobalRecord_TheTerminalWriteIsStructural is the answer to "how do you
// know a terminal path added next year will record?".
//
// appendBrokerResult carries the same must-run-on-every-exit-path rule and
// satisfies it by ENUMERATION — nine call sites that a tenth terminal can
// silently fail to join. The ledger write satisfies it by PLACEMENT: one defer
// on the first line of the single shared lifecycle function. This test pins
// that placement from the AST, so moving it, deleting it, or adding a second
// lifecycle driver that forgets it all fail here rather than in production as a
// quietly under-counted ceiling.
func TestGlobalRecord_TheTerminalWriteIsStructural(t *testing.T) {
	fns := scanBrokerFuncs(t)

	t.Run("runLifecycle records on its FIRST line", func(t *testing.T) {
		rl := fns["runLifecycle"]
		if rl == nil {
			t.Fatal("runLifecycle not found; this test no longer checks what it claims to")
		}
		if !rl.firstStmtRecord {
			t.Error("runLifecycle's first statement must be `defer tr.recordGlobalUsage()`. " +
				"Registered any later, every terminal that returns before that point " +
				"(a clone failure, a stage-quota failure, the disk/push preflights, an " +
				"unresolvable agent, a mint failure) silently stops counting against the ceiling.")
		}
	})

	t.Run("every task-terminal owner records", func(t *testing.T) {
		// appendMetrics is the package's existing declaration of "this function
		// owns a task's terminal". Every function that makes that declaration
		// must also record to the ledger; a new lifecycle that writes a metrics
		// row without a ledger entry fails here.
		owners := 0
		for name, ff := range fns {
			if !ff.defersMetrics {
				continue
			}
			owners++
			if !ff.defersRecord {
				t.Errorf("%s defers appendMetrics (it owns a task terminal) but never defers recordGlobalUsage; "+
					"that terminal's start and spend would never reach the global ledger", name)
			}
		}
		if owners < 2 {
			t.Fatalf("found %d functions deferring appendMetrics, want at least 2 (runLifecycle and resumePush); the scan is broken", owners)
		}
	})

	t.Run("every lifecycle driver is covered", func(t *testing.T) {
		// The second, independent net: anything that BUILDS a taskRun is driving
		// a task, so it must either record itself or delegate to runLifecycle
		// (which does). Catches a brand-new terminal path that skips the metrics
		// row too.
		drivers := 0
		for name, ff := range fns {
			if !ff.buildsTaskRun {
				continue
			}
			drivers++
			if !ff.defersRecord && !ff.callsRunLifecyle {
				t.Errorf("%s constructs a taskRun but neither defers recordGlobalUsage nor delegates to runLifecycle; "+
					"tasks it drives would never be counted against the global ceiling", name)
			}
		}
		if drivers < 2 {
			t.Fatalf("found %d taskRun constructors, want at least 2 (HandleTask/runQueued and resumePush); the scan is broken", drivers)
		}
	})
}

// ---- (a) every terminal path records, LIVE ----

// terminalCase drives one real lifecycle terminal end to end.
type terminalCase struct {
	// outcomes are the tr.outcome values this case covers. The table is
	// cross-checked against the package source below, so a NEW terminal outcome
	// added without a case here fails the suite.
	outcomes []string
	// build wires the broker for this terminal and returns the submit body.
	build func(t *testing.T) (*Broker, *GlobalLedger, string)
	// wantUSD, when non-nil, pins the recorded broker-metered figure.
	wantUSD *float64
	// unresolvedVendor marks a terminal that fires before the agent resolves,
	// so the ledger cannot claim the lane meters at all.
	unresolvedVendor bool
}

func usd(v float64) *float64 { return &v }

func terminalCases() map[string]terminalCase {
	const diff = "diff --git a/x b/x\n@@ -0,0 +1 @@\n+y\n"
	const okLine = `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":9.99,"num_turns":1}`
	body := `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`
	gatedBody := `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude"}`

	return map[string]terminalCase{
		"pushed": {
			outcomes: []string{"pushed"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff},
					&fakeGrant{spent: 0.25}, writesResult(okLine))
				return b, l, body
			},
			wantUSD: usd(0.25),
		},
		"no_diff": {
			outcomes: []string{"no_diff"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir()},
					&fakeGrant{spent: 0.1}, writesResult(okLine))
				return b, l, body
			},
			wantUSD: usd(0.1),
		},
		"planned": {
			outcomes: []string{"planned"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), plan: "the plan", planOK: true},
					&fakeGrant{spent: 0.3}, writesResult(okLine))
				return b, l, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","plan_only":true}`
			},
			wantUSD: usd(0.3),
		},
		"agent run failed": {
			outcomes: []string{"error"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				run := func(context.Context, []string, io.Writer, io.Writer) error {
					return errors.New("the agent VM died")
				}
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff}, &fakeGrant{spent: 0.4}, run)
				return b, l, body
			},
			// The lease metered real spend before the VM died; it must count.
			wantUSD: usd(0.4),
		},
		"cancelled mid-run": {
			outcomes: []string{"cancelled"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				var b *Broker
				run := func(ctx context.Context, _ []string, _, _ io.Writer) error {
					b.CancelAll()
					<-ctx.Done()
					return ctx.Err()
				}
				var l *GlobalLedger
				b, l = ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff}, &fakeGrant{spent: 0.5}, run)
				return b, l, body
			},
			wantUSD: usd(0.5),
		},
		"denied at the diff gate": {
			outcomes: []string{"denied"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff},
					&fakeGrant{spent: 0.6}, writesResult(okLine))
				b.ApprovalTimeout = 50 * time.Millisecond
				return b, l, gatedBody
			},
			wantUSD: usd(0.6),
		},
		"policy_blocked": {
			outcomes: []string{"policy_blocked"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				twoFile := diff + "diff --git a/b.go b/b.go\n@@ -0,0 +1 @@\n+z\n"
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: twoFile},
					&fakeGrant{spent: 0.7}, writesResult(okLine))
				b.DiffPolicy = config.DiffPolicy{MaxFilesChanged: 1}
				return b, l, body
			},
			wantUSD: usd(0.7),
		},
		"push_failed": {
			outcomes: []string{"push_failed"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				st := &fakeStage{workDir: t.TempDir(), diff: diff, pushErr: errors.New("remote rejected")}
				b, l := ledgerBroker(t, st, &fakeGrant{spent: 0.8}, writesResult(okLine))
				b.PushMaxRetries = 0
				return b, l, body
			},
			wantUSD: usd(0.8),
		},
		"setup_failed before the agent ran": {
			outcomes: []string{"setup_failed"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				exitErr := realExitErr(t)
				log := &runLog{}
				setupFn := func(context.Context, []string, io.Writer, io.Writer) error { return exitErr }
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff},
					&fakeGrant{spent: 0.9}, setupSplitRun(log, setupFn))
				b.Setup = map[string]SetupProfile{"github.com/o/r": {Setup: [][]string{{"npm", "ci"}}}}
				return b, l, body
			},
			// FAIL-CLOSED-BEFORE-SPEND: no bearer ever entered a VM, so the
			// lease was never even read. $0 here is a fact, not an estimate —
			// and the START still counts, which is the whole point of G1's
			// second limb.
			wantUSD: usd(0),
		},
		"verify_failed": {
			outcomes: []string{"verify_failed"},
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				exitErr := realExitErr(t)
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff}, &fakeGrant{spent: 1.1},
					verifySplitRun(okLine, func(context.Context, []string, io.Writer, io.Writer) error { return exitErr }))
				b.Verify = map[string]VerifyRepo{
					"github.com/o/r": {Commands: [][]string{{"go", "test"}}, Required: true},
				}
				return b, l, body
			},
			wantUSD: usd(1.1),
		},
		"aborted before the audit log exists": {
			// NOT a tr.outcome value — this terminal happens before the audit
			// log (and therefore before appendMetrics) exists at all. It is the
			// exact class of path a metrics-hook-based ledger write would miss,
			// and every one of them is still a real task START.
			outcomes: nil,
			build: func(t *testing.T) (*Broker, *GlobalLedger, string) {
				b, l := ledgerBroker(t, &fakeStage{workDir: t.TempDir(), diff: diff},
					&fakeGrant{}, writesResult(okLine))
				b.prepareStage = func(context.Context, string, string) (taskStage, error) {
					return nil, errors.New("clone failed")
				}
				return b, l, body
			},
			wantUSD:          usd(0),
			unresolvedVendor: true,
		},
	}
}

// verifySplitRun hands verify-<id> VM runs to verifyFn and answers every other
// run (the agent VM) with agentLine.
func verifySplitRun(agentLine string, verifyFn func(context.Context, []string, io.Writer, io.Writer) error,
) func(context.Context, []string, io.Writer, io.Writer) error {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		if strings.HasPrefix(setupContainerName(args), "verify-") {
			return verifyFn(ctx, args, stdout, stderr)
		}
		fmt.Fprintln(stdout, agentLine)
		return nil
	}
}

// TestGlobalRecord_EveryTerminalRecordsExactlyOneEntry drives the REAL
// lifecycle to every terminal it can reach and asserts each wrote exactly one
// ledger entry carrying the lease's spend.
//
// The table is not the safety net — the structural test above is. This is the
// proof that the structural placement actually produces a correct entry on each
// concrete path, including the ones that terminate before the audit log opens.
func TestGlobalRecord_EveryTerminalRecordsExactlyOneEntry(t *testing.T) {
	for name, tc := range terminalCases() {
		t.Run(name, func(t *testing.T) {
			b, l, body := tc.build(t)
			_, events, term := submitTerminates(t, b, body)
			id := submittedID(t, events)

			e := onlyEntry(t, l, id)
			if e.Kind != GlobalEntryTask {
				t.Errorf("kind = %q, want %q", e.Kind, GlobalEntryTask)
			}
			if e.Src != GlobalSrcTerminal {
				t.Errorf("src = %q, want %q (this entry came from the task-terminal path)", e.Src, GlobalSrcTerminal)
			}
			if e.EndedAtMs != recNow {
				t.Errorf("ended_at_ms = %d, want the broker clock's %d", e.EndedAtMs, recNow)
			}
			if tc.wantUSD != nil && e.USD != *tc.wantUSD {
				t.Errorf("usd = %v, want %v (the LEASE's metered spend, terminal=%v)", e.USD, *tc.wantUSD, term)
			}
			// Every lane that resolved is metered (anthropic, api_key), so the
			// figure must be recorded as trustworthy. A terminal that fired
			// before the agent resolved has no lane to make a claim about.
			if wantMetered := !tc.unresolvedVendor; e.Metered != wantMetered || e.USDTrusted != wantMetered {
				t.Errorf("metered=%v usd_trusted=%v, want both %v", e.Metered, e.USDTrusted, wantMetered)
			}
			// And it counts as exactly one start against the G1 task limb.
			if u := l.Usage(recNow); u.Starts != 1 {
				t.Errorf("usage.Starts = %d, want 1", u.Starts)
			}
			for _, want := range tc.outcomes {
				if e.Outcome != want {
					t.Errorf("outcome = %q, want %q", e.Outcome, want)
				}
			}
		})
	}
}

// TestGlobalRecord_TerminalOutcomeTableIsComplete closes the loop on the table
// above: it reads every `tr.outcome = "..."` literal out of the package source
// and fails if the live table does not cover it. Adding a new terminal outcome
// without driving it here is a test failure, not a silently untested path.
func TestGlobalRecord_TerminalOutcomeTableIsComplete(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range terminalCases() {
		for _, o := range tc.outcomes {
			covered[o] = true
		}
	}
	fns := scanBrokerFuncs(t)
	inSource := map[string]bool{}
	for _, ff := range fns {
		for _, o := range ff.outcomeLiterals {
			inSource[o] = true
		}
	}
	if len(inSource) < 8 {
		t.Fatalf("only %d outcome literals found in the package source; the scan is broken", len(inSource))
	}
	var missing []string
	for o := range inSource {
		if !covered[o] {
			missing = append(missing, o)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("terminal outcome(s) %v exist in the lifecycle but are not driven by terminalCases(); "+
			"add a case proving the new terminal records exactly one ledger entry", missing)
	}
}

// ---- (b) the USD is the lease's, never the audit's ----

// TestGlobalRecord_USDIsTheLeaseNotTheAudit is G4 as a test. The agent prints a
// wildly different total_cost_usd than the gateway metered; audit.TotalCost
// returns the AGENT's number for exactly this trace shape, and the ledger must
// record the gateway's.
func TestGlobalRecord_USDIsTheLeaseNotTheAudit(t *testing.T) {
	// The agent's own result row claims almost nothing was spent. A broker that
	// recorded this number would let a compromised CLI deflate the ceiling.
	forged := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"total_cost_usd":0.0001,"num_turns":1}`
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b, l := ledgerBroker(t, st, &fakeGrant{spent: 42.5}, writesResult(forged))

	_, events, _ := submitTerminates(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id := submittedID(t, events)

	e := onlyEntry(t, l, id)
	if e.USD != 42.5 {
		t.Errorf("usd = %v, want the LEASE's 42.5 — never the agent-reported 0.0001 (plan G4)", e.USD)
	}
	if !e.USDTrusted {
		t.Error("usd_trusted = false on a metered api_key lane with a live lease")
	}
	if u := l.Usage(recNow); u.USD != 42.5 {
		t.Errorf("the USD limb sums to %v, want 42.5", u.USD)
	}
}

// TestGlobalRecord_SurvivesTheGrantRevoke is the trap this design exists to
// avoid, pinned so it cannot come back.
//
// `defer grant.Revoke()` is registered at mint time, and the ledger write is
// deferred EARLIER (runLifecycle's first line), so it runs LATER — after the
// lease is gone. gateway's grant.Spent() answers 0 for a revoked token, so a
// terminal that read the lease at write time would record $0 for EVERY TASK: a
// total, silent deflation of the USD limb that no test of the write path alone
// would catch. The figure is snapshotted in runSandbox instead.
func TestGlobalRecord_SurvivesTheGrantRevoke(t *testing.T) {
	grant := &revokingGrant{spent: 7.25}
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, nil, writesResult(`{"type":"result","subtype":"success"}`))
	b.Providers = map[string]creds.Provider{"anthropic": revokingProvider{grant}}
	clk := &testClock{}
	clk.set(recNow)
	b.now = clk.now
	l, err := OpenGlobalLedger(b.AuditRoot, 24*time.Hour, recNow)
	if err != nil {
		t.Fatal(err)
	}
	b.GlobalLedger = l

	_, events, _ := submitTerminates(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id := submittedID(t, events)

	if !grant.revoked {
		t.Fatal("the grant was never revoked; this test no longer exercises the trap it exists for")
	}
	e := onlyEntry(t, l, id)
	if e.USD != 7.25 {
		t.Errorf("usd = %v, want 7.25: the lease's spend must be snapshotted while the lease is ALIVE, "+
			"not read after `defer grant.Revoke()` has zeroed it", e.USD)
	}
}

// revokingGrant reproduces the gateway grant's real behavior: Spent() answers 0
// once the lease has been revoked.
type revokingGrant struct {
	spent   float64
	revoked bool
}

func (g *revokingGrant) EnvVars() []string { return []string{"ANTHROPIC_AUTH_TOKEN=tok_test"} }
func (g *revokingGrant) Revoke() error     { g.revoked = true; return nil }
func (g *revokingGrant) Spent() float64 {
	if g.revoked {
		return 0
	}
	return g.spent
}

type revokingProvider struct{ g *revokingGrant }

func (p revokingProvider) Mint(float64) (creds.Grant, error) { return p.g, nil }

// ---- (c) unmetered lanes ----

// TestGlobalRecord_UnmeteredLaneRecordsAStartWithUntrustedUSD: a subscription
// (or priceless openai-compat) lane meters at $0 BY CONSTRUCTION. Recording
// that as a measured zero would let the USD limb average itself down with
// fiction. It is recorded as untrustworthy — and the task still counts as a
// START, which is the limb that actually bounds subscription mode (G1).
func TestGlobalRecord_UnmeteredLaneRecordsAStartWithUntrustedUSD(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b, l := ledgerBroker(t, st, &fakeGrant{spent: 0}, writesResult(`{"type":"result","subtype":"success"}`))
	// The SAME signal writeBrief keys on, so the two can never disagree about
	// which lanes carry real dollars.
	b.UnmeteredVendors = map[string]bool{"anthropic": true}
	b.AnthropicAuth = "subscription"

	_, events, _ := submitTerminates(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id := submittedID(t, events)

	e := onlyEntry(t, l, id)
	if e.Metered {
		t.Error("metered = true on a lane in UnmeteredVendors")
	}
	if e.USDTrusted {
		t.Error("usd_trusted = true on an unmetered lane: its $0 is a construction, not a measurement")
	}
	if e.Auth != "subscription" {
		t.Errorf("auth = %q, want %q", e.Auth, "subscription")
	}
	u := l.Usage(recNow)
	if u.Starts != 1 {
		t.Errorf("usage.Starts = %d, want 1 — an unmetered task is still a task START, the limb that bounds subscription mode", u.Starts)
	}
	if u.USD != 0 {
		t.Errorf("usage.USD = %v, want 0: an untrusted figure must never be summed into the USD limb", u.USD)
	}
}

// TestGlobalRecord_UnresolvedVendorIsNotClaimedAsMetered: a task that dies
// before its agent resolves has no vendor, so the ledger must not assert that
// its lane meters. The start still counts.
func TestGlobalRecord_UnresolvedVendorIsNotClaimedAsMetered(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b, l := ledgerBroker(t, st, &fakeGrant{}, writesResult(`{"type":"result","subtype":"success"}`))
	b.prepareStage = func(context.Context, string, string) (taskStage, error) {
		return nil, errors.New("clone failed")
	}
	_, events, _ := submitTerminates(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id := submittedID(t, events)

	e := onlyEntry(t, l, id)
	if e.Metered || e.USDTrusted {
		t.Errorf("metered=%v usd_trusted=%v, want both false when the vendor never resolved", e.Metered, e.USDTrusted)
	}
	if u := l.Usage(recNow); u.Starts != 1 {
		t.Errorf("usage.Starts = %d, want 1: an aborted task consumed an admission and is a task start", u.Starts)
	}
}

// ---- (d) idempotency, ordering, and the resumed terminal ----

// TestGlobalRecord_IsIdempotentOnReplay: recording the same task id twice —
// a crash-replayed terminal, or boot reconciliation racing a resumed terminal
// — counts it once. Task ids are 128-bit random, so "already present" can never
// suppress a distinct start.
func TestGlobalRecord_IsIdempotentOnReplay(t *testing.T) {
	root := t.TempDir()
	l, err := OpenGlobalLedger(root, 24*time.Hour, recNow)
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{AuditRoot: root, GlobalLedger: l}
	clk := &testClock{}
	clk.set(recNow)
	b.now = clk.now
	tr := &taskRun{b: b, id: newID(), taskVendor: "anthropic", agentName: "claude",
		outcome: "pushed", leaseSpentUSD: 3, leaseCaptured: true}

	for i := 0; i < 3; i++ {
		tr.recordGlobalUsage()
	}
	if u := l.Usage(recNow); u.Starts != 1 || u.USD != 3 {
		t.Fatalf("after 3 replays: starts=%d usd=%v, want 1 and 3", u.Starts, u.USD)
	}
	// And across a restart: the durable file must not hold three lines either.
	reopened, err := OpenGlobalLedger(root, 24*time.Hour, recNow)
	if err != nil {
		t.Fatal(err)
	}
	if u := reopened.Usage(recNow); u.Starts != 1 || u.USD != 3 {
		t.Fatalf("after restart: starts=%d usd=%v, want 1 and 3", u.Starts, u.USD)
	}
}

// TestGlobalRecord_ClaimIsReleasedAfterTheEntryLands pins the ordering
// globalcap.go reasons about: the in-flight claim OUTLIVES the ledger write, so
// a task is briefly both claimed and recorded. globalcap's tally keys on
// identity and skips a claimed id the ledger already has, so that instant is
// counted exactly ONCE — a bare counter would have counted it twice (silently
// tightening the ceiling by up to max_concurrent_tasks) or, with the opposite
// ordering, zero times (loosening it).
//
// It is proved in three layers because no runtime seam fires BETWEEN the write
// and the release: the placement is structural, the instant itself is exercised
// directly, and the end-to-end run asserts nothing leaks.
func TestGlobalRecord_ClaimIsReleasedAfterTheEntryLands(t *testing.T) {
	t.Run("structurally: the release is deferred before the lifecycle runs", func(t *testing.T) {
		// A defer registered BEFORE a call fires AFTER that call returns, and
		// runLifecycle's own first-line defer writes the entry inside it. So
		// this ordering plus TestGlobalRecord_TheTerminalWriteIsStructural is
		// the proof. If someone moves the release above the claim, or below the
		// lifecycle call, this fails.
		fns := scanBrokerFuncs(t)
		checked := 0
		for name, ff := range fns {
			if !ff.callsRunLifecyle {
				continue
			}
			checked++
			if !ff.releaseDeferPos.IsValid() {
				t.Errorf("%s drives runLifecycle but never defers releaseGlobalStart; a leaked claim is a permanent unit of ceiling", name)
				continue
			}
			if ff.releaseDeferPos > ff.runLifecyclePos {
				t.Errorf("%s registers `defer releaseGlobalStart` AFTER calling runLifecycle, so the claim is dropped "+
					"before the terminal ledger write lands", name)
			}
		}
		if checked < 2 {
			t.Fatalf("found %d runLifecycle drivers, want at least 2 (HandleTask and runQueued); the scan is broken", checked)
		}
	})

	t.Run("the claimed-and-recorded instant is counted exactly once", func(t *testing.T) {
		root := t.TempDir()
		l, err := OpenGlobalLedger(root, 24*time.Hour, recNow)
		if err != nil {
			t.Fatal(err)
		}
		b := &Broker{
			AuditRoot: root, GlobalLedger: l, GlobalMaxTasks: 5,
			Providers: map[string]creds.Provider{"anthropic": freshMintProvider{}}, DefaultAgent: "claude",
		}
		clk := &testClock{}
		clk.set(recNow)
		b.now = clk.now

		id := newID()
		if blocked, why := b.admitGlobalStart(id, "claude"); blocked {
			t.Fatalf("admission refused: %s", why)
		}
		// Claimed, not yet recorded: the tally must count it.
		b.capMu.Lock()
		_, before := b.GlobalLedger.UsageWithClaims(b.nowMs(), b.capInFlight, "")
		b.capMu.Unlock()
		if before != 1 {
			t.Fatalf("uncounted in-flight = %d before the write, want 1", before)
		}
		tr := &taskRun{b: b, id: id, taskVendor: "anthropic", agentName: "claude",
			outcome: "pushed", leaseSpentUSD: 1, leaseCaptured: true}
		tr.recordGlobalUsage()
		// THE INSTANT: still claimed, now also recorded. Counted once, not twice.
		b.capMu.Lock()
		_, during := b.GlobalLedger.UsageWithClaims(b.nowMs(), b.capInFlight, "")
		b.capMu.Unlock()
		if during != 0 {
			t.Errorf("uncounted in-flight = %d in the claimed-and-recorded instant, want 0 "+
				"(the ledger already counts it; counting the claim too would double-count)", during)
		}
		if u := l.Usage(recNow); u.Starts != 1 {
			t.Fatalf("usage.Starts = %d, want 1", u.Starts)
		}
		b.releaseGlobalStart(id)
		if u := l.Usage(recNow); u.Starts != 1 || b.inFlightStarts() != 0 {
			t.Errorf("after release: starts=%d claims=%d, want 1 and 0", u.Starts, b.inFlightStarts())
		}
	})

	t.Run("end to end: the entry lands and no claim leaks", func(t *testing.T) {
		st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
		b, l := ledgerBroker(t, st, &fakeGrant{spent: 1}, writesResult(`{"type":"result","subtype":"success"}`))
		b.GlobalMaxTasks = 100 // arm a limb so the claim machinery is live

		_, events, _ := submitTerminates(t, b,
			`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
		id := submittedID(t, events)

		onlyEntry(t, l, id)
		if n := b.inFlightStarts(); n != 0 {
			t.Errorf("%d claims leaked after the task finished, want 0", n)
		}
		if u := l.Usage(recNow); u.Starts != 1 {
			t.Errorf("usage.Starts = %d, want 1", u.Starts)
		}
	})
}

// TestGlobalRecord_ResumedTerminalUsesTheBrokerAuthoredRow: a task resumed at
// the diff gate after a restart has NO lease in this process — its agent ran in
// a previous brokerd life. Its broker-metered spend survives only in that
// process's own src:"broker" result row, and that (never audit.TotalCost, and
// never a row the agent wrote) is what the entry must carry.
//
// This is also the path that converges globalcap.go's documented residual: such
// a task holds no in-flight claim here, so its ledger entry is the only thing
// that makes it visible to the ceiling.
// THE ASSERTION IS INVERTED FROM WHAT IT WAS, AND THE INVERSION IS THE FIX.
//
// This test used to require the resumed entry to CARRY the previous process's
// src:"broker" figure and to mark it trusted. That read came from the task's own
// audit trace — a file the agent's stdout is copied into verbatim, in which
// `src` is a self-declared string — and recordGlobalUsage wrote it straight into
// the TRUSTED USD limb. Measured: a forged row claiming 999999 produced
// "global_budget_usd is exhausted: $999999.00 of $100.00" and refused every task
// start on the install, across all vendors and both auth modes, until it aged out
// — forever, in total mode.
//
// What kept it out in practice was ORDERING (the genuine broker row is normally
// appended after the agent exits), not authentication, and one swallowed write
// error on a task that then parks at the diff gate handed the limb to the agent.
//
// The rule is now the one boot reconciliation already follows: a value read from
// the agent-writable trace is NEVER trusted, so the ceiling does not read it. The
// START still counts exactly — that is the limb G7 names as the backstop.
func TestGlobalRecord_AResumedTerminalTrustsNoTraceContent(t *testing.T) {
	root := t.TempDir()
	l, err := OpenGlobalLedger(root, 24*time.Hour, recNow)
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{AuditRoot: root, GlobalLedger: l, GlobalBudgetUSD: 100}
	clk := &testClock{}
	clk.set(recNow)
	b.now = clk.now

	header := `{"type":"drydock_meta","subscription":false,"sensitive":false}` + "\n" +
		`{"type":"drydock_task","agent":"claude"}` + "\n"
	traces := map[string]string{
		// A genuine-looking trace: the agent's own cheap row, then a broker row.
		"a broker row that looks entirely genuine": header +
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":0.0002,"num_turns":1}` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":12.5,"num_turns":1,"src":"broker"}` + "\n",
		// ...and one an agent forged, which is BYTE-IDENTICAL in kind. Nothing
		// distinguishes them, which is the whole argument.
		"a forged inflating row": header +
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":999999,"num_turns":1,"src":"broker"}` + "\n",
		// The deflating direction is refused for the same reason.
		"a forged deflating row": header +
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":0,"num_turns":1,"src":"broker"}` + "\n",
		"no broker row at all": header +
			`{"type":"result","subtype":"interrupted","is_error":true,"duration_ms":0,"total_cost_usd":0,"num_turns":0}` + "\n",
	}
	ids := make([]string, 0, len(traces))
	for name, trace := range traces {
		id := newID()
		ids = append(ids, id)
		path := filepath.Join(root, id+".jsonl")
		if err := os.WriteFile(path, []byte(trace), 0o600); err != nil {
			t.Fatal(err)
		}
		tr := &taskRun{b: b, id: id, auditPath: path, resumed: true,
			agentName: "claude", taskVendor: "anthropic", outcome: "pushed"}
		tr.recordGlobalUsage()

		e := onlyEntry(t, l, id)
		if e.USDTrusted {
			t.Errorf("%s: usd_trusted = true; nothing read from an agent-writable trace may be trusted", name)
		}
		if e.USD != 0 {
			t.Errorf("%s: usd = %v, want 0; the resumed path must not source a figure from trace content", name, e.USD)
		}
	}

	u := l.Usage(recNow)
	if u.Starts != len(ids) {
		t.Errorf("usage.Starts = %d, want %d: every resumed terminal is a task start, which is the limb "+
			"that still bounds this case", u.Starts, len(ids))
	}
	if u.USD != 0 {
		t.Errorf("the TRUSTED usd limb reads $%.2f out of agent-writable files", u.USD)
	}
	// And the ceiling is not refused by the forgery either: an unauthenticatable
	// figure contributes nothing in EITHER direction.
	if u.USD >= b.GlobalBudgetUSD {
		t.Errorf("a forged trace exhausted the USD limb: $%.2f of $%.2f", u.USD, b.GlobalBudgetUSD)
	}
}

// TestGlobalRecord_NoLedgerIsAStrictNoOp: the stock install. With no store the
// terminal write must not create files, must not error, and must not slow the
// path down — the ceiling is off by default (G7) and off has to mean off.
func TestGlobalRecord_NoLedgerIsAStrictNoOp(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b := testBroker(t, "anthropic", st, &fakeGrant{spent: 1}, writesResult(`{"type":"result","subtype":"success"}`))
	if b.GlobalLedger != nil {
		t.Fatal("testBroker must not wire a ledger; this test asserts the default")
	}
	_, _, term := submitTerminates(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed", term["outcome"])
	}
	if _, err := os.Stat(GlobalLedgerPath(b.AuditRoot)); !os.IsNotExist(err) {
		t.Errorf("a ledger file was created with no ledger configured (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(b.AuditRoot, "global")); !os.IsNotExist(err) {
		t.Error("the ledger directory was created with no ledger configured")
	}
}

// TestGlobalRecord_MetricsRowCarriesABrokerAuthoredEndTime: boot reconciliation
// needs an absolute instant that is not the file's mtime (the flaw
// seedAggregateFromAudit still has). The metrics row is where it comes from, and
// it must come from the broker's clock seam like every other broker timestamp.
func TestGlobalRecord_MetricsRowCarriesABrokerAuthoredEndTime(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	b, _ := ledgerBroker(t, st, &fakeGrant{spent: 1}, writesResult(`{"type":"result","subtype":"success"}`))
	_, events, _ := submitTerminates(t, b,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	id := submittedID(t, events)

	m := lastMetricsLine(t, readAudit(t, b.AuditRoot, id))
	got, ok := m["ended_at_ms"].(float64)
	if !ok {
		t.Fatalf("metrics row has no ended_at_ms: %v", m)
	}
	if int64(got) != recNow {
		t.Errorf("ended_at_ms = %d, want the broker clock's %d (not a wall clock, not an mtime)", int64(got), recNow)
	}
}

// TestGlobalRecord_TrustBriefAndLedgerAgreeOnMetering pins the "same signal"
// requirement: writeBrief's BudgetUnbounded and the ledger's Metered are two
// renderings of one fact and must never disagree about a lane.
func TestGlobalRecord_TrustBriefAndLedgerAgreeOnMetering(t *testing.T) {
	for _, unmetered := range []bool{false, true} {
		t.Run(fmt.Sprintf("unmetered=%v", unmetered), func(t *testing.T) {
			st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
			b, l := ledgerBroker(t, st, &fakeGrant{spent: 2}, writesResult(`{"type":"result","subtype":"success"}`))
			if unmetered {
				b.UnmeteredVendors = map[string]bool{"anthropic": true}
			}
			_, events, _ := submitTerminates(t, b,
				`{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
			id := submittedID(t, events)

			brief, err := trustbrief.Read(b.AuditRoot, id)
			if err != nil {
				t.Fatalf("read brief: %v", err)
			}
			e := onlyEntry(t, l, id)
			if brief.Policy.BudgetUnbounded == e.Metered {
				t.Errorf("brief.BudgetUnbounded=%v but ledger.Metered=%v; the two must be exact opposites — "+
					"they render the same UnmeteredVendors fact", brief.Policy.BudgetUnbounded, e.Metered)
			}
		})
	}
}
