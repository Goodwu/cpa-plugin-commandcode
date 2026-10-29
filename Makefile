# Build the CommandCode subscription plugin as a CLIProxyAPI dynamic plugin.
#
# Local development (current host platform):
#   make build   # runs vet + tests, then builds bin/commandcode-go-v<version>.<ext>
#   make install PLUGINS_DIR=/path/to/cliproxyapi/plugins
#
# Release packaging (the 4-platform matrix used by CI and tag releases):
#   make package-linux-amd64 package-linux-arm64 \
#        package-darwin-amd64 package-darwin-arm64
#   # -> dist/commandcode-go_<version>_<goos>_<goarch>.zip + dist/checksums.txt
#
# The version lives here and is injected at link time
# (-ldflags -X main.pluginVersion=$(VERSION)), so the registration metadata,
# artifact file names, and release assets always agree.
#
# Default is derived from git: an exact release tag yields that tag (v0.1.3 ->
# 0.1.3); otherwise the nearest tag plus the short commit id. CI and releases
# pass VERSION= explicitly, which always wins.
GIT_DESCRIBE := $(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null || git describe --always 2>/dev/null)
GIT_VERSION := $(shell echo '$(GIT_DESCRIBE)' | sed -E 's/^v//; s/-[0-9]+-g/-g/; s/^([0-9a-f]{7,})$$/0.0.0-g\1/')
VERSION ?= $(if $(GIT_VERSION),$(GIT_VERSION),0.1.0)
PLUGIN_ID := commandcode-go

GO_IMAGE ?= docker.io/library/golang:1.26-bookworm
CONTAINER_RUNTIME ?= docker
CONTAINER_USER_ARGS ?= --user $$(id -u):$(id -g)
CACHE_DIR := .cache
VERSION_LDFLAG := -X main.pluginVersion=$(VERSION)
BUILD_FLAGS := -buildvcs=false -trimpath -ldflags "$(VERSION_LDFLAG)" -buildmode=c-shared

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

ifeq ($(OS),Windows_NT)
PLUGIN_EXT := dll
else ifeq ($(UNAME_S),Darwin)
PLUGIN_EXT := dylib
else
PLUGIN_EXT := so
endif

GOOS := $(shell uname -s | tr 'A-Z' 'a-z')
GOARCH := $(UNAME_M)
ifeq ($(GOARCH),aarch64)
GOARCH := arm64
endif
ifeq ($(OS),Windows_NT)
GOOS := windows
ifeq ($(GOARCH),AMD64)
GOARCH := amd64
endif
endif

BIN_DIR := $(CURDIR)/bin
OUT := $(BIN_DIR)/$(PLUGIN_ID)-v$(VERSION).$(PLUGIN_EXT)

# Release artifacts land in build/plugins/<goos>/<goarch>/ (mirroring the
# host's plugins directory layout) and are zipped by scripts/package-release.sh.
LINUX_AMD64_PLUGIN_DIR := build/plugins/linux/amd64
LINUX_AMD64_PLUGIN := $(LINUX_AMD64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).so
LINUX_ARM64_PLUGIN_DIR := build/plugins/linux/arm64
LINUX_ARM64_PLUGIN := $(LINUX_ARM64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).so
DARWIN_AMD64_PLUGIN_DIR := build/plugins/darwin/amd64
DARWIN_AMD64_PLUGIN := $(DARWIN_AMD64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).dylib
DARWIN_ARM64_PLUGIN_DIR := build/plugins/darwin/arm64
DARWIN_ARM64_PLUGIN := $(DARWIN_ARM64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).dylib

# Point this at the plugins directory of your CLIProxyAPI deployment
# (the directory referenced by `plugins.dir` in config.yaml, default "plugins").
PLUGINS_DIR ?=

.PHONY: build test vet fmt clean install \
	build-linux-amd64 build-linux-arm64 \
	build-darwin-amd64 build-darwin-arm64 \
	package-linux-amd64 package-linux-arm64 \
	package-darwin-amd64 package-darwin-arm64

build: vet test $(OUT)

$(OUT): $(wildcard *.go *.h *.c catalog.json go.mod)
	@mkdir -p $(BIN_DIR)
	go build $(BUILD_FLAGS) -o $(abspath $(OUT)) .
	rm -f $(BIN_DIR)/$(PLUGIN_ID)-v$(VERSION).h
	@echo "built $(OUT)"

vet:
	go vet ./...

test:
	go test ./...

fmt:
	gofmt -w .

# Linux builds run inside Docker so the artifact links against the glibc of
# the pinned image (matching the containerized deployment).
build-linux-amd64:
	mkdir -p $(LINUX_AMD64_PLUGIN_DIR) $(CACHE_DIR)/go-build $(CACHE_DIR)/go-mod $(CACHE_DIR)/home
	$(CONTAINER_RUNTIME) run --rm \
		--platform linux/amd64 \
		$(CONTAINER_USER_ARGS) \
		-e HOME=/src/$(CACHE_DIR)/home \
		-e GOCACHE=/src/$(CACHE_DIR)/go-build \
		-e GOMODCACHE=/src/$(CACHE_DIR)/go-mod \
		-v "$(CURDIR):/src" \
		-w /src \
		$(GO_IMAGE) \
		sh -ec 'CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(LINUX_AMD64_PLUGIN) . && rm -f $(LINUX_AMD64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).h'

build-linux-arm64:
	mkdir -p $(LINUX_ARM64_PLUGIN_DIR) $(CACHE_DIR)/go-build-arm64 $(CACHE_DIR)/go-mod $(CACHE_DIR)/home
	$(CONTAINER_RUNTIME) run --rm \
		--platform linux/arm64 \
		$(CONTAINER_USER_ARGS) \
		-e HOME=/src/$(CACHE_DIR)/home \
		-e GOCACHE=/src/$(CACHE_DIR)/go-build-arm64 \
		-e GOMODCACHE=/src/$(CACHE_DIR)/go-mod \
		-v "$(CURDIR):/src" \
		-w /src \
		$(GO_IMAGE) \
		sh -ec 'CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(LINUX_ARM64_PLUGIN) . && rm -f $(LINUX_ARM64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).h'

# Darwin cross-compilation requires a Darwin host (the Apple SDKs).
build-darwin-amd64:
	@test "$(UNAME_S)" = "Darwin" || \
		{ echo "error: build-darwin-amd64 requires a Darwin host" >&2; exit 1; }
	mkdir -p $(DARWIN_AMD64_PLUGIN_DIR)
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build $(BUILD_FLAGS) -o $(abspath $(DARWIN_AMD64_PLUGIN)) .
	rm -f $(DARWIN_AMD64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).h

build-darwin-arm64:
	@test "$(UNAME_S)" = "Darwin" || \
		{ echo "error: build-darwin-arm64 requires a Darwin host" >&2; exit 1; }
	mkdir -p $(DARWIN_ARM64_PLUGIN_DIR)
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(abspath $(DARWIN_ARM64_PLUGIN)) .
	rm -f $(DARWIN_ARM64_PLUGIN_DIR)/$(PLUGIN_ID)-v$(VERSION).h

package-linux-amd64: build-linux-amd64
	scripts/package-release.sh "$(VERSION)" linux amd64

package-linux-arm64: build-linux-arm64
	scripts/package-release.sh "$(VERSION)" linux arm64

package-darwin-amd64: build-darwin-amd64
	scripts/package-release.sh "$(VERSION)" darwin amd64

package-darwin-arm64: build-darwin-arm64
	scripts/package-release.sh "$(VERSION)" darwin arm64

# Install the built plugin into a CLIProxyAPI plugins directory.
# Usage: make install PLUGINS_DIR=/path/to/cliproxyapi/plugins
install: build
	@if [ -z "$(PLUGINS_DIR)" ]; then \
		echo "PLUGINS_DIR is empty; refusing to guess. Example:"; \
		echo "  make install PLUGINS_DIR=/path/to/cliproxyapi/plugins"; \
		exit 1; \
	fi
	@mkdir -p $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)
	cp $(abspath $(OUT)) $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/
	@echo "installed into $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/"

clean:
	rm -rf $(BIN_DIR) build dist $(CACHE_DIR)
