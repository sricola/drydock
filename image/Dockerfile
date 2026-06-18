FROM node:22-bookworm-slim
# Base toolchain + Python 3.11 (Debian default). Python lives next to Node so
# the egress allowlist's pypi.org slot has a runtime to land on.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset gosu \
      python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*

# Go: Debian bookworm ships 1.19 which is too old to be useful; install the
# upstream tarball matching the build arch. Adds ~400MB to the image but the
# alternative is no Go at all.
ARG GO_VERSION=1.26.4
RUN ARCH=$(dpkg --print-architecture) && \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" \
        | tar -C /usr/local -xz && \
    ln -s /usr/local/go/bin/go    /usr/local/bin/go && \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Pin the agent CLI. Bump deliberately — every drydock release is
# anchored to a known claude-code version so the threat-model claims
# track behavior we've actually tested. Override at build time with
# --build-arg CLAUDE_CODE_VERSION=x.y.z.
ARG CLAUDE_CODE_VERSION=2.1.177
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

# Pin the Codex CLI alongside claude-code. Bump deliberately; every drydock
# release anchors to a known codex version. Override with
# --build-arg CODEX_VERSION=x.y.z.
ARG CODEX_VERSION=0.140.0
RUN npm install -g @openai/codex@${CODEX_VERSION}

RUN useradd -m -u 10001 -s /bin/bash agent
COPY init-firewall.sh /usr/local/bin/init-firewall.sh
COPY entrypoint.sh    /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/init-firewall.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
