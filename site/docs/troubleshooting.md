# Troubleshooting

Start with `drydock doctor`; it smoke-tests the sandbox setup (image freshness,
VM boot, the egress pin) with no API spend. Then consult the table below.

## Common failures

| Symptom | First place to look |
|---|---|
| `192.168.66.1 never became bindable` | `container ls -a` (is the anchor running?), `container network inspect drydock-egress` (gateway IP present?) |
| Image build `COPY` fails: `failed to calculate checksum … not found` on a file that exists on disk | Apple `container` ships an empty build context when the context path traverses a symlink. Fixed in v0.5.1 (drydock resolves the path); if you hit it on an older build, run `container build` against the real path (`readlink -f` the image dir). |
| Image build dies at `apt-get`: `Temporary failure resolving 'deb.debian.org'` | In-VM DNS is broken because the host's resolvers are loopback proxies (Cloudflare WARP, dnscrypt, some VPNs) the VM can't reach. `scutil --dns` shows only `127.x` in resolver #1. Fix: `container builder delete --force && container builder start --dns 1.1.1.1`, then rebuild. `drydock doctor` warns when it detects this. |
| Image build fails on `npm install` | Transient registry timeout; rerun `container build` (or `make image`) |
| Squid CONNECT 403 to an expected host | `cat ~/.drydock/squid/squid-default-acl.conf`; add it in `egress.yaml` or per-task with `--egress-extra` (see [Egress](egress.html)) |
| Stale anchor after a crash | `container rm -f drydock-anchor`; the next `drydock start` does this for you |
| Gateway 401 | Key is wrong or a placeholder (`sk-ant-fake` is *expected* to 401) |
| `502 credential unavailable` on a subscription task | The subscription token expired and the refresh failed. brokerd auto-refreshes and (as of the self-heal fix) reloads a token rotated in by another process; if it persists, the refresh token itself is dead; run `drydock auth claude` (or `codex`) to re-authenticate. |
| VM reaches a host it shouldn't | Confirm `init-firewall.sh` ran inside the VM; overriding `--entrypoint` skips it |
| `no usable agent credential` at start | No API key in env / `api-keys.env`, and no `*_auth: subscription` set; see [Authentication](authentication.html) |
| Subscription task errors after spinning | A `task_max_requests` cap was hit (HTTP 429); the agent retries with backoff before exiting; see [Authentication](authentication.html) |

## Where to look

- Per-task agent output (stream-json): `~/.drydock/audit/<id>.jsonl`,
  follow live with `drydock logs <id> -f`.
- The captured diff: `~/.drydock/audit/<id>.diff`.
- An egress-widening request awaiting approval: `~/.drydock/audit/<id>.widen.json`.

## Housekeeping

The audit directory has no automatic retention. Delete old artifacts with:

```bash
drydock prune --older-than 720h --keep-last 50   # dry-run unless you add --yes
```
