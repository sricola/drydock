#!/usr/bin/env bash
# Root installs the egress pin (only the host gateway:8088 and :3128), then
# drops privileges to run Claude. The non-root agent cannot flush nft.
set -euo pipefail
/usr/local/bin/init-firewall.sh "${DRYDOCK_GW_IP:?missing gateway ip}" 8088 3128
cd /work
PROMPT="$(cat /work/.task/prompt.txt)"
AGENT="${DRYDOCK_AGENT:-claude}"

case "$AGENT" in
  codex)
    # The VM is the isolation boundary, so disable codex's own sandbox and
    # approval prompts. DRYDOCK_MODEL (when set) selects the model.
    MODEL_ARGS=()
    if [ -n "${DRYDOCK_MODEL:-}" ]; then
        MODEL_ARGS=(--model "${DRYDOCK_MODEL}")
    fi
    exec gosu agent codex exec \
        --dangerously-bypass-approvals-and-sandbox \
        "${MODEL_ARGS[@]}" \
        "${PROMPT}"
    ;;
  claude)
    MODEL_ARGS=()
    if [ -n "${DRYDOCK_MODEL:-}" ]; then
        MODEL_ARGS=(--model "${DRYDOCK_MODEL}")
    fi
    exec gosu agent claude --bare -p "${PROMPT}" \
        "${MODEL_ARGS[@]}" \
        --dangerously-skip-permissions \
        --output-format stream-json --verbose --include-partial-messages
    ;;
  *)
    echo "drydock: unknown DRYDOCK_AGENT=$AGENT" >&2
    exit 64
    ;;
esac
