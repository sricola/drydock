# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [SemVer](https://semver.org/spec/v2.0.0.html). Each
entry below corresponds to a Git tag of the same name.

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
