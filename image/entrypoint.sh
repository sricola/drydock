#!/usr/bin/env bash
# Root installs the egress pin (only the host gateway:8088 and :3128), then
# drops privileges to run Claude. The non-root agent cannot flush nft.
set -euo pipefail
/usr/local/bin/init-firewall.sh "${DRYDOCK_GW_IP:?missing gateway ip}" 8088 3128
cd /work
exec gosu agent claude --bare -p "$(cat /work/.task/prompt.txt)" \
     --dangerously-skip-permissions \
     --output-format stream-json --verbose --include-partial-messages
