#!/usr/bin/env bash
# Write a Gemini CLI settings.json that (1) pins API-key auth — mandatory because
# @google/gemini-cli 0.49.0 sees GOOGLE_GEMINI_BASE_URL and otherwise auto-selects
# a "gateway" auth type its non-interactive validator rejects (exit 41) — and
# (2) disables every phone-home so the CLI stays within deny-by-default egress
# (it may reach only the drydock gateway + squid). Written under the agent's
# home, never /work, so it can't land in the captured diff.
#
# Usage: write-gemini-config.sh <gemini-dir>
set -euo pipefail
GEMINI_DIR="${1:?usage: write-gemini-config.sh <gemini-dir>}"
: "${GEMINI_API_KEY:?missing GEMINI_API_KEY}"

mkdir -p "$GEMINI_DIR"
jq -n '{
  security: { auth: { selectedType: "gemini-api-key" } },
  telemetry: { enabled: false },
  usageStatisticsEnabled: false,
  general: { checkForUpdates: false }
}' > "$GEMINI_DIR/settings.json"
