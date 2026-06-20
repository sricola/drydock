# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [SemVer](https://semver.org/spec/v2.0.0.html). Each
entry below corresponds to a Git tag of the same name.

## v0.1.10 — 2026-06-19

### Added

- **`drydock setup`** — one command from install to ready. It installs the
  Homebrew prerequisites (Apple `container`, squid), prompting before each
  (`--yes` to skip), then runs `drydock init`. Previously `init` only *checked*
  the prerequisites and exited if they were missing, so the happy path is now
  `brew install drydock` → `drydock setup` instead of a multi-step prerequisite
  hunt. Without a TTY and without `--yes`, it prints the install command and
  stops rather than silently modifying the system.

### Changed

- `drydock doctor` gives an **actionable message when Codex is missing** from
  the sandbox image: it names the offending image and points at the fix (run
  `drydock init` to rebuild, or correct a stale `sandbox_image`) instead of
  dumping a raw `/bin/sh: codex: not found`. The usual cause is a config that
  predates the v0.1.5 `claude-sandbox` → `drydock-sandbox` rename.

### Fixed

- Website: selected text is legible inside the dark terminal/code blocks. The
  global `::selection` rule had forced selected text to the dark page color,
  rendering it invisible (dark-on-dark) when highlighted in a terminal block.

### Docs

- README: a real `drydock redteam` screenshot beside the command, showing the
  A1/A2/A7 containment attacks passing live against the sandbox.

## v0.1.9 — 2026-06-19

### Added

- **`drydock redteam`** runs drydock's live containment attacks against your
  own sandbox — on your own Mac, on your actual image — and prints a pass/fail
  table, so you can verify containment yourself instead of trusting the threat
  model. It runs the VM-backed attacks behind **A1** (the real vendor key never
  enters the VM), **A2** (egress to non-allowlisted hosts is blocked), and
  **A7** (no state persists between tasks). No API spend — the attacks inspect
  the VM env, egress, and filesystem; they never call a model.
- **Breach demo** — `demo/breach.sh` (`make demo`, `make demo VM=1`) is a
  narrated runner that executes the real `THREAT_MODEL.md` red-team tests and
  shows them contain actual attacks; every green is a live `go test` pass, not a
  scripted print. Recorded GIFs ship in `demo/` and lead the README and website.

### Fixed

- The `log_json`, `strict_container_version`, and `notifications` keys in
  `~/.drydock/config.yaml` were parsed but never honored — brokerd read each
  one's `DRYDOCK_*` env var directly at the point of use, so setting the YAML
  key did nothing. Config is now loaded first and drives logging, the
  container-version check, and notifications. The env vars keep working (config
  still folds them on top, env winning), so precedence is unchanged.

### Changed

- Front-door copy leads with the local-first stance — *run coding agents on your
  own Mac like you assume they're already hacked* — across the README tagline
  and the site hero. The README install now opens with a one-line macOS-26 /
  Apple-silicon eligibility self-check so users self-qualify in seconds.

## v0.1.8 — 2026-06-19

A hardening release: reliability, performance, and supply-chain work from an
internal audit. No new user-facing features; behavior is unchanged except where
noted.

### Fixed / reliability

- **Graceful shutdown.** On `SIGINT`/`SIGTERM`, brokerd now cancels every
  in-flight task (each tears down its own VM and answers the client), drains
  the HTTP server, stops squid + the anchor, and removes the socket — instead
  of `os.Exit`-ing and orphaning running VMs until the next boot.
- **HTTP server timeouts** (`ReadHeaderTimeout` + `IdleTimeout`) on the broker
  and gateway listeners blunt slow-loris / idle-keepalive abuse without cutting
  off long agent streams or the blocking approval wait.
- Task IDs and gateway tokens **fail closed** if `crypto/rand` ever fails
  (no zero-filled IDs/tokens); failed VM force-deletes and squid stops are now
  logged instead of silently swallowed.

### Performance

- The credential gateway **meters usage by streaming** the response line by
  line and keeping only the few usage-bearing events, instead of buffering the
  whole multi-MB SSE body. Peak memory drops from O(body) to O(one line).
- `drydock tasks` reads each audit log's **tail** (not the whole trace) and
  sorts from cached `mtime` (no per-comparison `stat`).

### Supply chain

- `govulncheck` runs in CI and fails the build on a known dependency
  vulnerability; the sandbox base image is **pinned by digest**.
- Release **binaries are byte-for-byte reproducible**; each release publishes a
  per-binary checksum and `make verify-build` re-derives it.

### Notes

- `DRYDOCK_TASK_BUDGET_USD` is documented as a **soft cap** (post-paid): a
  single in-flight request can overshoot by its own cost before the next is
  refused. See `THREAT_MODEL.md` N4.

## v0.1.7 — 2026-06-19

### Security / credibility

- **Adversarial red-team harness.** Every `THREAT_MODEL.md` claim (A1–A7) is
  now backed by a test that runs the actual attack and asserts containment.
  `make redteam` runs the host-side claims (A3–A6, also in CI); `make
  redteam-vm` runs the VM-backed claims (A1, A2, A7) on macOS / Apple silicon.
  THREAT_MODEL carries a `Verified by:` link for each.
- **Verifiable releases.** Each tagged release now ships a CycloneDX SBOM, a
  keyless **cosign** signature, and **SLSA build provenance** (built by the
  release workflow from the tagged commit), all attached to the GitHub
  release. See `SECURITY.md` "Verifying a release"; `make sbom` generates the
  SBOM locally.

### Changed

- Removed the dead `Broker.Approve` hook (it was set but never called — the
  real gates are `gatePush` / `gateEgressWiden`). Internal cleanup, no
  behavior change.

## v0.1.6 — 2026-06-19

### Added

- **`drydock prune --older-than DUR [--keep-last N] [--yes]`** deletes old
  per-task audit artifacts (`<id>.jsonl` / `.diff` / `.widen.json`) from the
  audit dir, which previously grew unbounded. Dry-run by default (prints what
  it would remove + bytes freed); `--older-than` is required so it can never
  prune everything by accident; `--keep-last` always retains the N
  most-recent tasks. Only touches files matching the task-artifact pattern.
- `brokerd` warns at boot when `default_agent`'s vendor has no API key
  configured — tasks that don't pass `--agent` would otherwise be rejected
  at submit time with no upfront signal.

## v0.1.5 — 2026-06-18

### Added

- **OpenAI Codex as a second agent.** Tasks choose their agent with
  `drydock submit --agent claude|codex`; the operator default is set via
  `default_agent` (config) / `DRYDOCK_DEFAULT_AGENT` (env), default
  `claude`. The credential gateway gained a vendor registry: the real key
  for whichever vendor (Anthropic or OpenAI) stays **host-only**, the VM
  only ever sees a budget-capped bearer token, and per-task USD metering +
  revoke apply to both. `brokerd` now accepts **at least one** of
  `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`. `drydock tasks` shows
  duration + metered cost + outcome for Codex tasks too.

### Changed

- **The sandbox image is renamed `claude-sandbox` → `drydock-sandbox`**
  and now ships both the Claude Code and Codex CLIs; `entrypoint.sh`
  dispatches on `DRYDOCK_AGENT`. Re-run `drydock init` to rebuild (it
  detects the stale image); set `SANDBOX_IMAGE` if you had pinned the old
  name. `api.openai.com` is added to the default egress allowlist and,
  like `api.anthropic.com`, routes through the gateway rather than squid.

### Fixed

- `drydock start` now accepts either vendor key — it previously refused
  to start without `ANTHROPIC_API_KEY`, blocking Codex-only operation
  even though `brokerd` itself accepted either key.

### Notes

- Codex routes through the gateway via a generated `model_provider`
  config written by the entrypoint (Codex ignores `OPENAI_BASE_URL`); the
  real key still never enters the VM. The OpenAI entries in the budget
  gate's pricing table are approximate — a safety cap, not a billing
  source of truth.

## v0.1.4 — 2026-06-17

### Changed

- **State dirs default to `~/.drydock/{stage,audit,squid}`** instead of
  `/tmp/broker/{stage,audit,squid}`. Audit history was silently
  evictable on `/tmp` (tooling, OS upgrades, disk pressure all treat it
  as scratch). Existing operators upgrading from < v0.1.4 still see
  pre-existing `/tmp/broker/audit` history in `drydock tasks` while it
  exists — a legacy fallback path triggers when the new default is
  empty. The seeded config now uses `~/.drydock/...`; tilde-expansion
  is applied at load time.

### Fixed

- **`drydock submit` propagates ^C as a request cancellation.** The
  comment claimed this for a while; the code used `context.Background()`
  so ^C just orphaned the CLI while brokerd kept running. Now wired
  through `signal.NotifyContext`.
- **Friendlier "brokerd not running" messages.** `drydock submit`,
  `approve`, `deny`, `pending`, `kill`, and `status` all detect the
  missing-socket / connection-refused case and print
  `brokerd not running — start it in another shell with drydock start`
  instead of the raw Go HTTP transport error. `drydock kill`
  specifically used to say "no such task" without ever asking brokerd.

### Added

- README: `--model` flag and `default_model` config field documented.
- README: Egress section reflects the Go module proxies and notes which
  runtimes ship in the sandbox image (Node 22, Python 3.11, Go 1.26).
- README: `brew tap` install snippet now includes `brew trust
  sricola/drydock` (personal taps require explicit trust on newer brew).
- `SECURITY.md`: dedicated "TCP exposure" section spelling out that
  `broker.addr` / `BROKER_ADDR` has no built-in auth and naming the
  acceptable deployment patterns (loopback + SSH, mTLS reverse proxy).
- `examples/hello-task.md` — a copy-paste-ready first task that fits the
  default $2 budget and exercises every layer of the boundary.
- `CHANGELOG.md` — this file.
- `THREAT_MODEL.md` opens with a five-bullet TL;DR so the security
  posture is scannable without reading the full doc.

## v0.1.3 — 2026-06-17

### Added

- **`drydock doctor`** — no-API-spend smoke command that checks sandbox
  image freshness (`DRYDOCK_GW_IP` baked in), VM boot, and that the nft
  egress pin enforces (non-allowlisted host blocked). ~5s; gives
  operators a way to validate setup before paying for an API call.
- **`drydock submit --model <id>`** — per-task model passthrough to
  `claude --model`. Empty falls back to `default_model` in
  `~/.drydock/config.yaml`, then to claude's own default.
- **`default_model` in `~/.drydock/config.yaml`** (also
  `DRYDOCK_DEFAULT_MODEL` env override) — operator-level fallback model
  for tasks that don't pass `--model`.
- **Python 3.11 and Go 1.26 in the sandbox image** alongside Node 22.
  Go is installed from the upstream tarball; Debian's `golang-go` is
  too old to be useful. Image grows ~265MB.
- **`proxy.golang.org` and `sum.golang.org` in the default egress
  allowlist** so `go mod download` works inside the sandbox.
- **macOS + Apple-silicon preflight in `drydock init`** — fails loudly
  with a one-line reason on non-darwin, non-arm64, or macOS < 26
  instead of a cryptic downstream `container build` error.
- **Embedded version** — `drydock version` reports `git describe`
  output on source builds (`v0.1.2-10-g26adad8`) instead of `"dev"`.
- **Pricing table covers the 4.x families** (Opus, Sonnet, Haiku) with
  an Opus-priced default fallback so unknown new releases can't undercount
  budgets.

### Fixed

- **`--help` on every subcommand.** Previously `drydock approve --help`
  would try to approve a task literally named `"--help"`; same trap on
  `kill`, `review`, `logs`, etc. Now per-subcommand help intercepts
  before any positional consumption.
- **`drydock init` rebuilds stale sandbox images.** If the entrypoint
  still reads `MACAGENT_GW_IP` (pre-rename layer cache), init detects
  it, deletes the image, and rebuilds `--no-cache`.
- **Failed tasks resolve to `error`, not `running?`.** When a task's
  container exits non-zero before claude can write a terminal `result`
  event, brokerd appends a synthetic result row so `drydock tasks`
  shows the right outcome.
- **`brokerd` upgrade nudge.** `drydock init` checks existing
  `~/.drydock/egress.yaml` for the hosts that ship in the current
  release; missing entries get a one-shot copy-paste hint at the end
  of init.

## v0.1.2 — 2026-06-16

### Changed

- Operator config moved to `~/.drydock/{config,egress}.yaml` — seeded
  by `drydock init`, never overwrites operator edits. Env vars
  (`DRYDOCK_*`, `BROKER_*`) still override file values, so existing
  scripts keep working. `ANTHROPIC_API_KEY` stays env-only by design.

## v0.1.1 — 2026-06-16

(early packaging pass — see git history)

## v0.1.0 — 2026-06-16

Initial public release of drydock — hardware-isolated sandbox for
autonomous coding agents on macOS. Per-task Apple `container` VM,
host-side credential gateway (real key never enters the VM), userspace
squid for hostname-based egress allowlist, host-side `git push`. See
`THREAT_MODEL.md` for the security claims this all backs.
