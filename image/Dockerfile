FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset gosu \
 && rm -rf /var/lib/apt/lists/*
# Pin the agent CLI. Bump deliberately — every drydock release is
# anchored to a known claude-code version so the threat-model claims
# track behavior we've actually tested. Override at build time with
# --build-arg CLAUDE_CODE_VERSION=x.y.z.
ARG CLAUDE_CODE_VERSION=2.1.177
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}
RUN useradd -m -u 10001 -s /bin/bash agent
COPY init-firewall.sh /usr/local/bin/init-firewall.sh
COPY entrypoint.sh    /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/init-firewall.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
