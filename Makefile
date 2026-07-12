PREFIX ?= /usr/local
BIN := bin
SRC := $(shell find . -name '*.go' -not -path './bin/*')

# VERSION drives `-X main.version=…` so `drydock version` reports something
# useful on source builds, not just "dev". Resolution order:
#   1. VERSION=v0.1.3 make build                — explicit override
#   2. release builds: nearest tag (v0.1.x)     — what the brew formula sees
#   3. dev builds:     "<tag>-<n>-g<sha>"       — git describe
#   4. no git context: "dev"                    — preserves the old default
VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build install uninstall test test-squid-live test-squid-e2e test-integration redteam redteam-report redteam-vm release-preflight tag-release check-release-args demo sbom docs verify-build vet lint image network init clean help

all: build

help:
	@echo "Targets:"
	@echo "  build       compile bin/brokerd and bin/drydock"
	@echo "  install     binaries into \$$PREFIX/bin, image+config into \$$PREFIX/share/drydock (PREFIX=$(PREFIX))"
	@echo "  uninstall   remove installed binaries + share"
	@echo "  test        go test -race ./..."
	@echo "  test-squid-live  proxy-auth path (squidlive tag); requires squid on PATH"
	@echo "  test-squid-e2e   VM-level egress widening (squide2e tag); requires container runtime + images + squid"
	@echo "  redteam     run the host-side adversarial containment suite (A3-A6)"
	@echo "  redteam-report  same as redteam but prints a per-claim GREEN/RED table"
	@echo "  redteam-vm  run the VM-backed attacks (A1/A2/A7); macOS + container runtime"
	@echo "  release-preflight  full pre-release gate: unit + host (A3-A6) + VM (A1/A2/A7)"
	@echo "  tag-release VERSION=vX.Y.Z  run the preflight, then tag + push the release"
	@echo "  demo        run the narrated breach demo (real attacks; add VM=1 for A1/A2/A7)"
	@echo "  sbom        write a CycloneDX SBOM to dist/drydock.cdx.json"
	@echo "  verify-build  rebuild the binaries and check them against SUMS=<release bin.sha256>"
	@echo "  vet         go vet ./..."
	@echo "  lint        staticcheck ./... (deeper static analysis)"
	@echo "  image       container build -t drydock-sandbox:latest image/"
	@echo "  network     create the drydock-egress vmnet network if missing"
	@echo "  init        run \`drydock init\` to do first-time setup end-to-end"
	@echo "  clean       remove bin/"

$(BIN)/brokerd: $(SRC) go.mod go.sum
	@mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $@ ./cmd/brokerd

$(BIN)/drydock: $(SRC) go.mod go.sum
	@mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $@ ./cmd/drydock

build: $(BIN)/brokerd $(BIN)/drydock

install: build
	install -d $(PREFIX)/bin $(PREFIX)/share/drydock
	install -m 0755 $(BIN)/brokerd $(PREFIX)/bin/brokerd
	install -m 0755 $(BIN)/drydock $(PREFIX)/bin/drydock
	# Copy image build contexts + config so `drydock init` can find them
	# from anywhere — the binary discovers $PREFIX/share/drydock/image
	# relative to itself.
	cp -R image $(PREFIX)/share/drydock/
	cp -R config $(PREFIX)/share/drydock/
	@echo "installed: $(PREFIX)/bin/{brokerd,drydock}, $(PREFIX)/share/drydock/"

uninstall:
	rm -f $(PREFIX)/bin/brokerd $(PREFIX)/bin/drydock
	rm -rf $(PREFIX)/share/drydock

test:
	go test -race -count=1 ./...

# Squid-backed proxy-auth test: boots a real squid on the loopback interface,
# exercises AddTask/RemoveTask and the brokerd __squid-authhelper end-to-end.
# Requires: squid on PATH (Homebrew: brew install squid). No container runtime.
test-squid-live:
	go test -tags squidlive ./internal/netfw/ -run TestSquidProxyAuth_Live -v

# Full VM-level egress widening test: real sandbox container on the drydock-egress
# vmnet, real init-firewall.sh nft default-deny pin, real squid, real auth helper.
# Requires: container system start; drydock-sandbox:latest + drydock-anchor:latest
# images (make image); the drydock-egress network (make network); squid on PATH.
test-squid-e2e:
	go test -tags squide2e ./internal/netfw/ -run TestEgressWidening_E2E -v

# Integration tests boot brokerd as a subprocess and exercise the HTTP +
# CLI surface against a real Apple container runtime. Macos-only; requires
# `make build network image-anchor` first so the binaries and the anchor
# image exist. Does NOT spend Anthropic tokens (uses a placeholder key).
test-integration: build
	go test -tags=integration -count=1 -timeout=2m ./tests/...

# redteam runs the adversarial containment suite: each test performs an attack
# from THREAT_MODEL.md and asserts it is blocked. Host-side claims (A3-A6) run
# here and in CI. VM-backed claims (A1, A2, A7) need the sandbox — run
# `make test-integration` on macOS / Apple silicon (added as they land).
REDTEAM := TestRedteam_A[0-9]|TestHostCommit_IgnoresPlantedHook|TestCaptureDiff_ExcludesTaskDir|TestGateway_RouteAllowlist
redteam:
	@echo "== drydock red-team — attacks that must fail (host-side: A3-A6) =="
	go test -count=1 -run '$(REDTEAM)' ./...
	@echo "== host-side containment verified. VM-backed A1/A2/A7: make redteam-vm =="

# redteam-report runs the same red-team tests with -json output and pipes
# through cmd/redteam-report to print a per-claim GREEN/RED table.
redteam-report:
	@go test -json -count=1 -run '$(REDTEAM)' ./... | go run ./cmd/redteam-report

# redteam-vm runs the VM-backed attacks (A1 key-exfil, A2 egress, A7
# ephemerality) inside the sandbox. macOS / Apple silicon only; needs the
# `container` runtime + the drydock-sandbox image (`make image`).
redteam-vm: build
	@echo "== drydock red-team — VM-backed attacks (A1, A2, A7) =="
	go test -tags=integration -count=1 -timeout=10m -run 'TestRedteam_' ./tests/...

# --- Release gate ---
# The A1/A2/A7 containment tests need Apple `container` (macOS 26 + Apple
# silicon), which hosted CI cannot provide, so a GitHub-green PR does NOT prove
# the isolation claims behind a release. release-preflight is the enforced local
# gate before cutting a release: rebuild the images the release ships, then run
# the full unit suite, the host red-team (A3-A6), and the VM-backed red-team (A1
# key-exfil, A2 egress, A7 ephemerality) against the freshly built image.
release-preflight: build image network
	@echo "== release preflight [1/3]: unit suite (race) =="
	go test -race -count=1 ./...
	@echo "== release preflight [2/3]: host red-team (A3-A6) =="
	go test -count=1 -run '$(REDTEAM)' ./...
	@echo "== release preflight [3/3]: VM-backed red-team (A1, A2, A7) =="
	go test -tags=integration -count=1 -timeout=10m -run 'TestRedteam_' ./tests/...
	@echo ""
	@echo "== release preflight GREEN: unit + host (A3-A6) + VM (A1/A2/A7) all pass =="

# tag-release is the blessed release path: it enforces release-preflight (so a
# release can never ship without the VM containment tests behind its headline
# claims), then creates and pushes the vX.Y.Z tag, which triggers the signed
# release build in release.yml. Requires main, a clean tree, a stamped CHANGELOG,
# and VERSION, e.g.  make tag-release VERSION=v0.6.3
tag-release: check-release-args release-preflight
	git tag -a "$(VERSION)" -m "drydock $(VERSION)"
	git push origin "$(VERSION)"
	@echo "== tagged + pushed $(VERSION); release.yml now builds the signed artifacts =="

check-release-args:
	@# VERSION defaults to `git describe` (for build ldflags); a release needs an
	@# explicit clean vX.Y.Z, so reject the describe/dev form.
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$$' \
		|| { echo "pass an explicit release version: make tag-release VERSION=vX.Y.Z (got '$(VERSION)')"; exit 1; }
	@test "$$(git rev-parse --abbrev-ref HEAD)" = "main" \
		|| { echo "cut releases from main (currently on $$(git rev-parse --abbrev-ref HEAD))"; exit 1; }
	@git diff --quiet && git diff --cached --quiet \
		|| { echo "working tree is dirty; commit or stash before tagging"; exit 1; }
	@grep -q "^## $(VERSION) " CHANGELOG.md \
		|| { echo "CHANGELOG.md has no '## $(VERSION)' section; stamp the release first"; exit 1; }
	@git rev-parse "$(VERSION)" >/dev/null 2>&1 && { echo "tag $(VERSION) already exists"; exit 1; } || true

# demo runs the narrated breach demo: the same red-team attacks, presented as a
# recordable 60-second story. Host-side by default (no VM, no API spend); set
# VM=1 to also run A1/A2/A7 in the sandbox. demo/breach.sh records nothing —
# wrap it in `asciinema rec` to capture a cast.
demo:
	@./demo/breach.sh $(if $(VM),--vm,)

# sbom writes a CycloneDX SBOM of the module + its dependencies to dist/, beside
# the release tarball. Go-native (no external binary); the version is pinned for
# reproducibility. The release workflow runs this after `make dist`.
CYCLONEDX_VERSION := v1.7.0
GRYPE_VERSION := v0.115.0 # image CVE scanner (.github/workflows/image-scan.yml reads this pin)
sbom:
	@mkdir -p dist
	go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_VERSION) \
		mod -json -output dist/drydock.cdx.json .
	@echo "==> dist/drydock.cdx.json"

# docs renders site/docs/*.md into site/docs/*.html for GitHub Pages.
# Go-native (no external binary); deterministic output.
docs:
	go run ./cmd/docs-build
	@echo "==> site/docs built"

vet:
	go vet ./...

# Deeper static analysis than `go vet` (unused code, simplifications, bug
# patterns). Go-native via pinned `go run`, matching the SBOM tool pattern;
# no global install needed. CI runs this on every PR.
STATICCHECK_VERSION := v0.7.0
lint:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

image: image-sandbox image-anchor

image-sandbox:
	container build -t drydock-sandbox:latest image/

image-anchor:
	container build -t drydock-anchor:latest image/anchor/

network:
	@container network ls 2>/dev/null | awk '{print $$1}' | grep -qx drydock-egress \
		|| container network create --subnet 192.168.66.0/24 drydock-egress

init: build
	$(BIN)/drydock init

clean:
	rm -rf $(BIN) dist/

# Build a redistributable tarball that anyone with repo access can fetch
# via `gh release download` and untar straight into a PREFIX. Contains a
# stripped binary pair plus the image+config build contexts so the
# downstream `drydock init` can build images without re-cloning. Set
# DRYDOCK_VERSION=v… on the command line; defaults to git describe.
DRYDOCK_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DRYDOCK_OS := $(shell uname -s | tr A-Z a-z)
DRYDOCK_ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
DIST_NAME := drydock-$(DRYDOCK_VERSION)-$(DRYDOCK_OS)-$(DRYDOCK_ARCH)
DIST_DIR := dist/$(DIST_NAME)

dist: clean
	@mkdir -p $(DIST_DIR)/bin $(DIST_DIR)/share/drydock
	go build -trimpath -ldflags '-s -w -X main.version=$(DRYDOCK_VERSION)' \
		-o $(DIST_DIR)/bin/brokerd ./cmd/brokerd
	go build -trimpath -ldflags '-s -w -X main.version=$(DRYDOCK_VERSION)' \
		-o $(DIST_DIR)/bin/drydock ./cmd/drydock
	# Per-binary checksums (paths relative to the dist root) so a third party
	# can `make verify-build` and confirm the binaries reproduce byte-for-byte.
	@cd $(DIST_DIR) && shasum -a 256 bin/brokerd bin/drydock > ../$(DIST_NAME)-bin.sha256
	cp -R image $(DIST_DIR)/share/drydock/
	cp -R config $(DIST_DIR)/share/drydock/
	cp README.md LICENSE SECURITY.md THREAT_MODEL.md $(DIST_DIR)/
	@printf '#!/bin/sh\nset -e\nPREFIX=$${1:-/usr/local}\ninstall -d "$$PREFIX/bin" "$$PREFIX/share/drydock"\ninstall -m 0755 bin/brokerd "$$PREFIX/bin/brokerd"\ninstall -m 0755 bin/drydock "$$PREFIX/bin/drydock"\ncp -R share/drydock/. "$$PREFIX/share/drydock/"\necho "installed: $$PREFIX/bin/{brokerd,drydock}, $$PREFIX/share/drydock/"\n' > $(DIST_DIR)/install.sh
	@chmod +x $(DIST_DIR)/install.sh
	tar -C dist -czf dist/$(DIST_NAME).tar.gz $(DIST_NAME)
	shasum -a 256 dist/$(DIST_NAME).tar.gz | tee dist/$(DIST_NAME).tar.gz.sha256
	@echo "==> dist/$(DIST_NAME).tar.gz"

# verify-build rebuilds the release binaries with the exact release flags and
# checks them against a published `*-bin.sha256` — proving they reproduce
# byte-for-byte. Needs Go 1.26.5 on darwin/arm64 and a CLEAN checkout of the
# tag (so `git describe` matches the released version). Usage:
#   git checkout vX.Y.Z
#   gh release download vX.Y.Z -R sricola/drydock -p '*-bin.sha256'
#   make verify-build SUMS=drydock-vX.Y.Z-darwin-arm64-bin.sha256
verify-build:
	@test -n "$(SUMS)" || { echo "usage: make verify-build SUMS=<...-bin.sha256 from the release>"; exit 2; }
	@mkdir -p $(BIN)
	go build -trimpath -ldflags '-s -w -X main.version=$(DRYDOCK_VERSION)' -o $(BIN)/brokerd ./cmd/brokerd
	go build -trimpath -ldflags '-s -w -X main.version=$(DRYDOCK_VERSION)' -o $(BIN)/drydock ./cmd/drydock
	shasum -a 256 -c $(SUMS)
	@echo "==> reproducible: binaries match $(SUMS)"
