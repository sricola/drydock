#!/usr/bin/env bash
# Write a codex config.toml that routes the OpenAI provider through the drydock
# credential gateway. codex ignores OPENAI_BASE_URL, so the gateway has to be
# wired in as a model_provider: base_url points at the gateway (+ /v1, which
# codex appends /responses to), and env_key tells codex to send the per-task
# bearer token (OPENAI_API_KEY) as the credential the gateway validates.
#
# Usage: write-codex-config.sh <gateway-base-url> <codex-home>
set -euo pipefail

GW_BASE="${1:?usage: write-codex-config.sh <gateway-base-url> <codex-home>}"
CODEX_HOME="${2:?usage: write-codex-config.sh <gateway-base-url> <codex-home>}"

mkdir -p "$CODEX_HOME"
cat > "$CODEX_HOME/config.toml" <<EOF
model_provider = "drydock"

[model_providers.drydock]
name = "drydock gateway"
base_url = "${GW_BASE%/}/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
EOF
