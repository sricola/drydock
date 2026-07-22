# Pinned by digest for a reproducible base. Bump deliberately: re-pull
# node:22-bookworm-slim, read its digest (`container image inspect`), and update
# here. See docs/ROADMAP.md (Phase 2.5) for the dependency-pinning policy.
FROM node:22-bookworm-slim@sha256:e21fc383b50d5347dc7a9f1cae45b8f4e2f0d39f7ade28e4eef7d2934522b752
# Base toolchain + Python 3.11 (Debian default). Python lives next to Node so
# the egress allowlist's pypi.org slot has a runtime to land on.
# Privilege drop uses setpriv (util-linux, already in the base) — not gosu — so
# the agent drop gets no-new-privs + an emptied capability bounding set, and the
# image sheds gosu's stale go1.19 CVE cluster. See image/drop-agent.sh.
# `apt-get upgrade` layers current Debian point-release security fixes on top of
# the digest-pinned base, so a rebuild (a user's `drydock setup`, or the daily
# image-scan CI) picks them up without waiting for a base-digest bump or a lucky
# rebuild. The base stays digest-pinned as a reproducible starting point (bumped
# deliberately, see Phase 2.5); we trade exact bit-reproducibility of the package
# set for security currency, which is the right call for a locally-built sandbox
# runtime (not a user-verified released artifact). See docs/ROADMAP.md 4.13.
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      git ca-certificates curl jq nftables dnsutils ipset \
      python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*

# Go: Debian bookworm ships 1.19 which is too old to be useful; install the
# upstream tarball matching the build arch. The tarball is extracted as root
# into /usr/local, so verify its published SHA-256 (per arch, pinned below)
# BEFORE extraction: a compromised go.dev/CDN or a corrupted transfer can no
# longer substitute a malicious archive. Bump the sums with GO_VERSION, from
# https://go.dev/dl/?mode=json.
ARG GO_VERSION=1.26.5
ARG GO_SHA256_amd64=5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053
ARG GO_SHA256_arm64=fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) GO_SHA256="$GO_SHA256_amd64" ;; \
      arm64) GO_SHA256="$GO_SHA256_arm64" ;; \
      *) echo "unsupported build arch: $ARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz && \
    echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz && \
    ln -s /usr/local/go/bin/go    /usr/local/bin/go && \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Pin npm itself. The base image's node 22 line bundles npm 10.9.x, whose own
# vendored node_modules (tar, brace-expansion, sigstore, picomatch) go stale
# with the digest-pinned base and accumulate fixable High/Critical CVEs that
# no CLI pin or apt upgrade can clear (the 2026-07-21 cve-gate failure: tar
# 7.5.11 and brace-expansion 2.0.2, both npm's bundled copies). Installing a
# pinned current npm over the bundled one replaces that vendored tree
# wholesale. Bumped by the weekly agent-cli-bump lane like the CLI pins.
# npm has no own install script, so --ignore-scripts per F-09 (below). The
# check asserts the exact pinned version, not just a working binary: here the
# version IS the security invariant, and a half-applied self-install that
# leaves the old npm answering must fail the build, not the next daily scan.
ARG NPM_VERSION=11.18.0
RUN npm install -g --ignore-scripts --no-audit --no-fund npm@${NPM_VERSION} \
 && [ "$(npm --version)" = "${NPM_VERSION}" ] \
 && rm -rf /root/.npm

# Pin the agent CLI. Bump deliberately — every drydock release is
# anchored to a known claude-code version so the threat-model claims
# track behavior we've actually tested. Override at build time with
# --build-arg CLAUDE_CODE_VERSION=x.y.z.
# npm supply chain (F-09): each `npm install -g` runs as root, so a package's
# (or a transitive dep's) install script executes as root at build time. Where a
# CLI's pinned version has NO own install script (codex, gemini-cli), pass
# --ignore-scripts so no install script (including a transitive dep's) runs;
# claude-code (install.cjs) and opencode (postinstall.mjs) genuinely need theirs,
# so they keep scripts and are pinned by exact version instead. Every install
# ends with a `<cli> --version` smoke check so a broken install fails the build.
ARG CLAUDE_CODE_VERSION=2.1.207
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && claude --version

# Pin the Codex CLI alongside claude-code. Bump deliberately; every drydock
# release anchors to a known codex version. Override with
# --build-arg CODEX_VERSION=x.y.z. No own install script -> --ignore-scripts.
ARG CODEX_VERSION=0.144.1
RUN npm install -g --ignore-scripts @openai/codex@${CODEX_VERSION} && codex --version

# Pin the opencode CLI for the OpenAI-compatible (bring-your-own-model) lane.
# Bump deliberately. Override with --build-arg OPENCODE_VERSION=x.y.z. The
# build-time `opencode --version` is a smoke check that the (script-running)
# install produced a working binary.
ARG OPENCODE_VERSION=1.17.18
RUN npm install -g --foreground-scripts opencode-ai@${OPENCODE_VERSION} && opencode --version

# Pin the Gemini CLI for the native google vendor. Bump deliberately; every
# drydock release anchors to a known version (0.49.0 validated by the 3B spike).
ARG GEMINI_CLI_VERSION=0.50.0
RUN npm install -g --ignore-scripts @google/gemini-cli@${GEMINI_CLI_VERSION} && gemini --version

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
