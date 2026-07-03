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
  opencode)
    # opencode ignores OPENAI_BASE_URL (validation spike) — it needs a config
    # file. Write one pointing a custom openai-compatible provider at the drydock
    # gateway, with the per-task bearer (OPENAI_API_KEY) as apiKey. Keep
    # opencode's config + state OUT of /work so they never pollute the diff.
    : "${OPENAI_BASE_URL:?missing OPENAI_BASE_URL}"
    MODEL="${DRYDOCK_MODEL:?opencode needs a model — set openai_compat.model or pass --model}"
    export XDG_CONFIG_HOME=/home/agent/.config
    export XDG_DATA_HOME=/home/agent/.local/share
    /usr/local/bin/write-opencode-config.sh "$OPENAI_BASE_URL" "$MODEL" "$XDG_CONFIG_HOME"
    mkdir -p "$XDG_DATA_HOME"
    chown -R agent:agent /home/agent/.config /home/agent/.local
    # The VM is the isolation boundary, so auto-approve all permissions. JSON
    # output gives the operator stream machine-readable events. The apiKey lives
    # in the written config, so opencode (as agent) needs no gateway env itself.
    exec gosu agent env \
        "HOME=/home/agent" \
        "XDG_CONFIG_HOME=$XDG_CONFIG_HOME" \
        "XDG_DATA_HOME=$XDG_DATA_HOME" \
        opencode run "${PROMPT}" -m "drydock/${MODEL}" \
        --dangerously-skip-permissions --format json
    ;;
  gemini)
    # The gateway injects GOOGLE_GEMINI_BASE_URL + GEMINI_API_KEY (per-task
    # bearer). The CLI (API-key mode) sends the bearer in x-goog-api-key; the
    # gateway admits it and swaps in the real key. A settings.json pinning
    # api-key auth is MANDATORY (env alone makes 0.49.0 pick a rejected auth
    # type); it also disables phone-home so the CLI stays within egress limits.
    : "${GOOGLE_GEMINI_BASE_URL:?missing GOOGLE_GEMINI_BASE_URL}"
    : "${GEMINI_API_KEY:?missing GEMINI_API_KEY}"
    MODEL="${DRYDOCK_MODEL:-gemini-2.5-pro}"
    export GEMINI_DIR=/home/agent/.gemini
    /usr/local/bin/write-gemini-config.sh "$GEMINI_DIR"
    chown -R agent:agent "$GEMINI_DIR"
    # VM is the isolation boundary, so:
    #   --approval-mode yolo : auto-approve ALL tool calls (edits/writes/shell).
    #     Without this the CLI's default "prompt for approval" mode blocks every
    #     file edit in headless -p mode (no TTV) — the task would produce no diff
    #     and never reach the push gate. This is the gemini-cli analogue of
    #     claude's --dangerously-skip-permissions / codex's approvals bypass.
    #   --skip-trust : clears the separate workspace-trust prompt.
    # GOOGLE_GENAI_USE_VERTEXAI=false forces the Gemini API (not Vertex), so
    # traffic stays on GOOGLE_GEMINI_BASE_URL (the gateway) — required by the
    # validated spike setup.
    exec gosu agent env \
        "HOME=/home/agent" \
        "GEMINI_DIR=$GEMINI_DIR" \
        "GOOGLE_GEMINI_BASE_URL=$GOOGLE_GEMINI_BASE_URL" \
        "GEMINI_API_KEY=$GEMINI_API_KEY" \
        "GOOGLE_GENAI_USE_VERTEXAI=false" \
        "GEMINI_CLI_TRUST_WORKSPACE=true" \
        gemini -p "${PROMPT}" -m "${MODEL}" --approval-mode yolo --skip-trust
    ;;
  *)
    echo "drydock: unknown DRYDOCK_AGENT=$AGENT" >&2
    exit 64
    ;;
esac
