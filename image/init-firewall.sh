#!/usr/bin/env bash
# nft default-deny output policy; allow DNS + allowlisted domains' IPs on :443.
# Run as ROOT in the entrypoint BEFORE dropping to the non-root agent.
set -euo pipefail
ALLOW="${1:?usage: init-firewall.sh <allowlist-file>}"
nft flush ruleset
nft add table inet fw
nft add chain inet fw out '{ type filter hook output priority 0; policy drop; }'
nft add rule inet fw out ct state established,related accept
nft add rule inet fw out oifname "lo" accept
nft add rule inet fw out udp dport 53 accept
nft add rule inet fw out tcp dport 53 accept
nft add set inet fw allow4 '{ type ipv4_addr; flags interval; }'
while read -r host port; do
  [ -z "${host:-}" ] && continue
  for ip in $(getent ahostsv4 "$host" | awk '{print $1}' | sort -u); do
    nft add element inet fw allow4 "{ $ip }"
  done
done < "$ALLOW"
nft add rule inet fw out ip daddr @allow4 tcp dport '{ 443 }' accept
