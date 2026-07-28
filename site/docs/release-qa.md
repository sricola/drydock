# Release QA (installed-artifact gate)

`make release-preflight` proves the source tree: unit suite, host red team,
VM red team. It does not prove the thing operators actually run: the
installed release (brew binaries, embedded web UI, launchd plist, squid
wiring). Release QA closes that gap with a black-box pass over the
installed `drydock` and `brokerd`, exactly as an operator would drive them.

Run it before every release, after installing the candidate build:

```bash
# no-spend gates: environment, CLI contract, doctor, red team,
# brokerd lifecycle, web UI auth boundary (~6 min)
tests/release/qa.sh

# everything, including the paid task-lifecycle phase and the
# launchd daemon round trip
tests/release/qa.sh --live git@github.com:you/some-disposable-repo.git --daemon
```

The script exits non-zero if any check fails, and prints a doctor-style
`ok/FAIL` line per check.

## What the phases cover

1. **Environment**: binaries on PATH, container runtime up, installed
   version matches the CHANGELOG head, no brokerd already running (QA
   owns the daemon lifecycle for the run).
2. **CLI contract** (no daemon, no spend): read-only commands work with
   brokerd down (`tasks`, `status`, `stats`), commands that need the
   broker fail cleanly (`pending`), usage errors exit 2, runtime errors
   exit 1, and `prune` is exercised (dry run and `--yes`) against a
   throwaway copy of the audit dir, never the real one.
3. **Doctor and red team**: `drydock doctor` (sandbox boot, all four
   agent CLIs, egress pin, credentials, PR tooling) and
   `drydock redteam` (A1 key isolation, A2 egress, A7 state persistence).
   A red-team failure is a containment breach: do not release.
4. **brokerd lifecycle**: foreground start answers on the socket, squid
   comes up under it, and (in `--daemon` mode) SIGTERM reaps squid on
   the way out. Note that only clean shutdown is covered: a SIGKILLed
   brokerd still strands squid, which is why the script cleans up by
   hand if the reap fails.
5. **Web UI boundary**: the token-gated loopback server rejects missing
   and wrong bearer tokens, cross-origin requests, and DNS-rebinding
   Host headers (all 403), refuses `auto_approve` from the UI (400), and
   accepts the minted token (200).
6. **Task lifecycle** (`--live`, real agent runs, real pushes): a task
   reaches the diff gate; `inspect` renders the trust brief; approve
   pushes `agent/<id>` to the remote and writes a metrics row; deny
   resolves the gate (asynchronously, the script polls), pushes nothing,
   retains the diff, and still writes a metrics row; kill mid-run tears
   down the VM and cleans the stage dir and per-task squid ACL; `stats`
   picks up the fresh runs. Point `--live` at a disposable repo you can
   push to: QA branches and history land there. Local paths are
   rejected by design, so it must be a real https/git/ssh URL.

## Manual checklist (browser, ~5 min)

The auth boundary and API are covered above; these are the visual and
interaction paths that only a human in a browser can judge. Start
`drydock ui --open` with a task pending review (the `--live` phase
leaves history to look at):

- [ ] Board shows running tasks with stage badges (`egress?`, `running`,
      `review?`, `pushing`) and cards update without a reload.
- [ ] The review modal renders the diff hunks and the Logs tab shows a
      readable narration of the run.
- [ ] Deny and Kill use a two-press confirm: first press turns the
      button into `Confirm?`; press it again to fire. Confirm the armed
      state survives a board poll tick (1.5s, 500ms while a gate is
      open): wait a couple of poll cycles between the two presses and
      the button should still read `Confirm?`, not have reset.
- [ ] Keyboard path works: R opens review, A approves, D twice denies.
- [ ] Approving from the modal pushes; the card moves to `pushing` and
      then lands in "Just finished".
- [ ] History view lists past runs with outcomes.
- [ ] macOS notification arrives when a task hits the review gate (run
      one task without `DRYDOCK_NO_NOTIFY`).

## Placement in the release flow

Release QA slots between the brew bump and announcing the release:

1. `make tag-release VERSION=vX.Y.Z` (source-tree gate, tags, CI builds)
2. Bump the Homebrew formula, then `brew upgrade drydock`
3. `tests/release/qa.sh --live <disposable repo> --daemon` against the
   installed build
4. Announce
