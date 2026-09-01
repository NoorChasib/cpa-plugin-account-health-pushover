SHELL := /bin/bash

GO ?= go
GOFMT ?= gofmt
PLUGIN_ID := account-health-pushover
MODULE := github.com/NoorChasib/cpa-plugin-account-health-pushover
VERSION ?= 0.1.0
LDFLAGS := -X $(MODULE)/internal/plugin.Version=$(VERSION)
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

ifeq ($(GOOS),windows)
LIB_EXT := dll
else ifeq ($(GOOS),darwin)
LIB_EXT := dylib
else
LIB_EXT := so
endif

LIB := dist/$(PLUGIN_ID).$(LIB_EXT)
ARCHIVE := dist/$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip

.PHONY: all fmt fmt-check test test-race vet build c-shared ci clean package-current checksums verify-release verify-release-full smoke

all: ci c-shared

fmt:
	$(GOFMT) -w .

fmt-check:
	@test -z "$$($(GOFMT) -l .)" || { $(GOFMT) -l .; exit 1; }

test:
	$(GO) test ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

c-shared:
	@mkdir -p dist
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $(LIB) .
	@rm -f dist/$(PLUGIN_ID).h

ci: fmt-check vet test-race build

package-current: c-shared
	python3 scripts/package-release.py --library $(LIB) --archive $(ARCHIVE) --entry $(PLUGIN_ID).$(LIB_EXT)

checksums:
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum $(PLUGIN_ID)_*.zip | sort > checksums.txt; \
	else \
		shasum -a 256 $(PLUGIN_ID)_*.zip | sort > checksums.txt; \
	fi

# Verifies whatever platform archives exist in dist (e.g. the single archive
# from `make package-current`). The release workflow's verify-bundle/publish
# jobs run the full five-platform check via verify-release-full semantics.
verify-release:
	./scripts/verify-release.sh --partial dist

verify-release-full:
	./scripts/verify-release.sh dist

smoke:
	./scripts/smoke-test.sh

clean:
	rm -rf dist
