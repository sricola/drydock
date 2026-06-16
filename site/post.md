# Running Claude Code unattended without giving it your API key

*A working design for containing an autonomous coding agent on macOS.
Hardware-isolated VMs per task, a credential gateway so the model key
never enters the sandbox, deny-by-default egress, and a diff-only return
path. Code: [github.com/sricola/drydock](https://github.com/sricola/drydock).*

---

You can run Claude Code (or any other LLM coding agent) unattended. You
probably shouldn't, today, on a Mac that has your real `ANTHROPIC_API_KEY`
in its environment, full network access, and write permission on every
repo you own. The default failure mode of an autonomous agent isn't
"steals the world." It's "writes a plausible-looking diff that happens to
include a Bash one-liner that exfiltrates your `.zshrc`," and you don't
notice because the diff is otherwise fine.

The reflex is to add prompts and review gates inside the agent loop:
`--dangerously-skip-permissions=false`, "are you sure?", a system prompt
that says don't do bad things. These are useful and inadequate. They live
inside the trust boundary they're supposed to defend.

The better reflex is to assume the agent is compromised — every call —
and design a containment that makes the assumption cheap. This post
describes that design. The reference implementation, **drydock**, runs on
Apple silicon using Apple's new `container` runtime and is open source.

## What "compromised" means in practice

The agent runs in a loop that reads files, runs commands, writes patches,
and talks to an LLM. Each step of that loop is a place where an attacker
gets to influence the agent's behavior: a hostile `AGENTS.md` in a
dependency, a prompt-injection in a fetched URL, a sufficiently weird
combination of tool outputs that nudges the model into doing something
useful for the attacker and surprising to you.

You don't need a sophisticated attacker. Most of the danger is from
ordinary buggy or under-specified prompts: the agent decides that "fix
the test" means installing seven npm packages and writing a `postinstall`
hook. It's not malicious, but the effect is the same.

So treat the agent's process as untrusted. Treat its outputs as
untrusted. Make the containment so cheap that you can let the agent run
overnight without flinching, and so loud that it can't escape it
quietly.

## The three claims drydock makes

A containment is only as good as the claims you can make precisely. Here
are drydock's:

1. **The model key never enters the sandbox.** The host holds the key.
   The sandbox gets a short-lived bearer token bound to a USD budget.
2. **Network egress is deny-by-default.** The sandbox can reach exactly
   two host endpoints: a credential gateway for Anthropic, and a
   userspace squid for registry traffic. Both have hostname allowlists.
   DNS itself is dropped.
3. **The only thing the sandbox can deliver to your host is a diff, and
   no diff reaches origin without your approval.** Git's `.git`
   directory is never mounted into the sandbox. Hooks are neutralized.
   The host pushes; the sandbox doesn't have credentials to.

The rest of this post is how each claim is enforced.

## Claim 1: the model key never enters the sandbox

This is the core design move and the most transferable idea in the post.

The naive way to give a sandbox API access is to copy `ANTHROPIC_API_KEY`
into the container's environment. That works until the agent decides to
print the environment (it will, eventually), or pipes it into a fetch
call, or writes it into a log file that ends up in the diff.

Instead, run a tiny HTTP service on the host — the **credential
gateway** — and point the sandbox's `ANTHROPIC_BASE_URL` at it. The
gateway holds the real key. The sandbox authenticates with a bearer
token the gateway mints per task, scoped to a USD budget and a TTL. Even
if the agent exfiltrates the bearer, it expires when the task ends and
caps at $2 (or whatever you set) along the way.

```
sandbox VM ──ANTHROPIC_AUTH_TOKEN=<bearer>──▶ gateway ──ANTHROPIC_API_KEY=<real>──▶ api.anthropic.com
                                              │
                                              ├─ verify token, check budget
                                              ├─ proxy request, parse usage
                                              └─ deduct cost, revoke if exhausted
```

The pattern transfers cleanly to any other agent stack. It's not specific
to Anthropic. The point is to make the key a host-only secret that the
sandbox never copies into its own state.

## Claim 2: deny-by-default egress, on two tracks

Sandbox egress works in two lanes, both pinned to a specific gateway IP:

| Lane | Port | Used by | Auth |
|------|------|---------|------|
| credential gateway | 8088 | model API calls | bearer token, budget-bound |
| squid forward proxy | 3128 | npm/pip & other allowlisted HTTPS | none; hostname allowlist |

Everything else is dropped. Inside the VM, an `nft` ruleset installs at
boot (as root, before dropping privileges to the agent user) that
denies all output except loopback, established/related, and the two
allowed `(gateway-IP, port)` pairs. The agent user has no
`CAP_NET_ADMIN`, so it cannot rewrite the rules.

DNS being dropped is a deliberate design choice. Squid resolves names on
the host on the sandbox's behalf, for hosts the operator allowlisted.
The sandbox has no other way to name anything — `curl example.com`
fails at resolution, not at the firewall, which is actually a tighter
failure mode.

The allowlist lives in a single YAML file:

```yaml
default:
  domains:
    - { host: api.anthropic.com,      ports: [443] }   # via gateway
    - { host: registry.npmjs.org,     ports: [443] }   # via squid
    - { host: pypi.org,               ports: [443] }   # via squid
    - { host: files.pythonhosted.org, ports: [443] }   # via squid
per_task_widening:
  requires_approval: true
```

A task can request additional egress, but the widening is gated.

## Claim 3: diff-only return, human-gated push

The sandbox writes files. The host commits and pushes. The pieces never
mix.

When a task starts, drydock clones the repo on the host, **moves the
`.git` directory out of the mounted path**, and bind-mounts only the
work tree into the VM. This matters more than it sounds: an agent that
plants a `.git/hooks/pre-commit` script in the work tree never gets it
to run on the host, because the host-side `git` operations use
`--git-dir=<separated>` plus `core.hooksPath=/dev/null` and
`core.fsmonitor=false`. We caught the hooks vulnerability in an
adversarial review and added a regression test, `TestHostCommit_IgnoresPlantedHook`,
that simulates the attack and asserts the planted hook never executed.

When the task exits, drydock captures the diff (excluding a `.task/`
control directory that contains the agent prompt and the compiled
allowlist), persists it to disk, and **blocks the original `POST /tasks`
request until a human reviews it**. The pending diff shows up in
`drydock pending`. A reviewer reads it (it's just a diff; you already
know how to do this), runs `drydock approve <id>` or `drydock deny
<id>`, and the original request unblocks. Only on approval does the host
push the branch and open the PR.

There's a `"auto_approve": true` escape hatch for trusted batch runs.
The threat model is explicit about what you're opting out of when you
use it.

## Honest limitations

If you only remember one section, make it this one. Containments fail
quietly when they overclaim.

- **You must review every diff before approving.** The gate makes review
  possible, not automatic. A subtle backdoor in 200 lines of plausible
  Python will land if you click through.
- **Host compromise defeats every claim above.** drydock assumes the
  host is trusted. Malware on your laptop reads keys, mounts sockets,
  rewrites binaries. Out of scope.
- **Apple `container` is v1.0.** A guest-to-host escape ends the story.
  We pin a tested version, watch upstream advisories, and call out
  flag drift loudly (we've already hit `--user` and mount-option
  parsing changes inside 1.0.x).
- **Prompt injection from the staged repo isn't filtered.** A hostile
  `AGENTS.md` or docstring can influence the agent for that task. The
  diff gate is the backstop.
- **Side-channels in the diff itself are possible.** A clever agent
  can encode information in file names, whitespace, or comment text.
  drydock makes this *visible* at review; it does not detect it.
- **The host's GitHub credentials are not isolated.** Whoever can run
  `drydock approve` can push to anything `gh` can reach.

There is a one-page [threat model](https://github.com/sricola/drydock/blob/main/THREAT_MODEL.md)
that is precise about each of these. Read it before deciding whether
this design is good enough for your workload.

## Why macOS-native

Linux has firejail, podman, kata, gVisor, and a dozen other
sandboxing primitives. The reason drydock targets macOS specifically is
that the audience for "I want to leave an agent running unattended on my
work laptop" is overwhelmingly Apple silicon. They're the people who
need this, and they're the people Apple's `container` runtime — released
this year — finally makes well-served. drydock isn't trying to be Docker.
It's trying to be the layer that turns `container` + `claude-code` into
something you'd trust overnight.

## What's next

Two things would meaningfully widen drydock's reach:

1. **Slack / web approval adapters.** The CLI is fine for solo work; a
   real on-call would want the approval prompt to land in Slack with the
   diff inline. The same `/admin/approve` endpoint that powers the CLI
   is the seam.
2. **Bitbucket PR opening.** Push works through the push-only adapter;
   PR creation needs a small REST integration since Bitbucket has no
   widely-adopted shell-CLI to wrap.

Neither is research; they're commit-shaped. If either is the thing
standing between you and using this, the issue tracker is where to say
so.

## Try it

```bash
brew install --cask container
brew install sricola/drydock/drydock
export ANTHROPIC_API_KEY=sk-ant-...
drydock init
drydock start
```

The `drydock init` step creates `~/.drydock/` (config + egress YAML),
fetches the sandbox + anchor images, and wires up the vmnet network.
The [README](https://github.com/sricola/drydock/blob/main/README.md)
walks the full operator surface; the
[threat model](https://github.com/sricola/drydock/blob/main/THREAT_MODEL.md)
is the precise contract.

drydock is open source under [MIT](https://github.com/sricola/drydock/blob/main/LICENSE).
The credential-gateway pattern in particular is portable — if you're
building any kind of autonomous-agent system and want one well-defined
boundary between the agent and your real keys, take that piece and use
it. The rest of drydock is a particular implementation; the gateway is
the idea.

---

*Open to design critiques. The threat model is the file to argue with.*
