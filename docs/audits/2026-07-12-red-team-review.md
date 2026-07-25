# Red-team security and product review

**Target:** drydock v0.6.1 (`7cac3a9efda098e5e9129486a3a071a6ce3098b8`)  
**Review date:** 2026-07-12  
**Perspective:** hostile staged repository, hostile/compromised in-VM agent and agent CLI, malicious web origin, operator error, and unattended-operation failure modes.

## Remediation verification pass, 2026-07-13

**Verification target:** `d655c70d166a` on `main` (post-v0.6.2; changes `cc0ac77` through `d655c70`)  
**Baseline audited:** v0.6.1, `7cac3a9efda0`  
**Method:** source/diff review, adversarial path analysis, full race/static/vulnerability checks, no-cache image build, host red-team suite, and Apple `container` VM-backed A1/A2/A7 suite.

### Verification outcome

The remediation set is **not fully closed**. Of the original ten findings, **2 are verified fixed, 7 are partially remediated, and 1 is not technically fixed (documented/accepted instead)**. The pass also found **two new gaps introduced or exposed by the remediation**, including a High-severity approval-integrity regression.

| ID | Original severity | Verification status | Result |
|---|---:|---|---|
| F-01 | Critical | **Verified fixed** | `.task` and final-component symlinks are rejected; the hostile-repository regressions pass. |
| F-02 | Critical | **Not fixed / documented risk** | Shipped defaults still admit unlimited concurrent API-key requests with no reservation. Documentation now calls the budget soft, but top-line “budget-capped token” claims remain. |
| F-03 | High | **Partial** | Representative control-plane routes are blocked, but the Anthropic rule still admits the unmetered Message Batches API. |
| F-04 | High | **Partial; new regression V-01** | Output and buffer caps landed. Disk protection is polling-based, and the review diff may be truncated while the full unreviewed change is committed. |
| F-05 | High | **Partial** | Individual request/usage buffers are capped, but per-lease concurrency, slow-body, and aggregate-memory exhaustion remain. |
| F-06 | High | **Verified fixed** | TCP mode rejects non-literal-loopback Host and hostile Origin values; rebinding regressions pass. |
| F-07 | Medium | **Partial** | Successful runs and push failures get broker-authored costs; task-run error/cancel paths still lose metered spend across restart. |
| F-08 | Medium | **Partial** | Unknown fields fail closed, but both YAML loaders silently ignore trailing YAML documents. |
| F-09 | Medium | **Partial** | Go checksum and Action SHA pinning landed; apt and npm dependency graphs remain mutable, and the pinning claim remains false. |
| F-10 | Low | **Partial** | Several stale statements and a regression sentinel were added; misleading budget/pinning claims and duplicated sources of truth remain. |

**Verification release recommendation:** do not close the remediation milestone as “all findings addressed.” Block an unattended/security-claim release at least on V-01 and the F-03 batch bypass. Treat F-02 as an explicit risk acceptance only if the product stops calling the default token budget-capped and secure defaults are not part of the intended guarantee.

## Closure addendum, 2026-07-25

All ten findings and both verification-pass gaps are now closed or explicitly
risk-accepted:

- **v0.6.3** closed V-01, F-02, F-03, F-05, F-07, F-08, and V-02 (see the
  CHANGELOG entry for the per-fix regression tests). F-01 and F-06 were
  verified fixed in the round-2 pass above.
- **F-04 closed:** each task's stage dir is now a size-capped APFS sparse
  image (`stage_quota_gb`, default 8 GiB), a filesystem hard wall behind
  the existing polling early-cancel guard. VM-verified by
  `TestRedteam_A8_WorkQuotaHardBound`.
- **F-09 closed with stated residuals:** apt resolves against a dated
  snapshot.debian.org archive and npm resolution is cut at a pinned
  `--before` date, both bumped deliberately. Accepted residuals: registry
  and mirror trust (snapshots give reproducibility, not provenance), and
  the claude-code/opencode install scripts that their packaging requires.
- **F-10 closed:** the security-defaults table is now generated from
  `config.Defaults()` and exported constants
  (`site/docs/security-defaults.md`), with a drift test and a
  verified-by-test-exists test alongside the forbidden-phrase guard.

The report below is retained for audit history. Where its wording and this
addendum disagree, the addendum and the generated security-defaults table
are current.

### V-01: High: Oversize diff truncation bypasses the human approval boundary

The F-04 memory fix caps the captured diff at 32 MiB, appends a truncation marker, and explicitly continues with the full staged work tree (`internal/stage/stage.go:109-170`). The approval gate receives only that truncated string (`internal/broker/broker.go:640-672`), while `pushWithRecovery` commits every staged change.

A hostile agent can create an alphabetically early padding diff larger than 32 MiB and place a security-sensitive change in a later path. `drydock review` and the persisted `.diff` omit the later change, but approval commits and pushes it. The marker warns that truncation occurred, which reduces surprise, but a containment product must not make “approve” authorize bytes it did not make reviewable. Auto-approve is affected as well.

**Required fix:** fail closed when the review representation exceeds the cap, or persist the complete diff to a bounded/quota-backed artifact and require the exact committed tree/diff digest to match the reviewed artifact. Add an end-to-end test with a malicious change beyond the cap and assert that no push is possible.

### V-02: Medium: The pre-release VM gate is conventional, not enforced

`make tag-release` runs `release-preflight`, which is a useful blessed workflow (`Makefile:114-137`). However, `.github/workflows/release.yml:14-40` publishes on any pushed `v*` tag and on manual workflow dispatch without checking an attestation, status, or artifact proving that the preflight ran for the tagged commit and freshly built image. A maintainer, automation token, or compromised workstation can still run `git tag`/`git push` directly and bypass every VM test.

**Required fix:** bind release publication to a verifiable preflight result for the exact commit and image digest. Prefer a protected tag/ruleset plus a required Apple-silicon runner job, or require and verify a signed preflight attestation before artifact publication. Keep `tag-release` as convenience, but do not describe it as enforcement by itself.

### Residual details by original finding

#### F-01: Verified fixed

`WriteTaskFiles` now checks `.task` with `Lstat`, rejects a symlinked directory, and opens `prompt.txt` with `O_NOFOLLOW`. `internal/stage/symlink_test.go` exercises both a committed `.task` symlink and a committed `.task/prompt.txt` symlink through real Git staging. The race and host red-team suites pass. The original pre-VM overwrite primitive is closed for the documented single-user threat model.

#### F-02: Not fixed; disclosure improved

The runtime defaults remain `task_max_requests: 0` and `max_request_cost_usd: 0` in API-key mode (`config/config.yaml:14-24`). Admission therefore still checks completed `SpentUSD` and admits every synchronized in-flight request before any response is metered (`internal/gateway/gateway.go:151-187`). The optional reservation works when an operator configures it, but it is disabled in the shipped posture and is not applied to the aggregate ledger as an in-flight reservation.

The threat model and configuration docs now describe concurrent overshoot accurately. That is documentation/risk acceptance, not remediation of the Critical technical condition. `README.md:28`, `site/docs/quickstart.md:24`, `THREAT_MODEL.md:112`, and the generated site still call the token “budget-capped,” which is materially stronger than the default implementation.

#### F-03: Partial; Anthropic batch authority remains

Per-vendor method/path rules now block representative Files, fine-tuning, admin, and sibling-prefix routes before credential injection. The matcher, however, defines every non-trailing prefix as both an exact endpoint and a subtree (`internal/gateway/vendor.go:59-68`). Anthropic allows `POST /v1/messages`, so the code and its own test deliberately accept `POST /v1/messages/batches` (`internal/gateway/route_tighten_test.go:21-23`).

Anthropic documents that endpoint as creating a batch containing as many as 100,000 Message requests: [Create a Message Batch](https://platform.claude.com/docs/en/api/messages/batches/create). Its create response is batch metadata, not normal per-message usage, so the gateway's response-usage meter does not provide the intended spend control. Cancellation and other POST subresources also inherit authority.

Make inference routes exact by default and make subtree matching an explicit route attribute. Add a regression requiring `POST /v1/messages/batches` to return a gateway-local 403 without contacting upstream.

#### F-04: Partial; resource bounds are soft

The 256 MiB shared task-output cap and bounded diff allocation materially reduce host OOM/disk risk. The `/work` control is explicitly a soft two-second poll (`internal/broker/stagesize.go:12-26`, `internal/broker/stagesize.go:80-105`), so worst-case disk overshoot is write rate times the poll interval. A hostile VM can consume the two-GiB free-space reserve or create a pathological tree before cancellation, and walk errors are ignored. This is useful mitigation, not a filesystem quota. V-01 makes the current diff behavior unsafe even though its memory use is bounded.

Use a quota-limited per-task filesystem or disk image for a hard byte/inode boundary. Fail closed on oversize diffs rather than approving a prefix.

#### F-05: Partial; aggregate exhaustion remains

`http.MaxBytesReader` caps each body at 16 MiB and the usage reader caps its retained buffers at 1 MiB. Those fixes close the single-buffer cases. There is still no per-lease in-flight request/connection limit or body-progress deadline. Subscription mode can admit its built-in maximum of 1,000 concurrent bodies; request rewriting may buffer roughly 16 GiB before JSON-decoding overhead. API-key mode defaults to unlimited requests, and many simultaneous response meters can each retain their cap.

Add a small per-lease concurrency semaphore (one is the secure default), global connection/memory admission, and a streaming-compatible minimum body rate/deadline. Oversize rewrites should return an explicit 413 rather than restoring and forwarding the partial body.

#### F-06: Verified fixed

The TCP broker wraps the handler with literal-loopback Host and loopback/no-Origin validation. Tests cover rebinding hostnames, alternative numeric IP encodings, trailing dots, userinfo, IPv4/IPv6 loopback, and hostile Origin values. Unix-socket mode remains the default. Native same-user process access is unchanged and correctly remains outside the browser-rebinding fix.

#### F-07: Partial; failure-path spend is not restart durable

Successful agent runs now end with a last-wins `src:"broker"` result using `grant.Spent()`, and restart seeding trusts only those broker records. Push failures and resume terminal outcomes also carry broker-authored cost.

In contrast, output-cap, stage-cap, generic task failure, and cancellation paths return before the success record and write either no terminal audit result or an untagged result with `total_cost_usd:0` (`internal/broker/broker.go:567-620`). `seedAggregateFromAudit` ignores any last result without positive cost and `src:"broker"` (`cmd/brokerd/main.go:729-767`). A task can spend through the gateway, exit non-zero, and have that spend disappear from the rolling aggregate ledger after broker restart.

Append one broker-authored terminal record using `grant.Spent()` in a single deferred finalizer for every exit path, and make the gateway ledger itself durable so a broker crash between metering and finalization cannot erase spend.

#### F-08: Partial; trailing documents remain fail-open

Both loaders use `KnownFields(true)`, and typo regressions pass. Each loader calls `Decode` exactly once and never requires a second decode to return `io.EOF` (`internal/config/config.go:244-263`, `internal/egress/egress.go:111-128`). A valid first YAML document followed by `---` and a second security configuration is silently accepted while the second document is ignored.

After decoding the intended document, decode once more and reject anything other than `io.EOF`. Add main and egress regressions with a security-relevant field in document two.

#### F-09: Partial; empirically non-reproducible

The Go archive now has architecture-specific SHA-256 verification, GitHub Actions are pinned to full commits, and Codex/Gemini installs use `--ignore-scripts` with CLI smoke checks. A no-cache image build executed those controls successfully, including `/tmp/go.tar.gz: OK` and the expected CLI versions.

That same no-cache build fetched current Debian indexes and upgraded packages relative to the cached image, directly demonstrating that identical source does not produce a fixed dependency set. `apt-get update && apt-get upgrade` remains mutable; global npm installs still resolve floating transitives without lockfiles, and Claude/opencode execute root install scripts because their packaging requires them. `docs/ROADMAP.md:107` still says every external input is pinned. Amend the claim and lock/snapshot the remaining dependency graphs.

#### F-10: Partial; high-risk claims still drift

The named stale statements were corrected, and `TestSecurityClaimsNoDrift` prevents a short blacklist of phrases from returning. The test itself says the generated single source of truth remains follow-up (`cmd/docs-build/claims_test.go:10-16`). Current README, quickstart, threat model, and generated landing page still describe the default bearer as budget-capped despite F-02. The roadmap's universal pinning statement still contradicts F-09. Generate the capability/default table and test affirmative claims against configuration/code, not only forbidden historical phrases.

### Verification commands and results

- `go test -race -count=1 ./...`: **PASS**.
- `go vet ./...`: **PASS**.
- `make lint` (`staticcheck v0.7.0`): **PASS**.
- `go run golang.org/x/vuln/cmd/govulncheck@v1.4.0 ./...`: **PASS**, no reachable Go vulnerabilities reported.
- `make redteam-report`: **GREEN** for all selected host claims. The first restricted run could not bind an `httptest` loopback listener; the permitted rerun passed, confirming an environment restriction rather than a product failure.
- `make image` followed by `container build --no-cache -t drydock-sandbox:verify image/`: **PASS**; checksum and CLI smoke steps executed successfully.
- `make redteam-vm` against freshly rebuilt images: **PASS** for VM-backed A1/A2/A7 integration tests.
- Live Squid tests and real-vendor paid calls were not run: Squid is not installed locally, and verification did not justify spending operator API funds. Current vendor route semantics were checked against Anthropic's primary API reference.

The original v0.6.1 report follows unchanged for audit history; its old verification section describes the earlier environment and is superseded by this addendum.

## Executive summary

The project has a stronger-than-average written threat model and meaningful tests around VM egress, privilege dropping, hook neutralization, approval gates, socket permissions, and cleanup. Those controls are real. They do not, however, support the current top-line claim that a hostile agent cannot touch the host filesystem or exercise the host credential beyond a budget-capped model call.

I found **2 critical, 4 high, 3 medium, and 1 low/product finding**. The most urgent issue happens before the VM starts: a repository-controlled `.task/prompt.txt` symlink is followed by the trusted broker, allowing a hostile repository to overwrite an arbitrary operator-writable host file. The second critical issue is a financial-control bypass: a valid task bearer can make many parallel gateway requests while `SpentUSD` is still zero; with the shipped defaults, all are admitted, so the documented “one request” overshoot bound is false for the explicitly hostile agent in the threat model.

The credential gateway is also an unrestricted reverse proxy to the vendor API. It scopes time and nominal budget, but not method or route. A task can therefore use the host key for file upload/download/deletion and other API control-plane operations that are neither required for coding inference nor reliably metered. Host availability is insufficiently bounded: the writable bind mount, agent stdout/audit stream, diff capture, request rewriting, and response metering all permit disk exhaustion or host-memory exhaustion.

**Release recommendation:** do not market v0.6.1 as containing a hostile repository/agent until F-01 and F-02 are fixed. Treat F-03 through F-06 as pre-1.0 blockers for unattended use.

## Severity summary

| ID | Severity | Finding | Primary impact |
|---|---:|---|---|
| F-01 | Critical | Pre-VM symlink traversal overwrites host files | Host integrity / possible code execution |
| F-02 | Critical | Parallel requests bypass per-task and aggregate spend bounds | Unbounded financial loss / rate-limit exhaustion |
| F-03 | High | Gateway bearer delegates the whole vendor API, not inference routes | Data exfiltration, destructive actions, unmetered spend |
| F-04 | High | No task disk, output, file-count, or diff-size bounds | Host disk exhaustion and broker OOM |
| F-05 | High | Gateway has unbounded request and response buffering | Host OOM / goroutine and connection exhaustion |
| F-06 | High | Optional loopback TCP broker is vulnerable to DNS rebinding | Remote submit/approve/kill through a browser |
| F-07 | Medium | Untrusted agent output can forge authoritative audit/result records | Audit integrity and aggregate-cap persistence |
| F-08 | Medium | Unknown YAML fields are silently ignored | Security controls disabled by typo |
| F-09 | Medium | “Every external input is pinned” is not true for sandbox builds | Non-reproducible, mutable supply chain |
| F-10 | Low | Operator-facing documentation has security-relevant drift | Unsafe configuration decisions / credibility loss |

## Detailed findings

### F-01: Critical: Pre-VM symlink traversal overwrites arbitrary host files

**Evidence**

- The staged repository is explicitly untrusted (`THREAT_MODEL.md:78-80`).
- `stage.Prepare` clones it on the host, preserving Git symlinks (`internal/stage/stage.go:29-47`).
- Before the VM starts, `WriteTaskFiles` calls `os.MkdirAll(<work>/.task)` and then `os.WriteFile(<work>/.task/prompt.txt, ...)` without rejecting symlinks or using `O_NOFOLLOW` (`internal/stage/stage.go:63-74`).
- The README claims a hostile agent/repository “can't reach ... your filesystem” and can write only to a disposable copy (`README.md:20-24`, `README.md:35-40`).

**Attack**

A hostile repository commits a real `.task/` directory whose `prompt.txt` is a symlink to an absolute operator-writable path, for example `~/.ssh/config`, `~/.zshrc`, `~/.drydock/config.yaml`, or a project automation file. On task submission, the trusted host process follows the final symlink and truncates the target with the task instruction before any VM boundary exists. A symlink at `.task` itself can similarly redirect creation of `prompt.txt` into another host directory.

The attacker may not always control the exact instruction bytes, but arbitrary truncation/overwrite is already a host-integrity violation and denial of service. A repository can socially prescribe an instruction whose contents form a useful shell/config payload, turning the primitive into code execution on the operator's next shell/tool launch.

**Remediation**

1. Never place broker control files under the untrusted work tree. Put the prompt in a separate broker-created directory mounted read-only at a dedicated path such as `/run/drydock/prompt.txt`.
2. If a work-tree path must remain, remove/recreate `.task` only after proving every component is beneath the stage root and is not a symlink; open the final file with `O_NOFOLLOW|O_CREAT|O_EXCL` using directory-relative APIs. On macOS, prefer an `openat`-style walk with no-follow semantics for every component.
3. Add regressions for a symlinked `.task`, a symlinked `.task/prompt.txt`, relative `..` targets, absolute targets, dangling targets, and replacement races.
4. Add this case to the host-side red-team suite. The current A3 test covers Git hooks, not general hostile-filesystem behavior.

### F-02: Critical: Parallel requests bypass the spend bounds

**Evidence**

- Admission checks only completed `SpentUSD`; it then increments a request counter and admits the request (`internal/gateway/gateway.go:156-187`).
- Spend is added only after a response body completes (`internal/gateway/gateway.go:309-337`).
- In-flight reservation exists, but the shipped default is `max_request_cost_usd: 0`, which disables it; API-key mode also defaults `task_max_requests: 0` (unlimited) (`config/config.yaml:14-24`).
- The threat model says overshoot is bounded by one call because “the agent calls the API sequentially” (`THREAT_MODEL.md:229-234`). That assumption contradicts the same document's requirement to treat every agent tool call and HTTP request as adversarial (`THREAT_MODEL.md:12-13`, `THREAT_MODEL.md:31-44`).
- The design archive already recognizes that without a reservation the effective bound grows with in-flight request count (`docs/superpowers/specs/2026-07-11-precise-gateway-metering-design.md:7-12`), but the mitigation shipped off by default.

**Attack**

The agent reads its own task bearer from the environment and launches many concurrent `curl` requests directly to gateway port 8088. Every request arrives before the first response has been metered, sees `SpentUSD == 0`, and is admitted. A $2 task can therefore create an overshoot of roughly `number_of_in_flight_requests × request_cost`, not one request. The aggregate ledger has the same completed-spend race, so multiple tasks or leases compound the loss.

**Remediation**

1. Make a per-lease in-flight limit of one the secure default; normal agent CLIs are sequential, so this should have little product cost.
2. Make reservations mandatory for any USD-bounded backend. Derive a conservative reservation from the requested model and maximum output tokens, and reject requests that omit or exceed a safe maximum.
3. Apply reservations to the aggregate ledger as well as the per-task lease.
4. Set a finite default `task_max_requests` in API-key mode as independent defense in depth.
5. Add an adversarial test that synchronizes N requests before releasing any upstream response and asserts that at most the reserved budget is admitted. Test both one lease and multiple leases under the aggregate cap.
6. Until fixed, stop calling `task_budget_usd` a hard ceiling and prominently require a non-zero `max_request_cost_usd`.

### F-03: High: The gateway token delegates the entire upstream API

**Evidence**

- After bearer validation, `ServeHTTP` forwards the request without a route or method policy (`internal/gateway/gateway.go:190-218`).
- The director replaces only scheme/host, preserves the attacker-selected path and query (or joins it under a configured base path), and injects the real host credential (`internal/gateway/gateway.go:274-292`; `internal/gateway/vendor.go:45-133`).
- Metering charges only responses from which a recognized token-usage object can be parsed (`internal/gateway/gateway.go:319-335`). Control-plane endpoints normally have no such usage object.
- OpenAI's API exposes authenticated file upload/list/download/delete operations and fine-tuning job creation on the same `api.openai.com` origin used by the gateway: [Files API](https://platform.openai.com/docs/api-reference/files/object?lang=curl), [Fine-tuning API](https://platform.openai.com/docs/api-reference/fine-tuning/resume?lang=python).

**Attack / impact**

A task can call `/v1/files`, `/v1/files/{id}/content`, `DELETE /v1/files/{id}`, `/v1/uploads`, `/v1/fine_tuning/jobs`, or any future same-origin endpoint its host key is authorized to use. This can persistently upload repository data, enumerate or download existing project data, delete objects, and initiate work whose cost is not represented by response token usage. The task never learns the real key, but it exercises much of the key's authority. The “short-lived, budget-capped token” is therefore time-scoped but not capability-scoped or reliably budget-scoped.

This is distinct from normal inference sending selected context to the configured model provider: it enables unrelated control-plane operations and persistent storage outside the reviewed diff.

**Remediation**

1. Define an allowlist per provider and auth mode: exact HTTP methods and path grammars required by the pinned CLI (for example, only the messages/responses endpoints).
2. Reject file upload, batch, fine-tuning, vector-store, model-management, organization/admin, and unknown future routes by default.
3. Strip organization/project/account-selection headers unless explicitly configured host-side.
4. Prefer a vendor key restricted to one project and only required endpoint permissions where the vendor supports it.
5. Add tests that attempt `GET/POST/DELETE` against representative control-plane paths and assert a gateway-local 403 without contacting upstream.
6. Reframe A1: “secret bytes do not enter the VM” is true; “the task receives only budget-capped model-call authority” is not currently true.

### F-04: High: A task can exhaust host disk or memory through the bind mount, output, and diff

**Evidence**

- The task has CPU and memory limits, but no disk, inode/file-count, stdout, or process-count limit; `/work` is a writable host bind mount (`internal/runner/runner.go:18-40`).
- Agent stdout is copied without a byte limit into the host audit log and host stdout (`internal/broker/broker.go:527-545`).
- `git add` stages every work-tree change, and `git diff --cached` is captured completely into a Go string (`internal/stage/stage.go:76-90`).
- The complete diff is then written again to the audit directory (`internal/broker/gates.go:92-100`).
- The documented audit-retention risk covers monotonic historical growth, not a single hostile task (`SECURITY.md:150-153`).

**Attack**

The untrusted agent runs `yes`, creates a huge/sparse file, or creates millions of small files under `/work`. It can fill the host filesystem through the bind mount or audit stream. If staging succeeds, `CaptureDiff` can allocate the full textual diff in broker memory and then duplicate it during persistence/HTTP/UI handling, causing host OOM. The VM's 4 GB memory cap does not constrain bytes written to the host mount or streamed through stdout.

**Remediation**

1. Back each task work tree with a quota-limited volume/disk image rather than an unconstrained host directory; set byte and inode limits.
2. Apply a bounded writer to stdout/stderr and terminate the task when the audit-output ceiling is exceeded.
3. Enforce maximum changed files, maximum individual file size, and maximum total diff size before `git add`/review.
4. Stream diff generation to a size-limited file instead of returning one unbounded string. Render/review it through bounded readers.
5. Reserve host free-space headroom and fail closed before task start; monitor during execution.
6. Add red-team tests for output floods, one huge file, a sparse file, and inode exhaustion.

### F-05: High: Gateway request/response buffering permits host OOM

**Evidence**

- Anthropic subscription mode declares request fields to strip (`internal/gateway/vendor.go:60-80`), which sends every JSON request through `stripRequestFields`.
- That function performs unbounded `io.ReadAll(r.Body)` and then unmarshals another full representation, with no gateway body limit (`internal/gateway/gateway.go:221-267`).
- The gateway HTTP server deliberately has no `ReadTimeout` and applies the same generic server configuration to long-lived streams (`cmd/brokerd/main.go:455-464`).
- The usage reader buffers the current line without a maximum and also retains every line containing the substring `usage` (`internal/gateway/gateway.go:341-403`). A non-streaming or malicious upstream response with no newline can therefore be buffered in full.

**Attack**

In Anthropic subscription mode, a task sends a very large or never-ending JSON body to the gateway using its valid bearer. The trusted host process reads it into memory. The request cap defaults to 1,000 for uncapped modes, so many admitted slow/concurrent bodies can also consume goroutines, sockets, and memory. Separately, an allowed upstream response with a giant newline-free body makes `usageReader.line` grow without bound.

**Remediation**

1. Put provider-specific maximum request sizes around the body before any read and return 413 when exceeded.
2. Avoid generic JSON map rewriting; use a bounded streaming transform or construct/validate only the request shape the provider route permits.
3. Cap SSE/event line length and total retained usage bytes. Abort metering safely if limits are exceeded.
4. Add per-lease concurrency limits, server connection limits, `MaxHeaderBytes`, and a body-progress deadline compatible with streaming.
5. Add tests for oversized content-length, chunked endless bodies, large JSON, huge no-newline responses, and many concurrent slow bodies.

### F-06: High: Loopback TCP broker can be driven through DNS rebinding

**Evidence**

- `broker.addr` has no authentication and exposes task submission plus approve/deny/kill and task enumeration (`cmd/brokerd/main.go:427-434`, `SECURITY.md:121-147`).
- The listener correctly refuses non-loopback addresses, but the handler has no `Host` or `Origin` validation and no bearer token (`cmd/brokerd/main.go:489-509`).
- The separate web UI already implements the missing controls: loopback Host, loopback Origin, and a bearer token (`internal/webui/server.go:107-130`).

**Attack**

When the optional TCP listener is enabled, a malicious website can use DNS rebinding: serve JavaScript from an attacker hostname/port, then re-resolve that hostname to `127.0.0.1`. Because browser origin is based on the hostname rather than the resolved IP, the script can read `/admin/pending` and `/admin/tasks`, approve a pending diff, kill tasks, or submit a task. Binding only to loopback does not stop this class of browser-origin attack; the broker accepts the attacker's `Host` header.

**Remediation**

1. Remove unauthenticated TCP mode, or require a strong bearer/mTLS credential for every route.
2. At minimum, reject non-literal-loopback `Host` values and non-loopback `Origin` values exactly as the web UI does. Authentication is still preferable.
3. Separate health/read-only endpoints from mutation endpoints and require authorization on both.
4. Add a test with `Host: attacker.example:<port>` and a non-loopback Origin against a loopback listener.
5. Update SECURITY.md: an SSH tunnel to an unauthenticated loopback service is not a complete browser-origin defense.

### F-07: Medium: Untrusted output can forge authoritative audit results

**Evidence**

- The agent CLI and its outputs are explicitly untrusted (`THREAT_MODEL.md:78-80`).
- Raw agent stdout is written directly into the same JSONL file as broker-authored `drydock_meta` and `drydock_task` records (`internal/broker/broker.go:394-432`, `internal/broker/broker.go:544-545`).
- On successful Claude runs, brokerd does not append an authoritative result; it trusts the CLI's result line. Only non-Claude agents get a synthetic result using gateway spend (`internal/broker/broker.go:578-585`).
- Audit parsing accepts the last JSON object with `type == "result"` (`internal/audit/audit.go:54-82`). Aggregate-budget recovery seeds from that parsed cost (`cmd/brokerd/main.go:676-710`).

**Impact**

A compromised/untrusted CLI can append a fake final result, outcome, duration, or `total_cost_usd`. This corrupts operator history and can understate the rolling aggregate ledger after broker restart. Even if normal Claude stream formatting makes shell-output injection awkward, the threat model explicitly treats the CLI binary itself as untrusted, so its stdout cannot be an integrity source.

**Remediation**

1. Separate broker control records from raw agent output, either into different files or an authenticated/framed event envelope the agent cannot emit.
2. Always append a broker-authored terminal result for every agent, using `grant.Spent()` and broker-observed duration/outcome.
3. Seed financial controls only from broker-authored gateway ledger records, not CLI-reported cost or file mtime.
4. Add a test where agent output contains several forged result records and verify UI/history/ledger use only the broker record.

### F-08: Medium: Configuration typos silently disable controls

**Evidence**

- Main configuration is decoded with `yaml.Unmarshal`, which ignores unknown mapping keys (`internal/config/config.go:240-263`).
- Egress configuration does the same (`internal/egress/egress.go:110-122`). The especially important `requires_approval` field does fail closed when absent, which is good, but unknown fields elsewhere remain silent.
- Several safety controls default off: `strict_container_version`, `aggregate_budget_usd`, `max_request_cost_usd`, and API-key `task_max_requests` (`config/config.yaml:14-24`, `config/config.yaml:50-52`).

**Impact**

An operator can misspell `aggregate_budget_usd`, `max_request_cost_usd`, `strict_container_version`, `broker`, or a nested provider field and receive a valid startup using a weaker default. In unattended use this is a realistic fail-open operational error.

**Remediation**

Use `yaml.Decoder` with `KnownFields(true)` for both files, reject trailing YAML documents, and provide migration aliases explicitly when compatibility is needed. Add typo tests for every security-relevant field and make `drydock doctor` print the effective resolved security posture.

### F-09: Medium: Sandbox supply-chain inputs are mutable despite the pinning claim

**Evidence**

- The roadmap states “every external input is pinned and bumped deliberately” (`docs/ROADMAP.md:106-116`).
- The Dockerfile pins the base image digest and top-level CLI versions, but runs mutable `apt-get update && apt-get upgrade`, so package versions depend on build time (`image/Dockerfile:10-21`).
- The Go tarball is streamed directly from the network into root-owned `/usr/local` without verifying a published checksum or signature (`image/Dockerfile:23-31`).
- Global `npm install` commands have no lockfile or vendored dependency graph; the allowlist itself acknowledges that transitive dependencies float on every build (`image/Dockerfile:33-56`, `image/cve-allowlist.yaml:12-22`). Install scripts execute as root during the image build.
- GitHub Actions are referenced by mutable major tags rather than commit SHAs (for example `.github/workflows/release.yml` and `.github/workflows/test.yml`).

**Impact**

Two builds from the same drydock commit can contain different apt and npm dependency trees. A registry/CDN compromise or malicious transitive update can execute during a root image build. The daily CVE scan improves detection of known vulnerabilities but is not provenance or reproducibility, and scanning after install does not prevent a malicious install script.

**Remediation**

1. Download the Go archive separately and verify its official SHA-256 before extraction.
2. Generate and commit lockfiles for the complete npm dependency graph; install with an immutable lockfile and disable scripts except for packages whose scripts are reviewed and explicitly required.
3. Use a dated Debian snapshot or record exact package versions/digests in a generated lock/SBOM that setup verifies.
4. Pin GitHub Actions by full commit SHA and use dependency automation to update them.
5. Amend the roadmap claim to distinguish pinned top-level selectors from a reproducible, fully locked image.

### F-10: Low: Security-relevant operator documentation has drifted

Examples at this commit:

- `config/config.yaml` calls `task_budget_usd` a “hard USD ceiling” (`config/config.yaml:14`) while the threat model correctly calls it soft and F-02 shows a concurrency bypass.
- README still says unattended daemon use has a “no-aggregate-cap caveat” (`README.md:98`), while the roadmap and configuration say the aggregate cap landed.
- `drydock daemon install` prints “no aggregate spend cap yet” even when the feature is configured (`cmd/drydock/daemon.go:318-320`).
- A2 still says privilege dropping uses `gosu` (`THREAT_MODEL.md:122-131`), but the image uses `setpriv` and removed gosu.
- Subscription documentation says operators must explicitly set `task_max_requests` and describes the unset case as unbounded (`THREAT_MODEL.md:236-267`), while brokerd now silently applies a built-in cap of 1,000 (`cmd/brokerd/main.go:67-82`, `cmd/brokerd/main.go:336-358`).
- The roadmap says a runaway loop can no longer drain an API key in aggregate (`docs/ROADMAP.md:213-223`), which is too strong while in-flight aggregate reservations are absent.

These contradictions are not cosmetic in a security product: operators use them to decide whether unattended execution and financial exposure are acceptable.

**Remediation:** make one generated security-capabilities table the source for README, config comments, docs, and CLI post-install output. Add tests for high-risk user-facing claims and require a threat-model/doc review when defaults change.

## Cross-cutting test gaps

The current red-team suite proves its named cases, but several tests are narrower than the corresponding product claim:

- **A1 checks secret bytes, not delegated authority.** A sentinel key absent from VM environment does not prove the bearer is route-scoped or budget-safe.
- **A2 checks destination hosts, not paths or data semantics.** The gateway is an allowed host through which unintended API actions and persistent uploads remain possible.
- **A3 checks Git hooks only.** It does not cover repository-controlled symlinks used by trusted pre-VM filesystem writes.
- **A7 checks cleanup between tasks, not host resource exhaustion during a task.** Ephemeral cleanup after disk-fill/OOM is too late.
- VM-backed A1/A2/A7 are manual on macOS rather than CI-enforced. A release can therefore be green without running the isolation tests most closely tied to its headline claims.

Add F-01 through F-06 as named adversarial cases, and change the threat-model mapping from one test per broad claim to one test per independently exploitable boundary.

## Positive controls observed

The following controls are well designed and should be preserved:

- The default per-UID Unix socket uses a `0700` parent and atomically restrictive socket creation, avoiding a bind/chmod race (`cmd/brokerd/main.go:511-533`).
- Non-loopback broker TCP binds are refused, closing direct VM self-approval (`cmd/brokerd/main.go:489-509`).
- The VM firewall is applied atomically and privilege dropping removes the capability bounding set with `no_new_privs` (`image/init-firewall.sh`, `image/drop-agent.sh`).
- Squid applies destination-private-address denial before hostname allow rules, reducing DNS-rebinding SSRF into the host/LAN (`internal/netfw/squid.go:25-40`).
- Egress host validation rejects wildcards, IP literals, malformed hostnames, and missing ports (`internal/egress/egress.go:42-107`).
- Host-side Git explicitly neutralizes hooks and fsmonitor and keeps `.git` outside the VM mount (`internal/stage/stage.go:50-60`).
- Audit and web-UI reads use `O_NOFOLLOW`; the web UI uses loopback Host/Origin checks and a non-cookie bearer token (`internal/audit/audit.go:18-25`, `internal/webui/server.go:107-130`).
- Task request bodies are capped at 64 KiB and repository references reject local paths (`internal/broker/broker.go:196-203`, `internal/broker/broker.go:247-257`).
- Credential files and task audit artifacts are generally created with restrictive modes.

## Verification performed

- `go test -race -count=1 ./...`: **pass** (required permission to create local TCP/Unix test listeners; the first sandboxed run failed only because listeners were prohibited).
- `go vet ./...`: **pass**.
- `go run golang.org/x/vuln/cmd/govulncheck@v1.4.0 ./...`: **No vulnerabilities found** for reachable Go dependency code as of the review date.
- `make redteam`: **pass** for host-side A3–A6.
- VM-backed `make redteam-vm`, live Squid, sandbox-image build/Grype scan, and real-vendor integration were not run in this environment. This limits validation of Apple `container`, nft, live proxy behavior, current image CVEs, and vendor wire compatibility; it does not affect the source-level findings above.

## Recommended remediation order

1. **Immediately:** move prompt/control files outside the untrusted work tree (F-01); add the symlink regression.
2. **Immediately:** serialize or conservatively reserve every task request and aggregate reservation (F-02); correct “hard cap” claims.
3. **Before unattended use:** route/method allowlist the gateway (F-03), add disk/output/diff quotas (F-04), and bound gateway bodies/lines/connections (F-05).
4. **Before supporting TCP mode:** require authentication and browser-origin defenses (F-06).
5. **Then:** make audit financial records broker-authoritative (F-07), reject unknown config fields (F-08), lock the image supply chain (F-09), and reconcile all operator-facing claims (F-10).
