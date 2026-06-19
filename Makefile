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

.PHONY: all build install uninstall test redteam redteam-vm vet image network init clean help

all: build

help:
	@echo "Targets:"
	@echo "  build       compile bin/brokerd and bin/drydock"
	@echo "  install     binaries into \$$PREFIX/bin, image+config into \$$PREFIX/share/drydock (PREFIX=$(PREFIX))"
	@echo "  uninstall   remove installed binaries + share"
	@echo "  test        go test -race ./..."
	@echo "  redteam     run the host-side adversarial containment suite (A3-A6)"
	@echo "  redteam-vm  run the VM-backed attacks (A1/A2/A7); macOS + container runtime"
	@echo "  vet         go vet ./..."
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
REDTEAM := TestRedteam_A[0-9]|TestHostCommit_IgnoresPlantedHook|TestCaptureDiff_ExcludesTaskDir
redteam:
	@echo "== drydock red-team — attacks that must fail (host-side: A3-A6) =="
	go test -count=1 -run '$(REDTEAM)' ./...
	@echo "== host-side containment verified. VM-backed A1/A2/A7: make redteam-vm =="

# redteam-vm runs the VM-backed attacks (A1 key-exfil, A2 egress, A7
# ephemerality) inside the sandbox. macOS / Apple silicon only; needs the
# `container` runtime + the drydock-sandbox image (`make image`).
redteam-vm: build
	@echo "== drydock red-team — VM-backed attacks (A1, A2, A7) =="
	go test -tags=integration -count=1 -timeout=10m -run 'TestRedteam_' ./tests/...

vet:
	go vet ./...

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
	cp -R image $(DIST_DIR)/share/drydock/
	cp -R config $(DIST_DIR)/share/drydock/
	cp README.md LICENSE SECURITY.md THREAT_MODEL.md $(DIST_DIR)/
	@printf '#!/bin/sh\nset -e\nPREFIX=$${1:-/usr/local}\ninstall -d "$$PREFIX/bin" "$$PREFIX/share/drydock"\ninstall -m 0755 bin/brokerd "$$PREFIX/bin/brokerd"\ninstall -m 0755 bin/drydock "$$PREFIX/bin/drydock"\ncp -R share/drydock/. "$$PREFIX/share/drydock/"\necho "installed: $$PREFIX/bin/{brokerd,drydock}, $$PREFIX/share/drydock/"\n' > $(DIST_DIR)/install.sh
	@chmod +x $(DIST_DIR)/install.sh
	tar -C dist -czf dist/$(DIST_NAME).tar.gz $(DIST_NAME)
	shasum -a 256 dist/$(DIST_NAME).tar.gz | tee dist/$(DIST_NAME).tar.gz.sha256
	@echo "==> dist/$(DIST_NAME).tar.gz"
