# Security policy

drydock is a containment for autonomous coding agents on macOS. A security
bug in drydock can let an untrusted agent reach things it shouldn't —
your real API key (Anthropic or OpenAI), your host filesystem, your git
credentials. We take reports seriously and respond quickly.

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
- Which `container` and agent CLI (`claude-code` / `codex`) versions were installed.
- Your assessment of the impact (read host file? exfil key? push without
  approval?).

We'll acknowledge within 72 hours and aim to ship a fix within 14 days
for high-severity issues. We'll credit you in the advisory unless you
prefer to stay anonymous.

## Scope

In scope (please report):

- Anything that lets an in-VM agent read or exfiltrate a real vendor key
  (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY`).
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
- Operator-key hygiene (a leaked `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`
  defeats the gateway; drydock doesn't manage its lifecycle).
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

## Verifying a release

Each tagged release is built by the `release` GitHub Actions workflow and
carries supply-chain attestations you can check before trusting a binary
(`X.Y.Z` = the release version):

- **SLSA build provenance** — the tarball was built by drydock's release
  workflow from the tagged commit, not uploaded by hand:

  ```
  gh attestation verify drydock-vX.Y.Z-darwin-arm64.tar.gz --repo sricola/drydock
  ```

- **cosign signature** (keyless / Sigstore — no key to trust):

  ```
  cosign verify-blob \
    --certificate drydock-vX.Y.Z-darwin-arm64.tar.gz.pem \
    --signature  drydock-vX.Y.Z-darwin-arm64.tar.gz.sig \
    --certificate-identity-regexp 'https://github.com/sricola/drydock/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    drydock-vX.Y.Z-darwin-arm64.tar.gz
  ```

- **SBOM** — `drydock.cdx.json` (CycloneDX) lists the module dependencies.
- **sha256** — the `.sha256` asset matches the value the Homebrew formula pins.
- **Reproducible binaries** — rebuild the binaries and confirm they match
  byte-for-byte (needs Go 1.26.4 on darwin/arm64, a clean tag checkout):

  ```
  git checkout vX.Y.Z
  gh release download vX.Y.Z -R sricola/drydock -p '*-bin.sha256'
  make verify-build SUMS=drydock-vX.Y.Z-darwin-arm64-bin.sha256
  ```

  The release tarball itself is not byte-reproducible (tar/gzip embed
  metadata), but the binaries inside it are — which is what matters.

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
  this becomes a disk-fill DoS. Run `drydock prune --older-than DUR`
  (dry-run unless `--yes`), or schedule it via cron.

- **macOS notifications contain attacker-controlled hostnames.**
  Per-task `egress_extra` hostnames flow into the notification body.
  macOS native notifications render plain text, so this is benign on
  the macOS surface; treat the body as untrusted input if you ever
  route it through a webhook adapter.

### Subscription auth — host-held full-account OAuth credential

When `anthropic_auth: subscription` is set, drydock stores a copy of the
Claude Pro/Max OAuth credential at `~/.drydock/claude-oauth.json` (mode
`0600`). This file holds a full-account OAuth access token **and** refresh
token; the gateway uses them to issue per-task bearers, and the credential
itself never enters the VM (A1 holds).

The blast radius is broader than a scoped API key in two ways:

- **Not per-task revocable.** A minted bearer expires with the task. The
  OAuth credential at `~/.drydock/claude-oauth.json` does not. If the host
  is compromised, an attacker obtains a long-lived refresh token tied to the
  full Anthropic account — not just a budget-limited bearer.
- **Not narrowly scoped.** A dedicated `ANTHROPIC_API_KEY` can be created
  with limited permissions or revoked independently. A personal OAuth token
  carries the same permissions as an interactive browser session.

This is a deliberate trade-off for the operator who does not want to maintain
a paid API key. Protect `~/.drydock/claude-oauth.json` the same way you
would any credential file. Revocation means re-authenticating via
`claude login` (which rotates the token in Anthropic's system) and running
`drydock auth claude` again to update the stored copy.

**ToS and rate-limit caveat.** Automating a personal subscription headlessly
may brush against Anthropic's terms of service for Claude Pro/Max accounts,
and subscription rate limits are lower than API rate limits — a batch job
will exhaust them faster than interactive use would. drydock makes no claim
that this usage mode is sanctioned by Anthropic. The operator assumes that
risk. See [`THREAT_MODEL.md` § N4](THREAT_MODEL.md#n4-cost-exhaustion-and-runaway-tasks)
for how to control subscription task runaway with `task_max_requests`.

- **Apple notarization still pending.** Each tagged release now ships a
  CycloneDX SBOM, a keyless **cosign** signature, and **SLSA build
  provenance** (the tarball was built by the release workflow from the
  tagged commit) — see "Verifying a release" above. The macOS binaries are
  not yet Apple-notarized, so a non-brew download may be quarantined by
  Gatekeeper; notarization lands once the Developer ID certificate is in
  place. You can also always build from source and `go mod verify`.
