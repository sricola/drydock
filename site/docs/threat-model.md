# Threat model

drydock's pitch is "run an agent you don't trust," so the security model is the
product. This page is an orientation; the canonical, exhaustive document is
[`THREAT_MODEL.md`](https://github.com/sricola/drydock/blob/main/THREAT_MODEL.md)
in the repo — read it before you trust drydock with anything that matters.

## The stance

Most agent tooling tries to keep the agent *well-behaved* — permission prompts,
output filters, policy. drydock takes the opposite stance: **contain the blast
radius** so a hostile agent (a poisoned repo, a malicious dependency, a
prompt-injection that turns a fetched URL into a shell command) can't reach your
key, your filesystem, your push credentials, or the open internet — regardless
of what it tries.

## What's contained — and the residual risk

| Attack | What drydock does | Residual risk |
|---|---|---|
| **Exfiltrate your API key** | The real key never enters the VM; the agent gets a short-lived, budget-capped token via the gateway. | A leaked token is rate- and budget-limited, and dies with the task. |
| **Call home / exfiltrate code** | Egress is deny-by-default; the agent reaches only your allowlist, through the proxy. | An allow-listed host is still reachable; widening is human-approved per task. |
| **Persist / tamper with the host** | The VM is a throwaway, destroyed after each task; the host filesystem is never mounted writable. | None on the host — any persistence dies with the VM. |
| **Sneak a backdoor into the diff** | Nothing reaches origin until you read the diff and approve it. | drydock makes a malicious diff *reviewable*, not impossible — approve a subtle one and it lands. |

## Prove it yourself

You don't have to take the table's word for it. Every claim is a real `go test`
red-team case that runs the actual attack and asserts it fails. Run them on your
own machine — no API key, no spend, about five minutes:

```bash
drydock redteam        # live containment attacks: key exfil, egress, ephemerality
make demo VM=1         # …or watch all seven, including live VM isolation
```

## Honest limits

A containment that overclaims fails quietly, so the honest edge: drydock makes a
malicious diff *reviewable*, not impossible. It *contains* prompt injection
rather than preventing it. And it trusts the Mac it runs on. It is pre-1.0,
single-maintainer, and has had **no third-party security audit**. The full
[threat model](https://github.com/sricola/drydock/blob/main/THREAT_MODEL.md) and
[SECURITY.md](https://github.com/sricola/drydock/blob/main/SECURITY.md) are the
contract.
