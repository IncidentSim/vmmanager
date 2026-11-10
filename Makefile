.DEFAULT_GOAL := build

GO ?= go
# Go <=1.24 auto-switching to early 1.25 releases can invoke the old `go`
# binary for coverage tools (golang/go#75031). Keep child tool invocations on
# the selected toolchain too, without installing/changing the system Go.
export PATH := $(shell $(GO) env GOROOT)/bin:$(PATH)
PYTHON ?= python3
BUILD_DIR ?= .
IMAGE ?= images/ubuntu-postgres.ext4
FC_SOCKET ?= /run/firecracker-builder/main.sock

.PHONY: build remote-client test vet check shell-syntax run verify demo proto proto-check consumer-smoke verify-units vmd vmctl vm-launcher vm-helper vm-helper-client vmbot vmapi control-build control-smoke

build:
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_DIR)/image-builder" .

remote-client:
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_DIR)/remote-client" ./cmd/remote-client

test:
	$(GO) test -race -cover -timeout 3m ./...
	$(PYTHON) -m unittest -v test_verify_image.py test_project_layout.py deployment/test_mtls.py deployment/multivm/test_deploy.py scripts/integration/test_multi_vm.py

vet:
	$(GO) vet ./...

shell-syntax:
	@for script in scripts/*.sh scripts/integration/*.sh deployment/*.sh deployment/multivm/*.sh; do bash -n "$$script" || exit; done

# New multi-VM applications (build only; no install, no sudo).
vmd vmctl vm-launcher vm-helper vm-helper-client vmbot vmapi:
	mkdir -p "$(BUILD_DIR)"
	CGO_ENABLED=0 $(GO) build -o "$(BUILD_DIR)/$@" ./cmd/$@

# Compile-check the standalone API and the HTTP-only frontend without writing
# binaries (multi-package builds discard output; nothing is installed).
control-build:
	$(GO) build ./cmd/vmapi ./cmd/vmbot

# Executable smoke: builds and runs the REAL vmapi process against an in-memory
# vmlibtest fake behind a real mTLS facade, driven by the public controlclient.
# No Firecracker/KVM/root/network beyond loopback. Included in `test` already;
# kept as an explicit, fast, focused target.
control-smoke:
	$(GO) test -count=1 -timeout 3m -run 'TestExecutableControlFlow|TestRecoveryFailureKeepsServingDegraded' ./cmd/vmapi

proto:
	bash scripts/gen-proto.sh

proto-check:
	bash scripts/gen-proto.sh --check

consumer-smoke:
	bash scripts/consumer-smoke.sh

# Real systemd-analyze on rendered copies of the proposed units (nothing installed).
verify-units:
	bash scripts/verify-units.sh

check: test vet shell-syntax control-build proto-check consumer-smoke verify-units

# Runtime helpers expect image-builder in the repository root.
# These targets use the existing external VMM; no host-service management.
run:
	$(GO) build -o image-builder .
	FC_SOCKET="$(FC_SOCKET)" bash scripts/run-image.sh "$(IMAGE)"

verify:
	$(GO) build -o image-builder .
	FC_SOCKET="$(FC_SOCKET)" FC_VERIFY_IMAGE="$(IMAGE)" $(PYTHON) ./verify-image.py

demo:
	$(GO) build -o image-builder .
	FC_SOCKET="$(FC_SOCKET)" bash scripts/demo-lifecycle.sh
