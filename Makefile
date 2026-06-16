PREFIX ?= /usr/local/bin
BIN := bin
SRC := $(shell find . -name '*.go' -not -path './bin/*')

.PHONY: all build install uninstall test vet image network init clean help

all: build

help:
	@echo "Targets:"
	@echo "  build       compile bin/brokerd and bin/drydock"
	@echo "  install     install both binaries into \$$PREFIX ($(PREFIX))"
	@echo "  uninstall   remove installed binaries"
	@echo "  test        go test -race ./..."
	@echo "  vet         go vet ./..."
	@echo "  image       container build -t claude-sandbox:latest image/"
	@echo "  network     create the drydock-egress vmnet network if missing"
	@echo "  init        run \`drydock init\` to do first-time setup end-to-end"
	@echo "  clean       remove bin/"

$(BIN)/brokerd: $(SRC) go.mod go.sum
	@mkdir -p $(BIN)
	go build -o $@ ./cmd/brokerd

$(BIN)/drydock: $(SRC) go.mod go.sum
	@mkdir -p $(BIN)
	go build -o $@ ./cmd/drydock

build: $(BIN)/brokerd $(BIN)/drydock

install: build
	install -d $(PREFIX)
	install -m 0755 $(BIN)/brokerd $(PREFIX)/brokerd
	install -m 0755 $(BIN)/drydock $(PREFIX)/drydock
	@echo "installed: $(PREFIX)/brokerd $(PREFIX)/drydock"

uninstall:
	rm -f $(PREFIX)/brokerd $(PREFIX)/drydock

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

image:
	container build -t claude-sandbox:latest image/

network:
	@container network ls 2>/dev/null | awk '{print $$1}' | grep -qx drydock-egress \
		|| container network create --subnet 192.168.66.0/24 drydock-egress

init: build
	$(BIN)/drydock init

clean:
	rm -rf $(BIN)
