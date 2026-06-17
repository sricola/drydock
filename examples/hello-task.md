# First task — sandbox smoke

A copy-paste-ready first task. Designed to:

- Verify the whole pipeline end-to-end (stage → claude → diff → approve → push)
- Stay well under the default $2 per-task budget (typical: $0.02 – $0.05)
- Exercise the Python/Go/Node runtimes that ship in the sandbox image so
  you catch any image-layer drift before you need it for real work.

## Setup (one-time)

```bash
# Apple-silicon prerequisites you may not have yet
brew install --cask container
brew install squid
brew install gh   # (or glab / tea if you push to GitLab / Gitea)
gh auth login     # adapter expects this

# drydock
brew install sricola/drydock/drydock
drydock init

# Smoke-test the sandbox before paying for an API call
drydock doctor    # 3 green checks, ~5s
```

If `drydock doctor` is red, fix it before running the task — submit will
fail in the same way and you'll have burned API time on a broken setup.

Create a throwaway GitHub repo you don't mind us pushing a branch to:

```bash
gh repo create $USER/drydock-smoke --public --add-readme
```

## Run the task

In one shell, start brokerd:

```bash
export ANTHROPIC_API_KEY=sk-ant-...    # workspace key with a spend cap
drydock start                          # ^C to stop
```

In another shell, submit:

```bash
drydock submit \
  --repo git@github.com:$USER/drydock-smoke.git \
  --model claude-sonnet-4-6 \
  --instruction "$(cat <<'EOF'
Append a section titled "## toolchain check" to README.md. Inside it,
add three fenced code blocks showing the output of each:

  - python3 -c "import sys; print(sys.version)"
  - node -e "console.log(process.version)"
  - go version

Tag the languages so they render. That is the entire task.
EOF
)"
```

The submit shell blocks until the agent finishes. brokerd fires a macOS
notification when the diff is ready. In your first shell, review and
approve:

```bash
drydock pending                # see the task ID and the requested gate
drydock review <id>            # opens the diff in $PAGER, prompts y/N
```

If you approve, the submit shell unblocks with:

```
task <id>: pushed agent/<id> (github)
```

…and a new branch + PR (or push-only ref) shows up on the throwaway repo.

## What this validates

| Layer | How |
|---|---|
| Container runtime | `drydock init` builds and runs the sandbox |
| Credential gateway | Agent talks to `api.anthropic.com` through it; real key never enters the VM |
| Egress proxy | Agent's `python3` / `node` / `go` commands run locally, no registry calls |
| nft pin | Inside the VM, only the gateway+squid ports are reachable |
| Stage isolation | `.git` is host-only; commit and push run host-side |
| Approval gate | The diff is captured but not pushed until you approve |

## Cleanup

```bash
# Discard the throwaway repo (gh needs the delete_repo scope first)
gh auth refresh -s delete_repo
gh repo delete $USER/drydock-smoke --yes
```

## Cost expectations

Sonnet 4.6 with a 9-turn first-task footprint typically lands around
**$0.02 – $0.05**. You can cap it lower with `--task-budget-usd` in
`~/.drydock/config.yaml`; the gateway rejects further model requests
once the budget is exhausted (current request finishes streaming).
