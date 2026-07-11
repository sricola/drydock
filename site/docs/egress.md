# Egress & widening

The sandbox's internet access is **deny-by-default**. The agent can reach only
the hosts on your allowlist; everything else is blocked. This is what stops a
hostile agent from exfiltrating your code or calling home.

## How enforcement works

Two host-side components sit on the seam between the VM and the internet:

- The **credential gateway** (`:8088`) handles the model API
  (`api.anthropic.com`, `api.openai.com`, `generativelanguage.googleapis.com`). The agent talks to the gateway with a
  per-task token; the gateway holds the real key. These hosts are deliberately
  **not** on the proxy allowlist; they route through the gateway, not the proxy.
- The **squid proxy** (`:3128`) handles all other egress (package registries,
  git, anything the agent's tools fetch). It enforces the hostname allowlist;
  a CONNECT to a non-allowlisted host returns 403.

Inside the VM, a default-deny firewall (`init-firewall.sh`) allows traffic only
to the gateway and proxy ports, so the agent has no path to the internet that
bypasses them.

## The default allowlist

`~/.drydock/egress.yaml` is the source of truth (the seed template lives at
`$HOMEBREW_PREFIX/share/drydock/config/egress.yaml`):

```yaml
default:
  domains:
    - { host: api.anthropic.com,      ports: [443] }   # routed via gateway
    - { host: api.openai.com,         ports: [443] }   # routed via gateway
    - { host: registry.npmjs.org,     ports: [443] }   # JavaScript, via squid
    - { host: pypi.org,               ports: [443] }   # Python, via squid
    - { host: files.pythonhosted.org, ports: [443] }   # Python, via squid
    - { host: proxy.golang.org,       ports: [443] }   # Go, via squid
    - { host: sum.golang.org,         ports: [443] }   # Go, via squid
per_task_widening:
  requires_approval: true
```

The sandbox image ships **Node 22, Python 3.11, and Go 1.26**, so JS, Python,
and Go tasks work out of the box. Other toolchains: extend `image/Dockerfile`
and rebuild with `make image` (or `drydock init`, which detects stale images).
Restart brokerd after editing the default allowlist.

## Per-task widening

Deny-by-default is strict on purpose, but a real task may need a host that
isn't on the default list (a private registry, an internal API). Request it
per task with `--egress-extra`:

```bash
drydock submit --repo … --instruction "…" \
  --egress-extra internal.example.com:443
```

When `per_task_widening.requires_approval: true` (the default), widening goes
through the **same human gate as the diff push**: brokerd blocks the task,
records the requested hosts to `~/.drydock/audit/<id>.widen.json`, and shows it
in `drydock pending` under gate `egress`. Approve it once you've reviewed:

```bash
drydock pending
drydock approve <id>
```

Approved hosts are reachable **only by that task**, for that task's lifetime:
each task gets its own scoped proxy credential, isolated from other concurrent
tasks. The default (non-widened) egress path is unchanged.

## Plain HTTP vs HTTPS (the CONNECT edge)

squid enforces the allowlist via two different request paths, and the rules
differ:

**HTTPS (CONNECT tunnel):** the agent opens a CONNECT tunnel to the target
host. squid permits CONNECT **only to port 443**; a CONNECT to any other port
is denied immediately, before the allowlist is consulted. Once the tunnel is
open, squid cannot inspect the TLS payload, so an allowlisted HTTPS host is
trusted at the **host level**: any URL path on that host is reachable over the
tunnel. The allowlist matches by hostname (`dstdomain`), not by path.

**Plain HTTP (forward-proxy GET):** a `GET http://host/...` request does not
use CONNECT. Rule 1 (CONNECT-only-443) does not apply, so it reaches the
SSRF guard and the allowlist normally. The default allowlist lists all hosts
on **port 443 only**, so plain HTTP (which targets port 80) is **denied by
default** (fail-closed). This is intentional: if no operator has explicitly
added a `:80` entry, HTTP traffic never flows.

**Asymmetry and remaining limits, stated plainly:**

- If an operator adds a host on port 80 (in `egress.yaml` or via
  `--egress-extra host:80`), plain HTTP to that host becomes allowed. The
  full URL and request body flow through squid in cleartext; squid logs the
  URL. Prefer port 443 (HTTPS) hosts wherever possible.
- A widened non-443 port (e.g. `host:8443`) permits plain-HTTP traffic to
  that port but **not** a CONNECT tunnel. CONNECT is locked to port 443
  regardless of what the allowlist says.
- Enforcement granularity is **host and port**, not path or content. An
  allowlisted host is fully reachable at any path on its allowed port(s).
- The in-VM nft firewall is default-deny and IPv6 fail-closed: there is no
  egress path that bypasses the gateway (`:8088`) and squid (`:3128`). This
  edge describes what squid itself permits, not a bypass.
- The SSRF guard (`http_access deny to_local`) blocks private, loopback,
  link-local, cloud-metadata (`169.254.169.254`), and CGNAT destinations for
  both CONNECT and plain-HTTP requests, before the allowlist is consulted.

## Troubleshooting egress

A CONNECT 403 to a host you expected to reach means it isn't on the allowlist:

```bash
cat ~/.drydock/squid/squid-default-acl.conf    # the compiled allowlist
```

Add it permanently in `egress.yaml`, or per-task with `--egress-extra`. If the
VM reaches a host it *shouldn't*, confirm `init-firewall.sh` ran inside the VM;
overriding `--entrypoint` skips it.
