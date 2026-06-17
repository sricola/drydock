#!/usr/bin/env bash
# Root installs the egress pin (only the host gateway:8088 and :3128), then
# drops privileges to run Claude. The non-root agent cannot flush nft.
set -euo pipefail
/usr/local/bin/init-firewall.sh "${DRYDOCK_GW_IP:?missing gateway ip}" 8088 3128
cd /work
# Optional model override (broker sets DRYDOCK_MODEL when --model or default_model
# is configured). When unset, claude-code picks its own default.
MODEL_ARGS=()
if [ -n "${DRYDOCK_MODEL:-}" ]; then
    MODEL_ARGS=(--model "${DRYDOCK_MODEL}")
fi
exec gosu agent claude --bare -p "$(cat /work/.task/prompt.txt)" \
     "${MODEL_ARGS[@]}" \
     --dangerously-skip-permissions \
     --output-format stream-json --verbose --include-partial-messages
