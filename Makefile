.DEFAULT_GOAL := build

# Thin orchestration only: every non-trivial step lives in Go (cmd/vmhost,
# cmd/vm-network, internal/*). See docs/install.md.

GO ?= go
# Keep child tool invocations on the selected toolchain (golang/go#75031).
export PATH := $(shell $(GO) env GOROOT)/bin:$(PATH)
BUILD_DIR ?= bin

# Installation (files only; nothing is started, enabled or reloaded).
PREFIX ?= /usr/local
DESTDIR ?=
SYSCONFDIR ?= /etc
CLIENT_GROUP ?=
ASSET_DIR ?=
STORAGE_ROOT ?=
FIRECRACKER ?=
KERNEL ?=
FORCE ?=
VERIFY_FLAGS ?=

PRODUCT := vmapi vmbot vmctl vm-launcher vm-network vm-helper vm-helper-client vmhost
LEGACY := image-builder remote-client service-launcher

VMHOST := $(BUILD_DIR)/vmhost
VMHOST_FLAGS = --build-dir "$(BUILD_DIR)" --prefix "$(PREFIX)" --sysconfdir "$(SYSCONFDIR)" \
	$(if $(DESTDIR),--destdir "$(DESTDIR)") $(if $(CLIENT_GROUP),--group "$(CLIENT_GROUP)") \
	$(if $(ASSET_DIR),--asset-dir "$(ASSET_DIR)") $(if $(STORAGE_ROOT),--storage-root "$(STORAGE_ROOT)") \
	$(if $(FIRECRACKER),--firecracker "$(FIRECRACKER)") $(if $(KERNEL),--kernel "$(KERNEL)") \
	$(if $(FORCE),--force)

.PHONY: build legacy test vet release-check fmt-check check control-smoke verify-units stage-check \
	install uninstall preflight bundle host-setup host-enable e2e-live e2e clean help

define HELP_TEXT
Targets (details in docs/install.md and docs/ci.md):
  build         build the product executables into BUILD_DIR (default bin/)
  legacy        build the legacy single-VM builder tools (not the product)
  test          all Go tests with -race (root-free)
  check         vet, gofmt, tests, build, staged install round trip, unit verification
  control-smoke executable API smoke (in-memory backend; not an E2E)
  stage-check   root-free install/uninstall round trip in a temporary root
  verify-units  render units and run systemd-analyze verify
  install       place files under DESTDIR/PREFIX (real install needs root)
  uninstall     remove exactly the files recorded by install
  bundle        build vmmanager-example.tar.gz (BUNDLE_OUT=dir; IMAGE=, ASSETS= optional; published by release.yml)
  preflight     read-only host readiness report
  host-setup    create empty jail/work/images dirs (USER_NAME=name, APPLY=1)
  host-enable   enable the helper socket only (APPLY=1)
  e2e-live      opt-in E2E against an already deployed vmapi (VMAPI_E2E=1)
  e2e           real E2E on a disposable KVM host (see docs/ci.md)
  release-check validate .goreleaser.yaml and build a snapshot (no publishing)
endef
export HELP_TEXT

help:
	@printf '%s\n' "$$HELP_TEXT"

## build        build the product executables into $(BUILD_DIR) (static, CGO off)
build:
	@mkdir -p "$(BUILD_DIR)"
	@for c in $(PRODUCT); do CGO_ENABLED=0 $(GO) build -o "$(BUILD_DIR)/$$c" ./cmd/$$c || exit 1; done

## legacy       build the legacy single-VM image builder tools (not part of the product)
legacy:
	@mkdir -p "$(BUILD_DIR)"
	CGO_ENABLED=0 $(GO) build -o "$(BUILD_DIR)/image-builder" .
	CGO_ENABLED=0 $(GO) build -o "$(BUILD_DIR)/remote-client" ./cmd/remote-client
	CGO_ENABLED=0 $(GO) build -o "$(BUILD_DIR)/service-launcher" ./cmd/service-launcher

## test         all Go tests with the race detector (root-free; includes tooling tests)
test:
	$(GO) test -race -cover -timeout 10m ./...

vet:
	$(GO) vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; test -z "$$out" || { echo "gofmt needed:" >&2; echo "$$out" >&2; exit 1; }

## control-smoke  executable API smoke (in-memory backend over loopback; NOT an E2E)
control-smoke:
	$(GO) test -count=1 -timeout 3m -run 'TestExecutableControlFlow|TestExecutableJWTAndComposedHTTPSGateway|TestRecoveryFailureKeepsServingDegraded' ./cmd/vmapi

## verify-units   render the units and run systemd-analyze verify (VERIFY_FLAGS=--require-systemd-analyze fails if absent)
verify-units: build
	$(VMHOST) verify-units $(VMHOST_FLAGS) $(VERIFY_FLAGS)

## stage-check    root-free install/uninstall round trip into a temporary root
stage-check: build
	$(VMHOST) stage-check $(VMHOST_FLAGS)

## check        vet + format + tests + build + staged install round trip + unit verification
check: vet fmt-check test build stage-check verify-units

## install      place files under DESTDIR/PREFIX (run `make build` first; real install needs root)
install:
	$(VMHOST) install $(VMHOST_FLAGS)

## uninstall    remove exactly the files recorded by install; user data is never touched
uninstall:
	$(VMHOST) uninstall $(VMHOST_FLAGS)

## preflight    read-only host readiness report (KVM, systemd, tools, subnet, storage)
preflight:
	$(VMHOST) preflight $(VMHOST_FLAGS)

## bundle       build the self-contained example archive (downloads pinned artifacts, needs fakeroot, squashfs-tools, e2fsprogs; not part of check)
BUNDLE_OUT ?= ../artifacts
BUNDLE_WORK ?= $(BUNDLE_OUT)/work
bundle: build
	$(GO) run ./cmd/vmbundle --bin "$(BUILD_DIR)" --out "$(BUNDLE_OUT)" --work "$(BUNDLE_WORK)" $(if $(IMAGE),--image "$(IMAGE)") $(if $(ASSETS),--assets "$(ASSETS)")

## host-setup   (root) create empty jail/work/images directories; USER=name; APPLY=1 to apply
host-setup:
	$(VMHOST) setup $(VMHOST_FLAGS) $(if $(USER_NAME),--user "$(USER_NAME)") $(if $(APPLY),--apply)

## host-enable  (root) enable the helper socket only; APPLY=1 to apply
host-enable:
	$(VMHOST) enable $(VMHOST_FLAGS) $(if $(APPLY),--apply)

## e2e-live     opt-in live E2E against an already deployed vmapi (VMAPI_E2E=1, see docs/e2e-live.md)
e2e-live:
	@test "$$VMAPI_E2E" = 1 || { echo "e2e-live: set VMAPI_E2E=1 and the VMAPI_E2E_* variables (see docs/e2e-live.md)" >&2; exit 2; }
	$(GO) test -count=1 -p 1 -timeout 45m -v ./e2e

## e2e          test-owned preparation and lifecycle on an explicitly disposable KVM host
# No sudo or privileged preparation without an explicit operator opt-in.
e2e:
	@test "$$VMAPI_E2E_DISPOSABLE" = 1 || { echo "e2e: set VMAPI_E2E_DISPOSABLE=1 ONLY on a disposable KVM host (see docs/ci.md)" >&2; exit 2; }
	$(MAKE) build
	VMAPI_E2E_BUILD_DIR="$(abspath $(BUILD_DIR))" $(GO) test -count=1 -p 1 -timeout 45m -v ./e2e

## release-check  validate .goreleaser.yaml and build a snapshot into dist/ (never publishes)
GORELEASER ?= goreleaser
release-check:
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean --skip=publish

clean:
	rm -rf "$(BUILD_DIR)"
