# Diff-Policy Caps + Second-Look Acknowledgment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Host-configured limits on an agent's diff, enforced broker-side. Max files / max lines / blocked-path globs make a task **fail closed** (`policy_blocked`) before the approval gate. Second-look path globs require the approver to **acknowledge each flagged category** before an approve succeeds — the anti-rubber-stamping mechanism.

**Architecture:** A new `diff_policy` config block (host-only). The broker checks caps right after `writeBrief` computes `DiffFacts`, before `setStage(StagePending)` — a violation writes a broker-authored `policy_blocked` result and terminates without ever reaching the gate. Second-look categories (flag kinds ∩ `second_look_paths` matches) are computed from the same `DiffFacts`, surfaced in the `awaiting_approval` event and `TaskState`, and enforced at approve: the approval channel carries ack tokens; approve without the full required set is refused (task stays pending). A new `internal/globmatch` provides `**`-aware path matching (none exists today; egress deliberately forbids wildcards).

**Tech Stack:** Go stdlib. Vanilla-JS SPA.

## Decision record (locked)

- `diff_policy` is **host-config only**; empty/absent = today's behavior (no caps, no acks). Fully backward compatible.
- Caps (`max_files_changed`, `max_lines_changed`, `blocked_paths`) → **fail closed before the gate**, outcome `policy_blocked` (a real broker `result` subtype, like `verify_failed`). `0`/empty = that cap disabled.
- `second_look_paths` → the approver must acknowledge each **flag category** that a matched/flagged file triggers. Missing acks → approve refused, task stays pending (fail-safe: never auto-approves).
- Required-ack categories are computed **broker-side** from `DiffFacts`; the client is told what to send but cannot fabricate the requirement. Acks are validated broker-side against the stored requirement.
- `auto_approve` still bypasses the human gate (documented), but **caps still apply** (a blocked-path or oversize diff is `policy_blocked` even under auto-approve — caps are enforcement, not a review aid). Second-look acks are a human-gate concept and do not apply to `auto_approve` (nobody to acknowledge).

## Global Constraints

- `policy_blocked` is written as a broker-authored `{"type":"result","subtype":"policy_blocked","src":"broker"}` row (mirror `internal/broker/verify.go`'s `verify_failed`), so `audit.OutcomeKey`'s `default: return r.Subtype` surfaces it in tasks/stats/ui with no refinement plumbing. Add it to `taskRun.outcome`'s documented set and `audit.Metrics.Outcome` doc.
- The approval-gate change is security-critical: missing/mismatched acks must **never** yield approved. Fail-safe direction is "stay pending". Add a red-team-style test that an approve lacking required acks does not push.
- Glob matching is bounded: patterns validated at config load (compile/shape check); matching is linear, no catastrophic backtracking; path inputs come from host-git `DiffFacts.Files[].Path` (trusted framing) but treat as data (cap count, no panic).
- No secret/credential in any new output. Ack categories are the fixed flag-kind vocabulary.
- Go gate before each commit: `go vet ./...`, `go test -race ./...` (scope per task), `gofmt -l internal/ cmd/` silent, `staticcheck ./...` clean. JS verified by `node --check` + reasoning (no JS harness).
- Commit `type(scope): summary`; trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`; no PR footer.

---

### Task 1: `internal/globmatch` — `**`-aware path matcher

**Files:** Create `internal/globmatch/globmatch.go` + `_test.go`.

**Interfaces:**
- `globmatch.Valid(pattern string) error` — rejects empty, and any pattern `path.Match` can't compile (`filepath.ErrBadPattern`); allows `**`.
- `globmatch.Match(pattern, name string) bool` — matches `name` (a forward-slash repo-relative path) against `pattern` supporting `*` (within a segment), `?`, `**` (spans path separators, including zero segments), and literal segments. Case-sensitive. No `..` interpretation (paths are already clean repo-relative).

**Semantics to test:** `**` matches any number of path segments (`**/*.yml` matches `a/b/c.yml` and `c.yml`); `.github/workflows/**` matches `.github/workflows/ci.yml` and `.github/workflows/x/y.yml` but not `.github/other`; `*.lock` matches `go.lock` not `a/go.lock`; `**` alone matches everything; a segment `*` (`src/*.go`) matches `src/x.go` not `src/a/x.go`.

- [ ] **Step 1: Failing test** — `globmatch_test.go`:

```go
package globmatch

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/nested/x.yml", true},
		{".github/workflows/**", ".github/workflowsx", false},
		{".github/workflows/**", ".github/other.yml", false},
		{"**/*.lock", "a/b/go.lock", true},
		{"**/*.lock", "go.lock", true},
		{"*.lock", "go.lock", true},
		{"*.lock", "a/go.lock", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/a/main.go", false},
		{"src/**/*.go", "src/a/b/main.go", true},
		{"**", "anything/at/all", true},
		{"Dockerfile", "Dockerfile", true},
		{"Dockerfile", "sub/Dockerfile", false},
		{"**/Dockerfile", "sub/Dockerfile", true},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.name); got != c.want {
			t.Errorf("Match(%q,%q)=%v want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	for _, ok := range []string{"**", "*.go", ".github/**", "src/**/*.ts", "a?b"} {
		if err := Valid(ok); err != nil {
			t.Errorf("Valid(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "[", "a[b"} {
		if err := Valid(bad); err == nil {
			t.Errorf("Valid(%q) = nil, want error", bad)
		}
	}
}

func TestMatch_HostileNoPanic(t *testing.T) {
	for _, p := range []string{"", "***", "**/**/**", "a/*/**/b"} {
		_ = Match(p, "a/b/c/d/e")
	}
}
```

- [ ] **Step 2:** fail. **Step 3:** implement. Approach: split both pattern and name on `/`. Recursive/iterative segment match where a `**` segment consumes zero-or-more name segments (try each split), a non-`**` segment must `path.Match(patSeg, nameSeg)` against the single name segment. `Valid` runs `path.Match` on each non-`**` segment against `""` to force a compile check (ignore the bool, catch `ErrBadPattern`) and rejects empty. Bound recursion by segment count (no unbounded blowup — memoize or iterate; `**/**` collapses).
- [ ] **Step 4:** `go test -race ./internal/globmatch/`; `staticcheck`. **Step 5:** commit `feat(globmatch): ** -aware repo-path glob matcher`.

---

### Task 2: config `diff_policy` block

**Files:** Modify `internal/config/config.go` (struct, Defaults n/a, validate, SeedTemplate), `internal/config/explain.go` (provenance), `config/config.yaml` (seed mirror); tests in `config_test.go` + `explain_test.go`.

**Interfaces:**
- `type DiffPolicy struct { MaxFilesChanged int \`yaml:"max_files_changed"\`; MaxLinesChanged int \`yaml:"max_lines_changed"\`; BlockedPaths []string \`yaml:"blocked_paths"\`; SecondLookPaths []string \`yaml:"second_look_paths"\` }`
- `Config.DiffPolicy DiffPolicy \`yaml:"diff_policy"\`` (after the `Verify` block). Zero value = disabled.
- Validation: `MaxFilesChanged >= 0`, `MaxLinesChanged >= 0`; every `BlockedPaths`/`SecondLookPaths` pattern passes `globmatch.Valid` (else `config: diff_policy.blocked_paths[i] invalid glob: …`).
- No env override.
- Explain provenance: register `DiffPolicy` in `provenanceTable()` — a yaml-only field (no env), value renderer summarizing `Nf/Nl caps, Nb blocked, Ns second-look` (compact one line), differs = any non-zero field. Add to `PolicyComparisonFields` (it IS enforced policy, keep it in the divergence set).

- [ ] **Step 1: Failing tests** — `config_test.go`:

```go
func TestDiffPolicy_LoadsAndValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	yaml := "network: x\ngateway_ip: 1.2.3.4\n" +
		"diff_policy:\n  max_files_changed: 50\n  max_lines_changed: 2000\n" +
		"  blocked_paths: [\"**/*.pem\", \".github/workflows/**\"]\n" +
		"  second_look_paths: [\"**/Dockerfile\"]\n"
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil { t.Fatal(err) }
	c, err := Load(path)
	if err != nil { t.Fatalf("Load: %v", err) }
	dp := c.DiffPolicy
	if dp.MaxFilesChanged != 50 || dp.MaxLinesChanged != 2000 ||
		len(dp.BlockedPaths) != 2 || len(dp.SecondLookPaths) != 1 {
		t.Errorf("diff_policy = %+v", dp)
	}
}

func TestDiffPolicy_Rejects(t *testing.T) {
	base := "network: x\ngateway_ip: 1.2.3.4\ndiff_policy:\n"
	cases := map[string]string{
		base + "  max_files_changed: -1\n":            "diff_policy",
		base + "  blocked_paths: [\"a[b\"]\n":          "diff_policy",
	}
	for yaml, want := range cases {
		path := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(path, []byte(yaml), 0o600)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) err=%v want %q", yaml, err, want)
		}
	}
}
```

Add an `explain_test.go` assertion that `DiffPolicy` appears as a yaml-only Field (source config.yaml when set, default when absent).

- [ ] **Step 2:** fail. **Step 3:** implement (mirror the `Verify` block's structure at config.go:151-153, validation loop at config.go:482-497, seed block at config.go:562-572, and the `renderVerifyRepos`/differs pattern in explain.go:148-159,235-240). Import `drydock/internal/globmatch`. Add the commented `diff_policy:` block to BOTH `SeedTemplate` and `config/config.yaml` identically (pinned by `TestSeedTemplate_MatchesOnDiskTemplate`).
- [ ] **Step 4:** `go test -race ./internal/config/ ./cmd/docs-build/`. **Step 5:** commit `feat(config): host-side diff_policy block (caps + blocked/second-look globs)`.

---

### Task 3: broker caps enforcement → `policy_blocked`

**Files:** Modify `internal/broker/broker.go` (Broker field, `writeBrief` returns facts, pre-gate check, taskRun.outcome doc), `internal/broker/verify.go` (n/a), `internal/audit/audit.go` (Metrics.Outcome + outcomeString doc), `cmd/brokerd/main.go` (wire `Broker.DiffPolicy`); tests in `internal/broker/`.

**Interfaces:**
- `Broker.DiffPolicy config.DiffPolicy` (broker imports config already). Wired from `cfg.DiffPolicy` in brokerd's Broker literal.
- `writeBrief` returns the `trustbrief.DiffFacts` it computed (change signature `func (b *Broker) writeBrief(tr *taskRun, diff string) trustbrief.DiffFacts`) so the caps check and second-look (Task 4) reuse it — no second `Analyze`.
- `(tr *taskRun) checkDiffCaps(facts trustbrief.DiffFacts) (blocked bool, reason string)` in a new `internal/broker/diffpolicy.go`: returns blocked=true with a human reason when `len(Files)+FilesOmitted > MaxFilesChanged` (when >0), or total adds+dels over `MaxLinesChanged` (when >0), or any `Files[].Path` matches any `BlockedPaths` glob. Reason names the specific cap/path.

**Behavior in `pushAndOpenPR`:** after `facts := b.writeBrief(tr, diff)` and before `setStage(StagePending)`:

```go
if blocked, reason := tr.checkDiffCaps(facts); blocked {
    tr.outcome = "policy_blocked"
    tr.appendBrokerResult(true) // synthetic result row, subtype policy_blocked (add subtype param or a variant mirroring verify.go)
    tr.sw.emit(map[string]any{"event":"result","outcome":"policy_blocked","task_id":tr.id,
        "reason": reason, "hint":"drydock inspect "+tr.id+" — a diff-policy cap blocked this task before review"})
    return
}
```

(Match the EXACT synthetic-result idiom verify.go uses for `verify_failed` — read it; likely a `fmt.Fprintf(tr.logf, {"type":"result","subtype":"policy_blocked",...,"src":"broker"})`. Ensure the metrics row's `tr.outcome` carries `policy_blocked`.) Caps apply even when `tr.autoApprove` (check runs before the auto-approve branch).

- [ ] **Step 1: Failing tests** in `internal/broker/diffpolicy_test.go` using the handle_task_test harness:
  - `TestDiffCaps_MaxFilesBlocksBeforeGate` — Broker.DiffPolicy.MaxFilesChanged=1, fakeStage.diff with 2 files → terminal outcome `policy_blocked`, `fakeStage.pushed==false`, task never in `b.pending` (assert via a short poll that pending stays empty), a broker result row with subtype policy_blocked in the audit.
  - `TestDiffCaps_MaxLinesBlocks` — small file count but adds+dels over the line cap → policy_blocked.
  - `TestDiffCaps_BlockedPathBlocks` — a diff touching `.github/workflows/ci.yml` with BlockedPaths `[".github/workflows/**"]` → policy_blocked, reason names the path.
  - `TestDiffCaps_UnderCapsProceedsToGate` — diff within caps → reaches awaiting_approval (gate) as today.
  - `TestDiffCaps_AutoApproveStillBlocked` — auto_approve + a blocked path → policy_blocked, never pushed (caps are enforcement).
  - `TestDiffCaps_DisabledByDefault` — zero DiffPolicy → any diff proceeds (no regression).
- [ ] **Step 2:** fail. **Step 3:** implement `diffpolicy.go` + wire. **Step 4:** `go test -race ./internal/broker/ ./internal/audit/ ./cmd/brokerd/`. **Step 5:** commit `feat(broker): enforce diff-policy caps — fail closed as policy_blocked before the gate`.

---

### Task 4: second-look acknowledgment at the gate

**Files:** Modify `internal/broker/broker.go` (channel type, taskRun required-acks, gate event), `internal/broker/gates.go` (awaitGate payload), `internal/broker/admin.go` (signal parses ack body + validates), `internal/broker/diffpolicy.go` (`requiredAcks(facts, cfg) []string`); tests.

**Design (careful — this changes the approval primitive):**
- New type `type gateReply struct { ok bool; acks []string }`. Change `b.pending map[string]chan bool` → `map[string]chan gateReply`; update `awaitGate` (return acks too or validate inside), `gates.go` select, and `admin.go signal`. The egress gate passes `{ok, nil}` (ignores acks).
- `requiredAcks(facts, cfg)` returns the sorted set of flag-kind categories that are "second-look": a category is required when a `Files[].Path` matches a `SecondLookPaths` glob OR (design choice) when a flag of a high-risk kind is present whose paths match. **Simplest correct rule:** for each `Flag` in `facts.Flags`, if ANY of its `Paths` matches ANY `SecondLookPaths` glob, the flag's `Kind` is a required ack. (So `second_look_paths` scopes which flagged files demand acknowledgment.) Empty `SecondLookPaths` → no required acks (feature off).
- Compute `tr.requiredAcks` at gate entry (in `pushAndOpenPR`, after the caps check, from `facts`), store on `taskRun`, and include in the `awaiting_approval` event: `"second_look": tr.requiredAcks` (array of category strings). Also surface in `TaskState` (add `SecondLook []string` json `second_look,omitempty`) so `HandlePending`/`HandleTasks` expose it.
- `signal` (admin.go): read an optional JSON body `{"acknowledge":["ci-workflow",...]}` (cap body size, tolerate empty for deny and for tasks with no required acks). Pass acks into the channel.
- Gate validation: in `gatePushMarked` (or right where awaitGate returns approved), if `ok` and `len(tr.requiredAcks)>0`, require that the received acks **superset** the required set; if not, respond to the approver with 422 and **keep the gate open** (do not consume the pending entry / do not push). Model: `signal` returns 422 when acks are insufficient and the task stays pending for a retry. (Confirm the channel/await mechanics allow a rejected approve to re-arm — simplest: validate in `signal` BEFORE sending on the channel, so an insufficient approve never signals the gate and returns 422 to the client; the task stays pending.)
- Deny path unaffected (no acks needed).

- [ ] **Step 1: Failing tests** (`internal/broker/secondlook_test.go`, harness + a helper `approveWithAcks(t,b,id,acks)`):
  - `TestSecondLook_RequiredAcksSurfacedInGateEvent` — SecondLookPaths `[".github/workflows/**"]`, diff touches a workflow (flag ci-workflow) → the awaiting_approval event carries `second_look:["ci-workflow"]`.
  - `TestSecondLook_ApproveWithoutAcksRefusedStaysPending` — approve with no acks → 422 (or non-204), `fakeStage.pushed==false`, task still in `b.pending`; a subsequent approve WITH the ack pushes. (The red-team-style fail-safe test.)
  - `TestSecondLook_ApproveWithAcksPushes` — approve with `["ci-workflow"]` → pushed.
  - `TestSecondLook_NoConfigNoAcksNeeded` — empty SecondLookPaths → approve with empty body pushes (no regression; existing gated-approve tests must stay green — update them to send the new empty body if the signature changed).
  - `TestSecondLook_DenyIgnoresAcks` — deny works with no body.
  - Egress gate regression: `TestGateEgressWiden_*` still pass with the channel-type change.
- [ ] **Step 2:** fail. **Step 3:** implement. Update `gatecause_test.go` (`TestAwaitGate_ApproveDeny` uses `chan bool` directly — migrate to `gateReply`). **Step 4:** `go test -race ./internal/broker/`. **Step 5:** commit `feat(broker): second-look acknowledgment — approve requires acking flagged categories`.

---

### Task 5: CLI + Web UI ack UX, policy_blocked rendering, docs

**Files:** `cmd/drydock/client.go` (signal sends acks; approve flag), `cmd/drydock/main.go` (approve `--acknowledge`), `cmd/drydock/review.go` (interactive ack prompt), `cmd/drydock/inspect.go`/`stats.go`/`tasks.go` (policy_blocked + second-look rendering), `internal/webui/server.go` (proxy forwards ack body), `internal/webui/assets/app.js` (per-category ack checkboxes gating the approve button) + `style.css`; docs `site/docs/submitting-tasks.md` + `configuration.md`; `CHANGELOG.md`.

**Behavior:**
- `drydock approve <id> [--acknowledge cat]...` — repeatable; marshals `{"acknowledge":[...]}` into the POST body. Without required acks the server returns 422; the CLI prints the missing categories and the `--acknowledge` hint. `drydock review <id>` reads the brief's flags + the pending task's `second_look` list, and for each required category interactively prompts "acknowledge <category> change? [y/N]" before approving (all must be y).
- `drydock pending` shows a `SECOND-LOOK` marker when a task has required acks.
- `inspect`/`tasks`/`stats`: render `policy_blocked` (friendly "policy blocked") — add to `outcomeString` and the stats `fixed` list.
- Web UI: `renderBrief` flag rows gain a checkbox per required second-look category; the overlay Approve button is disabled until all are checked; `act("approve", id, acks)` sends the body; `proxy`/`signalHandler` forward it. A 422 response surfaces a toast naming missing categories.
- Docs: document `diff_policy` (caps → policy_blocked, second_look → acknowledgment), the auto_approve interaction (caps apply, acks don't), and the fail-safe (missing acks never approve).

- [ ] **Step 1: Failing tests** — CLI: `signal` with acks posts the body (extend the client HTTP-contract test); a 422 path prints missing cats. Web: `proxy_test.go` asserts the ack body forwards to `/admin/approve`. Render tests for `policy_blocked` in inspect/status.
- [ ] **Step 2:** fail. **Step 3:** implement; `node --check app.js`; manual note that the checkbox gating works (vm-stub render like the brief-panel task). Add `policy_blocked` to `cmd/docs-build/claims.go` only if a new default/const warrants (it doesn't — skip). Check the docs-drift sentinel.
- [ ] **Step 4:** FULL gate: `go vet ./...`; `go test -race -count=1 ./...`; `gofmt -l internal/ cmd/` silent; `staticcheck ./...` clean; `go test ./cmd/docs-build/`; `node --check internal/webui/assets/app.js`. **Step 5:** commit `feat(cli,webui,docs): second-look ack UX + policy_blocked rendering`.

---

## Final verification (whole branch)

- Full gate green (vet/race/gofmt/staticcheck/docs-build/node --check).
- Grep gates: `policy_blocked` written as a broker `src:"broker"` result subtype (not metrics-only); the approval path NEVER returns approved without the required ack superset (trace `signal`→gate); caps apply under `auto_approve`; every new config glob validated at load.
- Red-team: a blocked-path diff cannot reach the gate; an approve missing required acks cannot push.
