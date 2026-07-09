#!/usr/bin/env bash
# nft default-deny in ALL directions; allow egress ONLY to the host gateway IP
# on the gateway+proxy ports. No DNS, nothing else. Run as ROOT pre-priv-drop.
#
# The whole ruleset is applied in a single atomic `nft -f` transaction (flush +
# rebuild together), so a mid-build failure can never leave the VM with a
# partial or empty (fail-open) ruleset — nft rejects the transaction as a whole
# and set -e aborts the entrypoint before the agent ever runs. input/forward
# default-drop too, so the agent can't be reached from siblings on the network.
set -euo pipefail
GW="${1:?usage: init-firewall.sh <gateway-ip> <port> [<port>...]}"
shift

allow=""
for port in "$@"; do
  allow+="    ip daddr ${GW} tcp dport ${port} accept"$'\n'
done

nft -f - <<EOF
flush ruleset
table inet fw {
  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    iifname "lo" accept
  }
  chain forward {
    type filter hook forward priority 0; policy drop;
  }
  chain output {
    type filter hook output priority 0; policy drop;
    ct state established,related accept
    oifname "lo" accept
${allow}  }
}
EOF
