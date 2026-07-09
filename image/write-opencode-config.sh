#!/usr/bin/env bash
# Write an opencode.json that routes a custom OpenAI-compatible provider through
# the drydock credential gateway. opencode ignores OPENAI_BASE_URL (confirmed by
# the validation spike), so the gateway is wired in as a provider: baseURL
# points at the gateway (+ /v1, which the openai-compatible provider appends
# /chat/completions to), and apiKey carries the per-task bearer the gateway
# validates. Written into the agent's XDG config dir — never /work — so it can't
# land in the captured diff.
#
# Usage: write-opencode-config.sh <gateway-base-url> <model> <xdg-config-home>
set -euo pipefail
# The per-task bearer lands in these files; keep them 0600, not umask-default 0644.
umask 077

GW_BASE="${1:?usage: write-opencode-config.sh <gateway-base-url> <model> <xdg-config-home>}"
MODEL="${2:?usage: write-opencode-config.sh <gateway-base-url> <model> <xdg-config-home>}"
CFG_HOME="${3:?usage: write-opencode-config.sh <gateway-base-url> <model> <xdg-config-home>}"
: "${OPENAI_API_KEY:?missing OPENAI_API_KEY}"

mkdir -p "$CFG_HOME/opencode"
# jq builds the JSON so the model id and the bearer are escaped safely.
jq -n \
  --arg base "${GW_BASE%/}/v1" \
  --arg key "$OPENAI_API_KEY" \
  --arg model "$MODEL" \
  '{
    "$schema": "https://opencode.ai/config.json",
    provider: {
      drydock: {
        npm: "@ai-sdk/openai-compatible",
        name: "drydock gateway",
        options: { baseURL: $base, apiKey: $key },
        models: { ($model): { name: $model } }
      }
    }
  }' > "$CFG_HOME/opencode/opencode.json"
