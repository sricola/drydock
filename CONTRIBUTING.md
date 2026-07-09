# Contributing to drydock

drydock is pre-1.0; only `main` is supported. Issues and
PRs are welcome. This file covers the codebase layout, how to build and test,
and the known gaps where help is most useful. User-facing docs live at
[sricola.github.io/drydock/docs](https://sricola.github.io/drydock/docs/); the
security model is in [`THREAT_MODEL.md`](THREAT_MODEL.md) and
[`SECURITY.md`](SECURITY.md).

## Layout

```
cmd/brokerd/      # broker daemon
cmd/drydock/      # operator CLI (setup|init|start|submit|status|tasks|pending|review|approve|deny|kill|prune|logs|doctor|redteam|auth|version)
cmd/docs-build/   # Go-native markdown -> HTML renderer for the docs site (make docs)
internal/
  broker/         # /tasks + admin handlers, approval + egress gates, concurrency, cancellation
  creds/          # Grant/Provider interfaces
  egress/         # YAML loader + allowlist compilation + host/port validation
  gateway/        # credential gateway (mint/serve/account/revoke), constant-time token check
  netfw/          # squid conf + per-task proxy-auth allowlist compiler
  remote/         # PR/MR adapters: github (gh), gitlab (glab), gitea (tea), push-only
  runner/         # `container run` argv builder
  sockpath/       # shared per-uid socket path discovery for brokerd + CLI
  stage/          # work tree, host-side commit + push, curated adapter env
image/            # drydock-sandbox (hosts claude-code + codex): Dockerfile + entrypoint.sh + init-firewall.sh
image/anchor/     # drydock-anchor: FROM scratch + static Go sleep binary
tests/integration # //go:build integration — boots brokerd against the real container CLI
site/             # landing page + docs (site/docs/*.md, rendered by make docs)
```

The design specs and implementation plans from the build-out are archived
on the `design-archive` branch (`git show design-archive:docs/superpowers/`),
kept out of the working tree so the checkout stays focused on the shipped
code. They record *why* decisions were made; the code, `THREAT_MODEL.md`, and
this file carry what a contributor needs day to day.

Three good entry points for understanding the system: **`internal/broker/`**
(the task lifecycle — gates, concurrency, the egress widening hook),
**`internal/gateway/`** (the credential gateway that keeps your key out of the
VM), and **`image/`** (what actually runs inside the sandbox VM).

## Build, test, CI

```bash
make build              # bin/brokerd, bin/drydock
make test               # go test -race ./...
make docs               # render site/docs/*.md -> site/docs/*.html
make image              # both sandbox images
make image-sandbox      # per-task agent image
make image-anchor       # minimal anchor image (FROM scratch + static binary)
make test-integration   # boot brokerd as a subprocess; macOS only, needs the container runtime
```

GitHub Actions runs `go build`, `go test -race`, and `go vet` on every push/PR.
Integration (`make test-integration`) requires the `container` runtime and is
macOS-only — it runs locally, not in CI. No real Anthropic or OpenAI spend.

The sandbox image is CVE-scanned in CI (`image-scan.yml`: grype + `cmd/cve-gate`).
If the gate fails, prefer bumping the pinned package/base; to accept a finding
temporarily, add `{id, reason, expires}` to `image/cve-allowlist.yaml`.
Local repro needs Docker (`docker build -t s image/ && grype docker:s`) —
Apple `container`'s OCI export is not grype-readable.

Some tests are gated behind build tags and require a live host (squid, the
container runtime):

```bash
make test-squid-live   # proxy-auth path (squidlive tag); requires squid on PATH, no container runtime
make test-squid-e2e    # full VM-level egress widening (squide2e tag); requires container runtime + images + squid
```

## Known gaps

- **Pricing** (`internal/gateway/pricing.go`) covers Anthropic 4.x families
  (Opus, Sonnet, Haiku) and OpenAI GPT-5/o4 families, each with a high-end
  default fallback; the budget gate is a safety cap, not a billing source of
  truth. Bump when either vendor publishes new rates.
- **Audit retention** — `~/.drydock/audit/` has no automatic retention. Run
  `drydock prune --older-than DUR [--keep-last N]` (dry-run unless `--yes`);
  brokerd-side auto-retention isn't wired up yet.
- **Concurrency** — up to `DRYDOCK_MAX_CONCURRENT_TASKS` tasks in flight per
  brokerd (default 2); raise on bigger hardware.
- **Approval adapters** — only the local CLI + macOS notifications; no
  Slack/web approval adapters yet.
- **Bitbucket** PR/MR opening falls back to push-only (no widely-adopted CLI to
  wrap). Contribution slot.
- **Apple `container`** is v1.0; flag drift is the most likely breakage source.
  `DRYDOCK_STRICT_CONTAINER_VERSION=1` fails closed on drift.
