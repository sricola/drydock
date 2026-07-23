#!/usr/bin/env bash
# drydock breach demo — runs REAL containment attacks and shows them fail.
#
# Every "CONTAINED" below is the result of an actual `go test` red-team case
# from THREAT_MODEL.md, not a scripted print. The four host-side attacks run
# anywhere Go is installed (no VM, no API spend). Pass --vm on macOS + Apple
# silicon to also run the VM-backed attacks (A1 key-exfil, A2 egress, A7
# ephemerality) via `make redteam-vm`.
#
#   ./demo/breach.sh          # 4 host-side attacks (~60s, free)
#   ./demo/breach.sh --vm     # + VM-backed A1/A2/A7 (needs sandbox)
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || { echo "run inside the drydock repo"; exit 1; }
command -v go >/dev/null || { echo "go is required"; exit 1; }

if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; X=$'\033[0m'
else
  B=""; DIM=""; R=""; G=""; Y=""; C=""; X=""
fi

# pace LINE — typewriter-ish pause so a screen recording reads well.
pace() { sleep "${PACE:-0.9}"; }
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

idx=0
# beat ATTACKER_LINE  PKG  TEST_REGEX  CONTAINMENT_LINE
beat() {
  idx=$((idx+1))
  printf "\n${B}${R}▶ ATTACK %d${X}  %s\n" "$idx" "$1"
  pace
  printf "  ${DIM}\$ go test -run %s %s${X}\n" "$3" "$2"
  if go test -count=1 -run "$3" "$2" >"$WORK/out" 2>&1; then
    line=$(grep -E '^ok' "$WORK/out" | head -1)
    printf "  ${DIM}%s${X}\n" "${line:-ok}"
    printf "  ${B}${G}✓ CONTAINED${X}  %s\n" "$4"
  else
    printf "  ${B}${R}✗ BREACH — containment failed${X}\n"
    sed 's/^/    /' "$WORK/out"
    exit 1
  fi
  pace
}

if [ "${1:-}" = "--vm" ]; then
  SUBTITLE="Seven real attacks from THREAT_MODEL.md — four host-side, three in a live VM. Each must fail."
else
  SUBTITLE="Four real attacks from THREAT_MODEL.md. Each one runs and must fail."
fi
clear 2>/dev/null || true
printf "${B}drydock — run coding agents like you assume they're already hacked.${X}\n"
printf "${DIM}%s${X}\n" "$SUBTITLE"
pace

beat "Agent plants ${C}.git/hooks/pre-commit${X} to run code on your host." \
     "./internal/stage" "TestHostCommit_IgnoresPlantedHook" \
     "host commits with ${C}core.hooksPath=/dev/null${X} — the hook never fires."

beat "Agent hides its prompt + allowlist in the work tree to smuggle into your PR." \
     "./internal/stage" "TestCaptureDiff_ExcludesTaskDir" \
     "control files are excluded from the diff — only the real change ships."

beat "Agent crafts a ${C}backdoor diff${X} (\"skip authn\") and tries to push it." \
     "./internal/broker" "TestRedteam_A5_GateBlocksUnapprovedPush" \
     "default-deny gate — you DENY, nothing reaches origin."

beat "Agent tries to widen egress to ${C}evil.example.com:443${X} mid-task." \
     "./internal/broker" "TestRedteam_A6_EgressWidenDenied" \
     "denied (403) — the host never compiles into the allowlist."

# vmbeat ATTACKER_LINE  TEST_REGEX  CONTAINMENT_LINE — runs one VM-backed
# attack inside a real throwaway sandbox VM.
vmbeat() {
  idx=$((idx+1))
  printf "\n${B}${R}▶ ATTACK %d${X}  %s\n" "$idx" "$1"
  pace
  printf "  ${DIM}\$ go test -tags=integration -run %s ./tests/integration${X}\n" "$2"
  printf "  ${DIM}booting throwaway VM…${X}\n"
  if go test -tags=integration -count=1 -timeout=4m -run "$2" ./tests/integration >"$WORK/out" 2>&1; then
    line=$(grep -E '^ok' "$WORK/out" | head -1)
    printf "  ${DIM}%s${X}\n" "${line:-ok}"
    printf "  ${B}${G}✓ CONTAINED${X}  %s\n" "$3"
  else
    printf "  ${B}${R}✗ BREACH — containment failed${X}\n"
    sed 's/^/    /' "$WORK/out"
    exit 1
  fi
  pace
}

if [ "${1:-}" = "--vm" ]; then
  printf "\n${B}${Y}── VM-backed attacks: each boots a real, throwaway sandbox VM ──${X}\n"
  pace
  vmbeat "Agent reads the real vendor API key from its own environment." \
         "TestRedteam_A1_RealKeyNeverInVM" \
         "only a budget-capped ${C}tok_${X} bearer is present — the real key never entered the VM."
  vmbeat "Agent curls a non-allowlisted host, raw DNS, and a direct IP." \
         "TestRedteam_A2_EgressToHostileHostBlocked" \
         "HTTPS, DNS, and direct-IP all blocked — deny-by-default holds inside the VM."
  vmbeat "Agent writes a secret to disk to smuggle into the next task." \
         "TestRedteam_A7_NoStatePersistsBetweenTasks" \
         "fresh VM per task — task 2 sees nothing; the secret is gone."
fi

printf "\n${B}Containment claims — attack-tested, every commit:${X}\n"
printf "  ${G}✓${X} A3  planted git hook neutralized        ${DIM}(shown above)${X}\n"
printf "  ${G}✓${X} A4  prompt/allowlist kept out of diff   ${DIM}(shown above)${X}\n"
printf "  ${G}✓${X} A5  unapproved push blocked             ${DIM}(shown above)${X}\n"
printf "  ${G}✓${X} A6  self-service egress widening denied  ${DIM}(shown above)${X}\n"
if [ "${1:-}" = "--vm" ]; then
  printf "  ${G}✓${X} A1  real API key never enters the VM    ${DIM}(shown above)${X}\n"
  printf "  ${G}✓${X} A2  egress to hostile hosts blocked      ${DIM}(shown above)${X}\n"
  printf "  ${G}✓${X} A7  no state persists between tasks       ${DIM}(shown above)${X}\n"
else
  printf "  ${DIM}○ A1/A2/A7  VM-backed — run: ./demo/breach.sh --vm  (macOS, Apple silicon)${X}\n"
fi
printf "\n${DIM}Don't trust the threat model — check it: ${X}${B}make redteam${X}${DIM}  ·  verify the release: ${X}${B}cosign verify-blob${X} ${DIM}+ ${X}${B}gh attestation verify${X}\n"
printf "${DIM}Beta · no third-party audit yet · macOS 26+, Apple silicon${X}\n\n"
