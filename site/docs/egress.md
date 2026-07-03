# Egress & widening

The sandbox's internet access is **deny-by-default**. The agent can reach only
the hosts on your allowlist — everything else is blocked. This is what stops a
hostile agent from exfiltrating your code or calling home.

## How enforcement works

Two host-side components sit on the seam between the VM and the internet:

- The **credential gateway** (`:8088`) handles the model API
  (`api.anthropic.com`, `api.openai.com`, `generativelanguage.googleapis.com`). The agent talks to the gateway with a
  per-task token; the gateway holds the real key. These hosts are deliberately
  **not** on the proxy allowlist — they route through the gateway, not the proxy.
- The **squid proxy** (`:3128`) handles all other egress (package registries,
  git, anything the agent's tools fetch). It enforces the hostname allowlist;
  a CONNECT to a non-allowlisted host returns 403.

Inside the VM, a default-deny firewall (`init-firewall.sh`) allows traffic only
to the gateway and proxy ports — so the agent has no path to the internet that
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

Deny-by-default is strict on purpose — but a real task may need a host that
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

Approved hosts are reachable **only by that task**, for that task's lifetime —
each task gets its own scoped proxy credential, isolated from other concurrent
tasks. The default (non-widened) egress path is unchanged.

## Troubleshooting egress

A CONNECT 403 to a host you expected to reach means it isn't on the allowlist:

```bash
cat ~/.drydock/squid/squid-allow.txt    # the compiled allowlist
```

Add it permanently in `egress.yaml`, or per-task with `--egress-extra`. If the
VM reaches a host it *shouldn't*, confirm `init-firewall.sh` ran inside the VM —
overriding `--entrypoint` skips it.
