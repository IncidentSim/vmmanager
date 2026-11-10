.DEFAULT_GOAL := build

GO ?= go
PYTHON ?= python3
BUILD_DIR ?= .
IMAGE ?= images/ubuntu-postgres.ext4
FC_SOCKET ?= /run/firecracker-builder/main.sock

.PHONY: build remote-client test vet check shell-syntax run verify demo proto proto-check

build:
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_DIR)/image-builder" .

remote-client:
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_DIR)/remote-client" ./cmd/remote-client

test:
	$(GO) test -race -cover ./...
	$(PYTHON) -m unittest -v test_verify_image.py test_project_layout.py deployment/test_mtls.py

vet:
	$(GO) vet ./...

shell-syntax:
	@for script in scripts/*.sh deployment/*.sh; do bash -n "$$script" || exit; done

proto:
	bash scripts/gen-proto.sh

proto-check:
	bash scripts/gen-proto.sh --check

check: test vet shell-syntax proto-check

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
