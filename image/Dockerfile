# Pinned by digest for a reproducible base. Bump deliberately: re-pull
# node:22-bookworm-slim, read its digest (`container image inspect`), and update
# here. See docs/ROADMAP.md (Phase 2.5) for the dependency-pinning policy.
FROM node:22-bookworm-slim@sha256:e21fc383b50d5347dc7a9f1cae45b8f4e2f0d39f7ade28e4eef7d2934522b752
# Base toolchain + Python 3.11 (Debian default). Python lives next to Node so
# the egress allowlist's pypi.org slot has a runtime to land on.
# Privilege drop uses setpriv (util-linux, already in the base) — not gosu — so
# the agent drop gets no-new-privs + an emptied capability bounding set, and the
# image sheds gosu's stale go1.19 CVE cluster. See image/drop-agent.sh.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset \
      python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*

# Go: Debian bookworm ships 1.19 which is too old to be useful; install the
# upstream tarball matching the build arch. Adds ~400MB to the image but the
# alternative is no Go at all.
ARG GO_VERSION=1.26.5
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

# Pin the opencode CLI for the OpenAI-compatible (bring-your-own-model) lane.
# Bump deliberately. Override with --build-arg OPENCODE_VERSION=x.y.z. The
# build-time `opencode --version` is a smoke check that the (script-running)
# install produced a working binary.
ARG OPENCODE_VERSION=1.17.11
RUN npm install -g --foreground-scripts opencode-ai@${OPENCODE_VERSION} && opencode --version

# Pin the Gemini CLI for the native google vendor. Bump deliberately; every
# drydock release anchors to a known version (0.49.0 validated by the 3B spike).
ARG GEMINI_CLI_VERSION=0.49.0
RUN npm install -g @google/gemini-cli@${GEMINI_CLI_VERSION} && gemini --version

RUN useradd -m -u 10001 -s /bin/bash agent

# Strip every SUID/SGID bit in the image. The sandbox never needs setuid: root
# runs the entrypoint, then drops to `agent` via setpriv. Combined with the
# --no-new-privs drop, this removes the "stray SUID binary lets the agent climb
# back to root+CAP_NET_ADMIN" path entirely (defense in depth).
RUN find / -xdev -perm /6000 -type f -exec chmod -s {} + 2>/dev/null || true

COPY init-firewall.sh         /usr/local/bin/init-firewall.sh
COPY drop-agent.sh            /usr/local/bin/drop-agent
COPY entrypoint.sh            /usr/local/bin/entrypoint.sh
COPY write-codex-config.sh    /usr/local/bin/write-codex-config.sh
COPY write-opencode-config.sh /usr/local/bin/write-opencode-config.sh
COPY write-gemini-config.sh   /usr/local/bin/write-gemini-config.sh
RUN chmod 0755 /usr/local/bin/init-firewall.sh /usr/local/bin/drop-agent /usr/local/bin/entrypoint.sh /usr/local/bin/write-codex-config.sh /usr/local/bin/write-opencode-config.sh /usr/local/bin/write-gemini-config.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
