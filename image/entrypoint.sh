#!/usr/bin/env bash
# Root installs the egress firewall, THEN drops privileges to run Claude.
# A non-root agent cannot flush nft, so the firewall holds for the task.
set -euo pipefail
/usr/local/bin/init-firewall.sh /work/.task/allowlist.txt
cd /work
exec gosu agent claude --bare -p "$(cat /work/.task/prompt.txt)" \
     --dangerously-skip-permissions \
     --output-format stream-json --verbose --include-partial-messages
