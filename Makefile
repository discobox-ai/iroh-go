# iroh-go
#
# `make lib` builds the native library for this machine and installs it into
# the matching module under libs/. Everything else assumes that has been done
# at least once.

GO ?= go
PLATFORM := $(shell $(GO) env GOOS)_$(shell $(GO) env GOARCH)

.PHONY: all
all: lib test

## lib: build the Rust cdylib for this machine and install it under libs/
.PHONY: lib
lib:
	./scripts/build-lib.sh

## libs: cross-build every platform this machine can target (release prep)
##       limit it with PLATFORMS, e.g. make libs PLATFORMS="linux_arm64"
.PHONY: libs
libs:
	./scripts/build-libs.sh $(PLATFORMS)

## test: run the Go test suite with CGO disabled, as users will build it
.PHONY: test
test:
	CGO_ENABLED=0 $(GO) test -count=1 ./...

## race: run the Go test suite under the race detector
.PHONY: race
race:
	$(GO) test -race -count=1 ./...

## rust-test: run the Rust unit tests
.PHONY: rust-test
rust-test:
	cd rust && cargo test

## vet: go vet plus gofmt and cargo fmt checks
.PHONY: vet
vet:
	$(GO) vet ./...
	@out=$$(gofmt -l . | grep -v '^rust/' || true); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	cd rust && cargo fmt --check && cargo clippy --all-targets -- -D warnings

## cross: check that every supported platform still compiles without CGO
.PHONY: cross
cross:
	@set -e; for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		echo "  $$t"; \
		CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -o /dev/null ./...; \
	done
	@echo "  linux/amd64 (musl)"
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -tags musl -o /dev/null ./...
	@echo "  iroh_nolibs"
	@CGO_ENABLED=0 $(GO) build -tags iroh_nolibs -o /dev/null ./...

## clean: remove build output
.PHONY: clean
clean:
	cd rust && cargo clean
	rm -f libs/*/lib/iroh_go.lib libs/*/lib/iroh_go.lib.sha256

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
