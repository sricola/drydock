#!/usr/bin/env bash
# nft default-deny output; allow egress ONLY to the host gateway IP on the
# gateway+proxy ports. No DNS, nothing else. Run as ROOT pre-priv-drop.
set -euo pipefail
GW="${1:?usage: init-firewall.sh <gateway-ip> <port> [<port>...]}"
shift
nft flush ruleset
nft add table inet fw
nft add chain inet fw out '{ type filter hook output priority 0; policy drop; }'
nft add rule inet fw out ct state established,related accept
nft add rule inet fw out oifname "lo" accept
for port in "$@"; do
  nft add rule inet fw out ip daddr "$GW" tcp dport "$port" accept
done
