#!/usr/bin/env bash
# Drop from root to the unprivileged `agent` user with no path back to
# privilege, then exec the given command. This is the sandbox's privilege
# boundary and it is load-bearing: the egress pin (init-firewall.sh) is only
# contained if the agent cannot regain CAP_NET_ADMIN to flush it.
#
# setpriv (not `gosu`, which changes UID only) additionally:
#   --no-new-privs    : a stray SUID binary or file capability can never elevate
#   --bounding-set=-all --inh-caps=-all : empty the capability sets, so even a
#                       re-entry to uid 0 gains no CAP_NET_ADMIN
# Verified by tests/integration TestRedteam_A2_AgentCannotRewriteFirewall:
# as the dropped agent, `nft flush ruleset` returns EPERM and egress stays
# blocked.
exec setpriv --reuid=agent --regid=agent --init-groups \
  --no-new-privs --inh-caps=-all --bounding-set=-all -- "$@"
