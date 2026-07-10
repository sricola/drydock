# Quickstart

Install drydock and run your first sandboxed task in about a minute. The agent
runs full-throttle in a throwaway VM; the only thing that comes back is a
`git diff` you approve.

> **Requires macOS 26+ on Apple silicon.** drydock runs on Apple's `container`
> runtime and won't run anywhere else.

## 1. Install

```bash
brew install sricola/drydock/drydock
drydock setup     # installs the container runtime + squid, then initializes ~/.drydock
```

`drydock setup` is idempotent and walks you through first-run configuration
(agent choice, auth mode). See [Authentication](authentication.html) for the
full matrix.

## 2. Give it a credential

The quickest path is a vendor API key. It stays **host-side** and never enters
the VM: the sandbox only ever sees a short-lived, budget-capped token.

```bash
export ANTHROPIC_API_KEY=sk-ant-...    # Claude Code tasks
# or
export OPENAI_API_KEY=sk-...           # OpenAI Codex tasks
```

Already pay for Claude Pro/Max or ChatGPT? You can [use your subscription
instead](authentication.html), no API key required.

## 3. Start the broker

```bash
drydock start     # foreground; ^C to stop
```

Prefer it always-on? [Run unattended](daemon.html) installs brokerd as a
launchd agent (read the spend-cap caveat first).

Check it's up:

```bash
drydock status
# brokerd     up
# in flight   0 running · 0 awaiting egress · 0 awaiting diff · 0 pushing
```

## 4. Submit a task

In another shell, point it at a repo and a task. It **blocks until the agent
runs and you approve the diff**:

```bash
drydock submit \
  --repo git@github.com:your-org/your-repo \
  --instruction "Add a one-line comment to README.md explaining the project."
```

The submit shell streams progress, then pauses at the approval gate:

```
task ab12cd34 accepted
  preparing · cloning repo
  running · claude working
  ⏸ awaiting approval · 1.2 KB diff (4 files)
     approve: drydock approve ab12cd34     review: drydock review ab12cd34
✓ pushed agent/ab12cd34 (github) · 4 files +120/-8 · 2m18s · $0.11
```

## 5. Review and approve

```bash
drydock review ab12cd34    # opens the diff in $PAGER, then prompts y/N
# … or step by step:
drydock approve ab12cd34   # or: drydock deny ab12cd34
```

Nothing reaches your real repo until you approve. That's the whole loop.

## Next

- [Submitting tasks](submitting-tasks.html): agents, models, flags, scripting.
- [Egress & widening](egress.html): what the sandbox can reach, and how to widen it.
- [Threat model](threat-model.html): what this actually protects you from.
