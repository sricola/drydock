#!/usr/bin/env bash
# Write a Gemini CLI settings.json that (1) pins API-key auth — mandatory because
# @google/gemini-cli 0.49.0 sees GOOGLE_GEMINI_BASE_URL and otherwise auto-selects
# a "gateway" auth type its non-interactive validator rejects (exit 41) — and
# (2) disables every phone-home so the CLI stays within deny-by-default egress
# (it may reach only the drydock gateway + squid). Written under the agent's
# home, never /work, so it can't land in the captured diff.
#
# The key names/nesting were validated against the @google/gemini-cli 0.49.0
# settings schema (docs/reference/configuration.md in that bundle): usage
# stats live under `privacy`, and update phone-home is
# `general.enableAutoUpdate[Notification]` (not a top-level/`checkForUpdates`
# key; those are silently ignored). The Dockerfile's GEMINI_CLI_VERSION has
# moved on since; re-check these keys against the pinned bundle whenever the
# weekly bump PR changes the CLI's settings reference.
#
# Usage: write-gemini-config.sh <gemini-dir>
set -euo pipefail
# settings.json carries only auth-mode and phone-home switches, not the token;
# keep it 0600 anyway rather than umask-default 0644.
umask 077
GEMINI_DIR="${1:?usage: write-gemini-config.sh <gemini-dir>}"
: "${GEMINI_API_KEY:?missing GEMINI_API_KEY}"

mkdir -p "$GEMINI_DIR"
jq -n '{
  security: { auth: { selectedType: "gemini-api-key" } },
  telemetry: { enabled: false },
  privacy: { usageStatisticsEnabled: false },
  general: { enableAutoUpdate: false, enableAutoUpdateNotification: false }
}' > "$GEMINI_DIR/settings.json"
