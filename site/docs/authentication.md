# Authentication

drydock runs **Claude Code** (Anthropic), **OpenAI Codex** (OpenAI), and
**Gemini** (Google), each with a vendor API key or, for Claude and Codex, your
existing subscription, and **`opencode`** for any OpenAI-compatible endpoint
(see [Bring your own model](models.html)).

Whichever you choose, the real credential stays host-side and **never enters the
VM**: the sandbox only ever sees a per-task token.

Pick the agent per task with `--agent claude|codex|gemini|opencode`, or set
`default_agent` in `config.yaml`.

## The matrix

| Agent | API key | Subscription (no key) |
|---|---|---|
| **Claude Code** | `export ANTHROPIC_API_KEY=…` | `drydock auth claude` + `anthropic_auth: subscription` |
| **OpenAI Codex** | `export OPENAI_API_KEY=…` | `drydock auth codex` + `openai_auth: subscription` |
| **Gemini** | `export GEMINI_API_KEY=…` | API key only; no subscription mode |

An API key is the quickest path for the three agents in the table above
(`opencode` is configured separately: see [Bring your own model](models.html)).
The subscription path lets you reuse a plan you already pay for (Claude and Codex
only; macOS only; needs
the vendor's `claude` / `codex` CLI).

### Gemini: API key only

Gemini (`--agent gemini`) is **API-key auth only**: there is no OAuth /
subscription lane. Set `GEMINI_API_KEY` in your shell env or store it at
`~/.drydock/api-keys.env`. See [Models: Gemini (native)](models.html) for
model choices and the comparison with the OpenAI-compat Gemini route.

### Bring your own model

`opencode` reaches any OpenAI-compatible endpoint (OpenRouter, a local
server, or Gemini via its compat lane). It's API-key-only (no OAuth) and
configured by the `openai_compat` block, not the matrix above. See
[Bring your own model](models.html).

## API key

Set at least one vendor key. Keep it in your shell env, or let the `drydock setup`
wizard store it at `~/.drydock/api-keys.env` (mode `0600`, read host-side). Either
way it never crosses the VM boundary.

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # Claude Code tasks
export OPENAI_API_KEY=sk-...          # OpenAI Codex tasks
export GEMINI_API_KEY=...             # Gemini tasks (native --agent gemini)
drydock start
```

## Subscription (Claude Pro/Max or ChatGPT)

```bash
# Claude
claude login            # log in to your Claude account (opens a browser)
drydock auth claude     # copy the credential into ~/.drydock/claude-oauth.json (0600)
export DRYDOCK_ANTHROPIC_AUTH=subscription   # or set anthropic_auth: subscription in config.yaml

# Codex
codex login             # log in to your ChatGPT account
drydock auth codex      # copy into ~/.drydock/codex-oauth.json (0600)
export DRYDOCK_OPENAI_AUTH=subscription      # or set openai_auth: subscription in config.yaml

drydock start
```

<details>
<summary><b>Important: subscription-mode limits and terms-of-service risk</b></summary>

**Budget vs. request cap.** The USD budget (`task_budget_usd`) does **not** apply
in subscription mode: there's no spend to meter. To stop a runaway task from
burning your subscription's rate limit, set `task_max_requests` in
`config.yaml`. `task_timeout` still applies as a wall-clock backstop. The cap
stops *inference* the moment it's hit (the gateway returns HTTP 429), but the
agent retries with backoff before giving up, so a capped task can spin for a
minute or two before erroring out.

**Credential blast radius.** The stored OAuth credential
(`~/.drydock/claude-oauth.json` or `codex-oauth.json`) is a **full-account
token**, broader than a scoped API key, and not per-task revocable. It never
enters the VM, but keep it protected. See
[SECURITY.md](https://github.com/sricola/drydock/blob/main/SECURITY.md) for the
full blast-radius note.

**Terms of service.** Headless use of a personal subscription may brush against
the provider's terms and hit rate limits sooner than interactive use. drydock
makes **no claim** that automating a personal Claude or ChatGPT subscription
headlessly is sanctioned by Anthropic or OpenAI. The operator assumes that
risk.

</details>

## Verify

```bash
drydock auth claude --status   # or: drydock auth codex --status
drydock status
```
