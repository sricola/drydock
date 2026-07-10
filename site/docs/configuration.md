# Configuration

`drydock init` creates `~/.drydock/` (mode `0700`) and seeds two files:

| Path | What |
|---|---|
| `~/.drydock/config.yaml` | Operator settings (network, gateway IP, budget, timeout, concurrency, paths, listener, behavior flags) |
| `~/.drydock/egress.yaml` | The allowlist: hosts and ports the sandbox may reach (see [Egress](egress.html)) |

Both are seeded from defaults the first time; `drydock init` never overwrites
them. **Env vars win over file values**, so existing scripts keep working. Edit
`config.yaml` and re-run `drydock start`.

The vendor keys (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GEMINI_API_KEY`) are
intentionally **not** in these files; they live in your shell env, or at
`~/.drydock/api-keys.env` (mode `0600`), read host-side and never passed into
the VM. All three keys are recognized automatically; no extra config is needed
to declare them.

## Common settings

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| `anthropic_auth` | `DRYDOCK_ANTHROPIC_AUTH` | `api_key` | `api_key` uses `ANTHROPIC_API_KEY`; `subscription` uses `~/.drydock/claude-oauth.json` |
| `openai_auth` | `DRYDOCK_OPENAI_AUTH` | `api_key` | `api_key` uses `OPENAI_API_KEY`; `subscription` uses `~/.drydock/codex-oauth.json` |
| `default_agent` | `DRYDOCK_DEFAULT_AGENT` | `claude` | Agent when `--agent` is omitted (`claude` \| `codex` \| `gemini` \| `opencode`) |
| `default_model` | `DRYDOCK_DEFAULT_MODEL` | *(empty)* | `--model` fallback for **Claude Code and Codex only**; empty = the agent picks. Not applied to `gemini` (uses its own `gemini-2.5-pro` default) or `opencode` (uses `openai_compat.model`). |
| `task_budget_usd` | `DRYDOCK_TASK_BUDGET_USD` | `2.0` | Per-task USD ceiling (`api_key` mode only; unused in subscription mode) |
| `task_max_requests` | `DRYDOCK_TASK_MAX_REQUESTS` | `0` (unlimited) | Hard cap on API round-trips per task; the primary runaway control in subscription mode |
| `task_timeout` | n/a | `30m` | Wall-clock per task |
| `approval_timeout` | n/a | `0s` | Auto-deny a task left at an approval gate after this long; `0` = wait forever (right for interactive use; set for unattended runs) |
| `max_concurrent_tasks` | `DRYDOCK_MAX_CONCURRENT_TASKS` | `2` | Excess POSTs to `/tasks` get HTTP 503 |
| `notifications` | `DRYDOCK_NO_NOTIFY=1` (off) | `true` | macOS notifications on pending approval |

## Bring your own model

`opencode` reaches any OpenAI-compatible endpoint via the `openai_compat` block
in `config.yaml` (or the `drydock setup` wizard). There is **no env override**;
configure it in the file. The real key is referenced by env-var **name**, never
stored here.

| Key (under `openai_compat:`) | Meaning |
|---|---|
| `base_url` | Endpoint host, e.g. `https://generativelanguage.googleapis.com` (empty = disabled; https, or http only for `localhost`) |
| `base_path` | Path joined onto the request, e.g. `/v1beta/openai` |
| `api_key_env` | **Name** of the host env var holding the real key (e.g. `GEMINI_API_KEY`) |
| `model` | Model id passed to the agent, e.g. `gemini-2.5-pro` |
| `prices` | Optional `{<model>: {input, output}}` USD per 1M tokens; enables USD budgeting, omit to rely on `task_max_requests` |

**Streaming and USD metering.** Streaming `chat/completions` responses commonly
omit token usage unless the client explicitly requests it (via
`stream_options.include_usage`). drydock does not inject that option, so a
*streamed* task against a priced `openai_compat` endpoint may be metered at $0
against `task_budget_usd`: the response completes but carries no usage to bill.
The usage-independent backstop is `task_max_requests`: it counts every API
round-trip regardless of whether the upstream reports usage. Set
`task_max_requests` for any `openai_compat` lane where streaming is expected.

**`prices` and the `"default"` row.** The `prices` map is keyed by model id. If
a task uses a model not explicitly listed and no `"default"` row exists, drydock
has no price to apply and meters that call at $0, so the USD budget will never
trip for that model. Add a `"default"` entry to catch unlisted models:

```yaml
openai_compat:
  prices:
    my-model: {input: 1.00, output: 3.00}
    default:  {input: 1.00, output: 3.00}  # fallback for any unlisted model
```

See [Bring your own model](models.html) for worked examples.

## Native Gemini

`--agent gemini` (`default_agent: gemini`) uses Google's native Gemini API
directly. No `openai_compat:` block is needed; just set `GEMINI_API_KEY` in
your env or `~/.drydock/api-keys.env`. `GEMINI_API_KEY` is a recognized key
automatically. There is no subscription mode for Gemini; API key is the only
auth path.

```yaml
default_agent: gemini          # make Gemini the default (defaults to gemini-2.5-pro)
```

`default_model` does not affect Gemini; pick a non-default Gemini model per task
with `--model gemini-2.5-flash`.

## Advanced: runtime, paths, listener

| Field (`config.yaml`) | Env override | Default | Meaning |
|---|---|---|---|
| `network` | `DRYDOCK_NETWORK` | `drydock-egress` | vmnet network name |
| `gateway_ip` | `DRYDOCK_GW_IP` | `192.168.66.1` | Gateway + squid bind here |
| `sandbox_image` | `SANDBOX_IMAGE` | `drydock-sandbox:latest` | Per-task agent VM image |
| `anchor_image` | `DRYDOCK_ANCHOR_IMAGE` | `drydock-anchor:latest` | Minimal image holding the vmnet gateway IP |
| `stage_root` / `audit_root` / `squid_run_dir` | `STAGE_ROOT` / `AUDIT_ROOT` / `SQUID_RUN_DIR` | `~/.drydock/{stage,audit,squid}` | Per-task scratch (audit dir `0700`; log + diff `0600`) |
| `broker.socket` | `BROKER_SOCKET` | `$TMPDIR/drydock-$UID/drydock.sock` | Unix socket (parent dir `0700`, socket `0600`) |
| `broker.addr` | `BROKER_ADDR` | *(empty)* | `host:port` to expose over TCP (**no auth**; see [SECURITY.md § TCP exposure](https://github.com/sricola/drydock/blob/main/SECURITY.md#tcp-exposure-brokeraddr--broker_addr)) |
| `log_json` | `DRYDOCK_LOG_JSON=1` | `false` | Force JSON logs even on a TTY |
| `strict_container_version` | `DRYDOCK_STRICT_CONTAINER_VERSION=1` | `false` | Fail closed when `container`'s major drifts from the tested range |
| n/a | `EGRESS_CONFIG` | `~/.drydock/egress.yaml` | Path override for the egress YAML |

Gateway port `8088` and squid port `3128` are hard-coded in
`cmd/brokerd/main.go` and `image/entrypoint.sh`; change both together.
