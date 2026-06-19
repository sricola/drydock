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
    # codex ignores OPENAI_BASE_URL — it routes by its own model_provider
    # config. Point a provider at the drydock credential gateway so the agent
    # never talks to api.openai.com directly (deny-by-default egress would
    # block it anyway). The grant injects OPENAI_BASE_URL=http://<gw>:8088 and
    # OPENAI_API_KEY=<per-task bearer token>; codex's provider base_url wants
    # the /v1 suffix (it appends /responses), and env_key tells it to send the
    # token as the bearer the gateway validates.
    export CODEX_HOME=/home/agent/.codex
    /usr/local/bin/write-codex-config.sh "${OPENAI_BASE_URL:?missing OPENAI_BASE_URL}" "$CODEX_HOME"
    chown -R agent:agent "$CODEX_HOME"
    # The VM is the isolation boundary, so disable codex's own sandbox and
    # approval prompts. DRYDOCK_MODEL (when set) selects the model.
    MODEL_ARGS=()
    if [ -n "${DRYDOCK_MODEL:-}" ]; then
        MODEL_ARGS=(--model "${DRYDOCK_MODEL}")
    fi
    exec gosu agent env "CODEX_HOME=$CODEX_HOME" codex exec \
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
