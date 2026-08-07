#!/usr/bin/env bash
# Release QA: black-box gate run against the INSTALLED drydock release
# (brew binaries on PATH), as an operator would use it. Complements
# `make release-preflight`, which tests the source tree; this proves the
# shipped artifact works end to end: CLI contract, daemon lifecycle,
# containment, task lifecycle, and the web UI boundary.
#
# Usage:
#   tests/release/qa.sh                  no-spend gates only (safe, ~6 min)
#   tests/release/qa.sh --live URL       also run the paid task-lifecycle
#                                        phase against URL, a DISPOSABLE
#                                        pushable repo (approve/deny/kill
#                                        branches and history land there)
#   tests/release/qa.sh --daemon        also exercise daemon install/uninstall
#                                        (touches ~/Library/LaunchAgents)
#
# Requires: a configured ~/.drydock (run `drydock setup` first), a valid
# agent credential for --live, and no brokerd already running.
set -u

LIVE_REPO=""
DO_DAEMON=0
while [ $# -gt 0 ]; do
  case "$1" in
    --live)   LIVE_REPO="${2:?--live needs a repo URL}"; shift 2 ;;
    --daemon) DO_DAEMON=1; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

# Fail fast on an unclonable --live repo. The broker clones host-side and
# non-interactively, so a bad URL, or a private https repo on a machine whose
# keychain holds no https credential, would otherwise send every live-phase
# task to "clone failed" and burn the phase's full wait deadlines (~30 min
# observed in the v0.7.0 QA) before reporting. A 2-second ls-remote against
# the exact URL the broker will use catches it up front.
if [ -n "$LIVE_REPO" ]; then
  if ! GIT_TERMINAL_PROMPT=0 git ls-remote "$LIVE_REPO" HEAD >/dev/null 2>&1; then
    echo "--live repo is not clonable non-interactively: $LIVE_REPO" >&2
    echo "(a private https repo needs an https credential in the keychain; try the ssh form: git@github.com:owner/repo)" >&2
    exit 2
  fi
fi

export DRYDOCK_NO_NOTIFY=1
PASS=0; FAIL=0; WARN=0
QA_TMP="$(mktemp -d /tmp/drydock-release-qa.XXXXXX)"
BROKER_PID=""
UI_PID=""

ok()   { PASS=$((PASS+1)); printf '  ok   %-40s %s\n' "$1" "${2:-}"; }
fail() { FAIL=$((FAIL+1)); printf '  FAIL %-40s %s\n' "$1" "${2:-}"; }
warn() { WARN=$((WARN+1)); printf '  warn %-40s %s\n' "$1" "${2:-}"; }
check() { # check <name> <expected-rc> <cmd...>
  local name="$1" want="$2"; shift 2
  "$@" >/dev/null 2>&1
  local got=$?
  if [ "$got" -eq "$want" ]; then ok "$name" "rc=$got"; else fail "$name" "rc=$got want=$want"; fi
}
# wait_until <seconds> <cmd...>: poll a condition (deny/kill resolve async)
wait_until() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -ge $deadline ] && return 1
    sleep 2
  done
}

cleanup() {
  if [ -n "$UI_PID" ]; then kill "$UI_PID" 2>/dev/null; wait "$UI_PID" 2>/dev/null; fi
  if [ -n "$BROKER_PID" ]; then
    # SIGTERM is the tested path: brokerd must reap squid on the way out.
    kill -TERM "$BROKER_PID" 2>/dev/null
    for _ in 1 2 3 4 5; do kill -0 "$BROKER_PID" 2>/dev/null || break; sleep 1; done
    if pgrep -f '.drydock/squid/squid.conf' >/dev/null; then
      echo "  note: squid survived brokerd SIGTERM; cleaning up by hand"
      pkill -f '.drydock/squid/squid.conf'; rm -f "$HOME/.drydock/squid/squid.pid"
    fi
  fi
  # keep the per-task logs around when something failed — they are the
  # only record of what the submits actually printed
  if [ "$FAIL" -gt 0 ]; then
    echo "  logs kept for debugging: $QA_TMP"
  else
    rm -rf "$QA_TMP"
  fi
}
trap cleanup EXIT

echo "drydock release QA ($(drydock version 2>/dev/null || echo 'not installed'))"
echo
echo "[1/6] environment"
check "drydock on PATH"            0 command -v drydock
check "brokerd on PATH"            0 command -v brokerd
check "container runtime running"  0 sh -c 'container system status | grep -q running'
if pgrep -x brokerd >/dev/null; then
  fail "no brokerd already running" "stop it first (or drydock daemon uninstall)"
  echo "aborting: QA needs to own the brokerd lifecycle"; exit 1
else
  ok "no brokerd already running"
fi
# The installed version should match the tag being released.
V_INSTALLED="$(drydock version | awk '{print $2}')"
V_REPO="$(sed -n 's/^## \(v[0-9.]*\).*/\1/p' "$(dirname "$0")/../../CHANGELOG.md" 2>/dev/null | head -1)"
if [ -n "$V_REPO" ] && [ "$V_INSTALLED" = "$V_REPO" ]; then
  ok "installed matches CHANGELOG head" "$V_INSTALLED"
else
  warn "installed vs CHANGELOG head" "installed=$V_INSTALLED changelog=${V_REPO:-?} (fine when validating a prior release)"
fi

echo
echo "[2/6] CLI contract (no daemon, no spend)"
check "version exits 0"            0 drydock version
check "tasks works with broker down" 0 drydock tasks
check "status works with broker down" 0 drydock status
check "pending fails with broker down" 1 drydock pending
check "unknown command is usage error" 2 drydock frobnicate
check "submit without --repo is usage error" 2 drydock submit --instruction x
check "logs on unknown id fails"   1 drydock logs 00000000000000000000000000000000
check "stats parses"               0 drydock stats --since 30d
check "stats rejects bad duration" 1 drydock stats --since bogus
check "retry on unknown id fails"  1 drydock retry 00000000000000000000000000000000
# prune must never touch the real audit dir from QA: exercise it on a copy.
cp -Rp "$HOME/.drydock/audit" "$QA_TMP/audit" 2>/dev/null || mkdir -p "$QA_TMP/audit"
check "prune dry-run"              0 env AUDIT_ROOT="$QA_TMP/audit" drydock prune --older-than 1h
check "prune --yes deletes on copy" 0 env AUDIT_ROOT="$QA_TMP/audit" drydock prune --older-than 8760h --yes

echo
echo "[3/6] doctor + red team (boots VMs, no API spend)"
if drydock doctor; then ok "doctor"; else fail "doctor" "see output above"; fi
if drydock redteam; then ok "redteam A1/A2/A7"; else fail "redteam A1/A2/A7" "CONTAINMENT BREACH: do not release"; fi

echo
echo "[4/6] brokerd lifecycle"
brokerd >"$QA_TMP/brokerd.log" 2>&1 &
BROKER_PID=$!
if wait_until 30 drydock pending; then ok "brokerd starts, socket answers"; else fail "brokerd starts" "$(tail -3 "$QA_TMP/brokerd.log")"; fi
check "healthz via status"         0 sh -c 'drydock status | grep -q "brokerd *up"'
if pgrep -f '.drydock/squid/squid.conf' >/dev/null; then ok "squid running under brokerd"; else fail "squid running under brokerd"; fi

echo
echo "[5/6] web UI boundary"
UI_PORT=7897
drydock ui --port $UI_PORT >"$QA_TMP/ui.log" 2>&1 &
UI_PID=$!
if wait_until 15 grep -q "UI ready" "$QA_TMP/ui.log"; then
  TOKEN="$(sed -n 's|.*#t=\([0-9a-f]*\).*|\1|p' "$QA_TMP/ui.log" | head -1)"
  http() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
  [ "$(http http://127.0.0.1:$UI_PORT/)" = 200 ] \
    && ok "index serves" || fail "index serves"
  [ "$(http http://127.0.0.1:$UI_PORT/api/tasks)" = 403 ] \
    && ok "api without token rejected" || fail "api without token rejected"
  [ "$(http -H "Authorization: Bearer 0000" http://127.0.0.1:$UI_PORT/api/tasks)" = 403 ] \
    && ok "bad token rejected" || fail "bad token rejected"
  [ "$(http -H "Authorization: Bearer $TOKEN" http://127.0.0.1:$UI_PORT/api/tasks)" = 200 ] \
    && ok "good token accepted" || fail "good token accepted"
  [ "$(http -H "Origin: https://evil.example" -H "Authorization: Bearer $TOKEN" http://127.0.0.1:$UI_PORT/api/tasks)" = 403 ] \
    && ok "cross-origin rejected" || fail "cross-origin rejected"
  [ "$(http -H "Host: evil.example" -H "Authorization: Bearer $TOKEN" http://127.0.0.1:$UI_PORT/api/tasks)" = 403 ] \
    && ok "dns-rebind host rejected" || fail "dns-rebind host rejected"
  [ "$(http -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
        -d '{"repo_ref":"git@example.com:x/y.git","instruction":"x","auto_approve":true}' \
        http://127.0.0.1:$UI_PORT/api/submit)" = 400 ] \
    && ok "auto_approve refused by UI" || fail "auto_approve refused by UI"
else
  fail "drydock ui starts" "$(tail -2 "$QA_TMP/ui.log")"
fi
kill "$UI_PID" 2>/dev/null; wait "$UI_PID" 2>/dev/null; UI_PID=""

echo
echo "[6/6] task lifecycle"
if [ -z "$LIVE_REPO" ]; then
  echo "  skip: pass --live <disposable-repo-url> to run the paid phase"
else
  # brokerd resumes awaiting-approval tasks across restarts (by design), and
  # a resumed gate pins the in-flight counters. The live phase needs sole
  # ownership of the queue, and silently killing a resumed task could throw
  # away a real reviewable diff, so make the operator resolve it instead.
  if drydock pending 2>/dev/null | grep -qE '[0-9a-f]{32}'; then
    fail "pending queue empty before live phase" "resolve leftovers first: drydock pending, then approve/deny/kill"
  else
    ok "pending queue empty before live phase"
  fi
  submit_and_get_id() { # submit_and_get_id <logfile> <flags...>
    local log="$1"; shift
    # --json: the human stream truncates the id ("task 8c761593… accepted")
    # until the gate hint, far too late to catch the running stage; the
    # accepted event carries the full task_id within a couple of seconds.
    drydock submit --json --repo "$LIVE_REPO" --platform none "$@" >"$log" 2>&1 &
    local deadline=$((SECONDS+60))
    # NOTE: must test with grep -q first; `grep | head && ...` is a trap
    # (head exits 0 on empty input, so the && fires with no match).
    while [ $SECONDS -lt $deadline ]; do
      if grep -q '"event":"accepted"' "$log"; then
        sed -n 's/.*"task_id":"\([0-9a-f]\{32\}\)".*/\1/p' "$log" | head -1
        return 0
      fi
      sleep 2
    done
    return 1
  }
  gate_open() { drydock pending | grep -q "$1"; }
  gate_gone() { ! drydock pending | grep -q "$1"; }

  # approve path: instruction, gate, approve, branch on remote
  ID=$(submit_and_get_id "$QA_TMP/t1.log" --instruction \
    "Append the line 'release-qa approve marker' to README.md. Change nothing else.")
  if [ -n "$ID" ] && wait_until 600 gate_open "$ID"; then
    ok "task reaches diff gate" "$ID"
    drydock inspect "$ID" >/dev/null 2>&1 && ok "inspect renders trust brief" || fail "inspect renders trust brief"
    drydock approve "$ID" >/dev/null 2>&1 || fail "approve accepted"
    # ground truth is the remote itself, not the client's log format
    if wait_until 120 sh -c "git ls-remote '$LIVE_REPO' 'refs/heads/agent/$ID' | grep -q ."; then
      ok "approved diff pushed to remote" "agent/$ID"
    else
      fail "approved diff pushed to remote" "$(tail -2 "$QA_TMP/t1.log")"
    fi
    grep -q '"type":"metrics"' "$HOME/.drydock/audit/$ID.jsonl" \
      && ok "metrics row written (approve)" || fail "metrics row written (approve)"
  else
    fail "task reaches diff gate" "$(tail -3 "$QA_TMP/t1.log")"
  fi

  # deny path: gate, deny, no branch, diff retained. Deny resolves async.
  ID=$(submit_and_get_id "$QA_TMP/t2.log" --instruction \
    "Append the line 'release-qa deny marker' to README.md. Change nothing else.")
  if [ -n "$ID" ] && wait_until 600 gate_open "$ID"; then
    drydock deny "$ID" >/dev/null 2>&1
    wait_until 30 gate_gone "$ID" && ok "deny resolves gate" "$ID" || fail "deny resolves gate"
    git ls-remote "$LIVE_REPO" "refs/heads/agent/$ID" | grep -q . \
      && fail "denied diff not pushed" "branch exists!" || ok "denied diff not pushed"
    [ -s "$HOME/.drydock/audit/$ID.diff" ] \
      && ok "denied diff retained in audit" || fail "denied diff retained in audit"
    wait_until 30 grep -q '"type":"metrics"' "$HOME/.drydock/audit/$ID.jsonl" \
      && ok "metrics row written (deny)" || fail "metrics row written (deny)"
  else
    fail "deny-path task reaches gate" "$(tail -3 "$QA_TMP/t2.log")"
  fi

  # kill path: kill while running, then full teardown (VM, ACL, stage)
  ID=$(submit_and_get_id "$QA_TMP/t3.log" --instruction \
    "Write a 500 word HISTORY.md about shipyards, then a 500 word DOCKS.md.")
  # Fast tasks can reach the gate before the kill lands; either way the
  # task must end as cancelled. Judge by the task's own result event, not
  # a global counter (other resumed/parallel tasks would poison that).
  if [ -n "$ID" ] && wait_until 300 sh -c "grep -q '\"stage\":\"running\"' $QA_TMP/t3.log"; then
    drydock kill "$ID" >/dev/null 2>&1
    wait_until 60 sh -c "grep -q '\"outcome\":\"cancelled\"' $QA_TMP/t3.log" \
      && ok "kill cancels the task" "$ID" || fail "kill cancels the task" "$(tail -2 "$QA_TMP/t3.log")"
    wait_until 60 sh -c "[ ! -e $HOME/.drydock/stage/$ID ] && ! ls $HOME/.drydock/squid/task-acls/ | grep -q $ID" \
      && ok "stage + squid ACL cleaned" || fail "stage + squid ACL cleaned"
  else
    fail "kill-path task starts running" "$(tail -3 "$QA_TMP/t3.log")"
  fi

  check "stats aggregates fresh runs" 0 sh -c 'drydock stats --since 1h | grep -q "tasks:"'
fi

if [ "$DO_DAEMON" = 1 ]; then
  echo
  echo "[daemon] install/uninstall"
  # QA owns the foreground brokerd; stop it so launchd can bind the socket.
  kill -TERM "$BROKER_PID" 2>/dev/null; wait "$BROKER_PID" 2>/dev/null; BROKER_PID=""
  wait_until 15 sh -c '! pgrep -x brokerd' || fail "foreground brokerd stops"
  pgrep -f '.drydock/squid/squid.conf' >/dev/null \
    && fail "squid reaped on SIGTERM" || ok "squid reaped on SIGTERM"
  check "daemon install" 0 drydock daemon install
  wait_until 30 sh -c 'drydock daemon status | grep -q running' \
    && ok "daemon running under launchd" || fail "daemon running under launchd"
  check "daemon uninstall" 0 drydock daemon uninstall
  wait_until 15 sh -c '! pgrep -x brokerd' \
    && ok "daemon fully stopped" || fail "daemon fully stopped"
fi

echo
echo "passed $PASS, failed $FAIL, warned $WARN"
if [ "$FAIL" -gt 0 ]; then echo "RELEASE QA FAILED"; exit 1; fi
echo "release QA passed. Manual checklist: site/docs/release-qa.md"
