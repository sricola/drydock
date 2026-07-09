# drydock docs

Operator documentation for **drydock** — a sandbox for running coding agents
(Claude Code, OpenAI Codex, or any OpenAI-compatible model) on your own repos, on macOS, without trusting
them. Each task runs in a throwaway VM; the agent never sees your real API key,
egress is deny-by-default, and only a diff you approve reaches your code.

New here? Read the [Quickstart](quickstart.html) — install to first task in
about a minute.

## Pages

- **[Quickstart](quickstart.html)** — install, then your first sandboxed task.
- **[Authentication](authentication.html)** — API key or subscription, for Claude Code and Codex.
- **[Bring your own model](models.html)** — run any OpenAI-compatible endpoint (Gemini, OpenRouter, local) via `opencode`.
- **[Submitting tasks](submitting-tasks.html)** — `drydock submit`, the approval gate, flags, and scripting.
- **[Web UI](web-ui.html)** — the board, approval gate, diff, and history in a local browser app.
- **[Run unattended](daemon.html)** — brokerd as a launchd agent: starts at login, restarts on crash; spend-limit caveats.
- **[Egress & widening](egress.html)** — the default allowlist, how enforcement works, and per-task widening.
- **[Configuration](configuration.html)** — `config.yaml` reference and env overrides.
- **[Troubleshooting](troubleshooting.html)** — `drydock doctor` and common failures.
- **[Threat model](threat-model.html)** — what drydock defends, and what it doesn't.

## Requirements

drydock runs on **macOS 26+ on Apple silicon** — it's built on Apple's
`container` runtime and won't run anywhere else. It is pre-1.0
alpha software with no third-party security audit; the [threat
model](threat-model.html) is the contract.
