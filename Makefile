TOOL := go tool -modfile=tools/tools.mod
BUF := $(TOOL) buf
GOLANGCI_LINT := $(TOOL) golangci-lint

# Paths whose contents are produced by `make gen` and must never drift.
GENERATED := gen internal/apiserver/servicegen internal/store/sqlite/db

.PHONY: all gen check lint lint-proto lint-go lint-sh breaking test test-root test-web e2e gen-check dev tools deps guest-kernel guest-initramfs e2e-host e2e-host-network e2e-host-vms e2e-host-flow release

# Every shell script in the repo, committed or not, outside ignored paths.
SHELL_SCRIPTS = $(shell git ls-files --cached --others --exclude-standard '*.sh' 'deploy/host/*.env' 'deploy/host/*.env.example' 'guest/kernel/*.env')

all: check

deps: node_modules/.modules.yaml

node_modules/.modules.yaml: package.json pnpm-lock.yaml gen/ts/package.json web/package.json
	pnpm install --frozen-lockfile

gen: deps
	@if [ -n "$$(find proto -name '*.proto' 2>/dev/null)" ]; then \
		$(BUF) generate && $(BUF) generate --template buf.gen.hangar.yaml; \
	else \
		echo "gen: no .proto files yet"; \
	fi
	$(TOOL) sqlc generate

lint: lint-proto lint-go lint-sh

lint-proto:
	@if [ -n "$$(find proto -name '*.proto' 2>/dev/null)" ]; then $(BUF) lint; fi

lint-sh:
	@if [ -n "$(SHELL_SCRIPTS)" ]; then ./tools/shellcheck.sh --shell=bash -x $(SHELL_SCRIPTS); fi

lint-go:
	$(GOLANGCI_LINT) run ./...

# Compares against main; skipped while main has no schema to compare with.
breaking:
	@if git cat-file -e main:proto 2>/dev/null; then \
		$(BUF) breaking --against '.git#branch=main'; \
	else \
		echo "breaking: main has no proto/ yet"; \
	fi

test:
	go test -race ./...

# Tests that need root: the guest's, which start processes as other users as
# PID 1 does, the image unpacker's, which give files owners, and vmm-exec's,
# which gives up root. They skip in make test unless it runs as root. CI runs
# this under sudo.
test-root:
	sudo -E env "PATH=$$PATH" go test -race -count=1 ./internal/guest/... ./internal/node/driver/chguest/ \
		./internal/node/driver/ocilayer/ ./internal/node/driver/registry/ ./internal/node/ \
		./internal/node/driver/chvm/

e2e:
	go test -race -tags e2e ./test/e2e/...

# Real-host tests (L4), on a testbed VM only: they boot guests as root.
# docs/runbook/testbed.md says what they need and how to provide it.
# Their hangar-edge has the testbed tag, so it answers a test CA's DNS-01
# challenges itself: never a release's.
# hangar-node is static, as a release builds it, and so is the probe that
# stands in for Cloud Hypervisor: a VM's unit has no dynamic loader. The
# vsock probe runs in a guest, from its root disk, so it is static too.
E2E_HOST_ROOTFS_IMAGE := docker:29-dind@sha256:4c42359ca23de31b73b7a58396a9ca1f3b9c8e0ffbb309f34c3b14c12d889d69
# Named by the image's digest, so a new pin makes a new image.
E2E_HOST_ROOTFS := .cache/testbed/dind-$(word 2,$(subst @sha256:, ,$(E2E_HOST_ROOTFS_IMAGE))).ext4

e2e-host: guest-initramfs $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/ ./cmd/hangar-node ./cmd/hangar-apiserver ./cmd/hangar
	CGO_ENABLED=0 go build -tags testbed -o bin/hangar-edge ./cmd/hangar-edge
	CGO_ENABLED=0 go build -tags testbed -o bin/ch-probe ./test/testbed/vms/chprobe
	CGO_ENABLED=0 go build -tags testbed -o bin/vsock-probe ./test/testbed/guest/vsockprobe
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -p 1 -timeout 30m -v ./test/testbed/...

# The real hypervisor driver's tests alone, with every other real driver.
e2e-host-vms: guest-initramfs $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/hangar-node ./cmd/hangar-node
	CGO_ENABLED=0 go build -tags testbed -o bin/ch-probe ./test/testbed/vms/chprobe
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -timeout 30m -v ./test/testbed/vms/ $(if $(RUN),-run '$(RUN)')

# The full flow alone: the real apiserver, node and CLI on every real driver.
e2e-host-flow: guest-initramfs $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/ ./cmd/hangar-node ./cmd/hangar-apiserver ./cmd/hangar
	CGO_ENABLED=0 go build -tags testbed -o bin/hangar-edge ./cmd/hangar-edge
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -timeout 30m -v ./test/testbed/flow/ $(if $(RUN),-run '$(RUN)')

# The real network driver's tests alone: root, but no KVM and no guest
# artifacts.
e2e-host-network:
	go test -exec 'sudo -E' -tags testbed -count=1 -timeout 15m -v ./test/testbed/network/

$(E2E_HOST_ROOTFS):
	mkdir -p $(@D)
	sudo env "PATH=$$PATH" test/testbed/guest-rootfs.sh $(E2E_HOST_ROOTFS_IMAGE) $@

# Regenerates into the working tree and fails if anything changed, so it holds
# for uncommitted work as well as in CI.
gen-check: deps
	@snap=$$(mktemp -d) && cp -r --parents $(GENERATED) $$snap/ && \
	$(MAKE) --no-print-directory gen >/dev/null && \
	if ! for d in $(GENERATED); do diff -r --exclude=node_modules $$snap/$$d $$d || exit 1; done; then \
		echo "gen-check: generated code is stale; run 'make gen' and commit the result"; \
		rm -rf $$snap; exit 1; \
	fi; rm -rf $$snap

check: lint breaking test test-web gen-check

# The console (web/): its types, its lint and its unit tests.
test-web: deps
	pnpm --dir web typecheck
	pnpm --dir web lint
	pnpm --dir web test

dev:
	go run ./cmd/hangar-apiserver --auth-mode=dev

# What a host runs, in bin/release/hangar-<revision>/ with SHA256SUMS: the
# binaries, the guest kernel and the initramfs, the console's files as
# console.tar and deploy/ as deploy.tar (docs/runbook/release.md). A host
# installs only releases the release workflow published; this one is for a
# testbed. Builds the kernel, so it takes
# minutes and needs a C toolchain, and the console, so it needs Node and pnpm.
# OUT=DIR writes elsewhere; ALLOW_DIRTY=1 builds from a tree with uncommitted
# changes.
release:
	deploy/release.sh $(if $(ALLOW_DIRTY),--allow-dirty) $(OUT)

# Not part of check: it takes minutes and needs a C toolchain.
guest-kernel:
	guest/kernel/build.sh

# INIT_MAIN=DIR packs another main package as /init (see docs/runbook/guest-kernel.md).
guest-initramfs:
	guest/initramfs/build.sh $(if $(INIT_MAIN),--main $(INIT_MAIN))

tools:
	@go tool protoc-gen-go --version
	@go list -m -f '{{.Path}} {{.Version}}' connectrpc.com/connect
	@$(BUF) --version | sed 's/^/buf /'
	@$(TOOL) sqlc version | sed 's/^/sqlc /'
	@$(GOLANGCI_LINT) version --short | sed 's/^/golangci-lint /'
	@node --version | sed 's/^/node /'
	@pnpm --version | sed 's/^/pnpm /'
