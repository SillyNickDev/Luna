# Luna. `make` builds ./luna; `make help` lists the rest.
# Releases are pure Go and static (CGO_ENABLED=0): the core bundle runs this same
# binary inside its sandbox, which has no dynamic loader.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local
GO      ?= go
PYTHON  ?= python3
GOFLAGS := -trimpath -ldflags="-s -w -X main.lunaVersion=$(VERSION)"
export CGO_ENABLED := 0

PLATFORMS := linux/amd64 linux/arm64
VENV      := build/venv

.PHONY: all build test test-go test-py check proto release install uninstall clean help

all: build

build: ## build ./luna for this machine
	$(GO) build $(GOFLAGS) -o luna ./cmd/luna

test: test-go test-py ## run every test

test-go: ## Go tests (the sandbox tests need user namespaces and Landlock)
	$(GO) test ./...

test-py: ## Python SDK tests
	cd sdk/python/tests && $(PYTHON) -m unittest test_sdk

check: ## formatting and vet, as CI would
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }
	$(GO) vet ./...

proto: $(VENV) ## regenerate the Go and Python stubs from api/plugin.proto
	PATH="$$PATH:$$($(GO) env GOPATH)/bin" protoc -I api --go_out=. --go_opt=module=luna \
		--go-grpc_out=. --go-grpc_opt=module=luna api/plugin.proto
	$(VENV)/bin/python -m grpc_tools.protoc -I api --python_out=sdk/python/luna/_pb \
		--grpc_python_out=sdk/python/luna/_pb api/plugin.proto
	sed -i 's/^import plugin_pb2 as plugin__pb2/from . import plugin_pb2 as plugin__pb2/' \
		sdk/python/luna/_pb/plugin_pb2_grpc.py

$(VENV): # grpcio-tools pinned to the runtime's grpcio
	$(PYTHON) -m venv $(VENV)
	$(VENV)/bin/pip -q install grpcio-tools==1.84.0

release: ## static builds for every platform into dist/, with checksums
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/luna-$(VERSION)-$$os-$$arch; \
		echo "  $$out"; GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) -o $$out ./cmd/luna || exit 1; \
	done
	cd dist && sha256sum luna-$(VERSION)-* > SHA256SUMS-$(VERSION)

install: build ## install to $(PREFIX)/bin (default ~/.local/bin)
	install -Dm755 luna $(DESTDIR)$(PREFIX)/bin/luna
	@echo "installed $(DESTDIR)$(PREFIX)/bin/luna; if the core bundle is installed, luna restart picks it up"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/luna

clean:
	rm -rf luna dist build
	find sdk/python -name __pycache__ -type d -exec rm -rf {} +

help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-10s %s\n", $$1, $$2}'
