# Security policy

drydock is a containment for autonomous coding agents on macOS. A security
bug in drydock can let an untrusted agent reach things it shouldn't —
your real Anthropic key, your host filesystem, your git credentials. We
take reports seriously and respond quickly.

## Reporting

**Please don't open a public GitHub issue for security bugs.**

Use GitHub's private advisory flow instead:

  https://github.com/sricola/drydock/security/advisories/new

That gives you a private channel with the maintainer, an embargo
timeline, and a CVE if one's warranted. If the advisories UI doesn't
work for you, email the address listed on the maintainer's GitHub
profile.

When you report, include:

- A minimal reproduction (commands, config, expected vs. observed).
- Which drydock commit or release you tested against.
- Which `container` and `claude-code` versions were installed.
- Your assessment of the impact (read host file? exfil key? push without
  approval?).

We'll acknowledge within 72 hours and aim to ship a fix within 14 days
for high-severity issues. We'll credit you in the advisory unless you
prefer to stay anonymous.

## Scope

In scope (please report):

- Anything that lets an in-VM agent read or exfiltrate the real
  `ANTHROPIC_API_KEY`.
- Anything that lets an in-VM agent reach a host not on the compiled
  allowlist.
- Anything that lets the host execute code planted in the work tree
  (hooks, fsmonitor, attacker-controlled `.git/config`).
- Anything that bypasses the diff-push approval gate without
  `auto_approve: true`.
- Anything that lets a process other than the brokerd-running user
  reach the Unix socket (mode-bit drift, race in chmod).
- Anything that lets a TCP listener (when `BROKER_ADDR` is set) accept
  approvals from a host other than the operator's.

Out of scope (don't report; documented in `THREAT_MODEL.md`):

- Host compromise (malware on your Mac).
- Guest-to-host escape in Apple `container` or the underlying
  virtualization stack — report those to Apple.
- Supply-chain compromise of dependencies (claude-code, squid, Debian
  base, Go std lib).
- Operator-key hygiene (a leaked `ANTHROPIC_API_KEY` defeats the
  gateway; drydock doesn't manage its lifecycle).
- Operator approving a malicious diff — the gate makes review possible,
  not automatic.
- Prompt injection in staged repo files influencing the agent for that
  task — the diff gate is the backstop.

See `THREAT_MODEL.md` for the precise threat model (A1–A7 defended,
N1–N7 not).

## Supported versions

drydock is pre-1.0. Only `main` is supported; we'll backport fixes to
the most recent tagged release if there's user demand. Always upgrade
to the latest `main` before reporting.

## Documented residuals

These are known limits we've decided not to engineer around at v0.x.
They're listed so an operator can decide whether the risk is acceptable
in their environment, not because they need a CVE.

### TCP exposure (`broker.addr` / `BROKER_ADDR`)

The default brokerd listener is a per-uid Unix socket
(`$TMPDIR/drydock-$UID/drydock.sock`, mode `0600`, parent dir `0700`).
Only processes running as your user can reach it; that's the
authentication.

Setting `broker.addr` (or the `BROKER_ADDR=host:port` env override)
opts into TCP and **drops that authentication entirely**:

- `POST /tasks` runs any agent task against any repo the caller names.
- `POST /admin/approve/{id}` pushes whatever diff is sitting in the
  approval gate, with the host's git credentials.
- `POST /admin/kill/{id}` tears down an in-flight VM.
- `GET /healthz` and `GET /admin/pending` leak running task IDs to
  anyone who can reach the port — recon for a race against approve.

drydock prints `listening on TCP — any process that can reach this port
can submit and approve tasks` at boot when this mode is on. **You are
responsible for the auth layer.** Acceptable patterns: bind to loopback
only and SSH-tunnel; put brokerd behind an authenticating reverse proxy
(mTLS, OAuth); restrict by firewall to a specific operator host. Binding
to `0.0.0.0` on a shared network is a vulnerability, not a feature.

- **`/healthz` and `/admin/pending` disclose task counts and IDs.**
  Fine on the per-user Unix socket; on TCP it's recon information for
  someone preparing a race against `/admin/approve`. Don't expose
  brokerd to untrusted networks.

- **Audit logs don't rotate.** `AUDIT_ROOT` (default
  `~/.drydock/audit`) grows monotonically. On a long-running brokerd
  this becomes a disk-fill DoS. Prune manually or add a cron.

- **macOS notifications contain attacker-controlled hostnames.**
  Per-task `egress_extra` hostnames flow into the notification body.
  macOS native notifications render plain text, so this is benign on
  the macOS surface; treat the body as untrusted input if you ever
  route it through a webhook adapter.

- **No SBOM and no signed binaries yet.** Released artifacts (now
  shipping through the `sricola/drydock` Homebrew tap) are MIT-licensed
  pre-built arm64 binaries verified by the sha256 in the release
  metadata. A real SBOM + Apple notarization land in a later cycle.
  Until then, build from source and `go mod verify`, or check the
  sha256 against the value the formula pins.
