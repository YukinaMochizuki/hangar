TOOL := go tool -modfile=tools/tools.mod
BUF := $(TOOL) buf
GOLANGCI_LINT := $(TOOL) golangci-lint

# Paths whose contents are produced by `make gen` and must never drift.
GENERATED := gen internal/apiserver/servicegen internal/store/sqlite/db

.PHONY: all gen check lint lint-proto lint-go lint-sh breaking test test-root test-web test-monitoring e2e test-e2e-race gen-check dev tools deps guest-kernel guest-initramfs guest-initramfs-testbed guest-converter guest-e2fsprogs host-swtpm e2e-host e2e-host-network e2e-host-vms e2e-host-flow e2e-host-images release

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

# The CLI's installer is POSIX sh, run by macOS's sh and dash alike.
lint-sh:
	@if [ -n "$(SHELL_SCRIPTS)" ]; then ./tools/shellcheck.sh --shell=bash -x $(SHELL_SCRIPTS); fi
	./tools/shellcheck.sh --shell=sh deploy/cli/install.sh.in

# The CLI is also released for macOS and Windows (deploy/release.sh), so
# its packages are linted, and so type-checked, as built for each, and so
# are its installers' tests, which run on Windows too. GOOS goes to the
# linter only: `go tool -n` builds it for this host and names it.
lint-go:
	$(GOLANGCI_LINT) run ./...
	lint=$$($(TOOL) -n golangci-lint) && \
		GOOS=darwin $$lint run ./cmd/hangar/... ./internal/cli/... && \
		GOOS=windows $$lint run ./cmd/hangar/... ./internal/cli/... ./test/install/...

# Compares against main; skipped while main has no schema to compare with.
breaking:
	@if git cat-file -e main:proto 2>/dev/null; then \
		$(BUF) breaking --against '.git#branch=main'; \
	else \
		echo "breaking: main has no proto/ yet"; \
	fi

# test/e2e runs without the race detector, and test-e2e-race with it ("Run in
# parallel" in docs/conventions/testing.md says why).
E2E_PKG := github.com/YukinaMochizuki/hangar/test/e2e

# The race detector, but not on the SQLite driver: SQLite translated to Go,
# whose instrumented memory accesses cost a third of a race run's CPU.
RACE := -race -gcflags=modernc.org/...=-race=false

test:
	go test $(RACE) -timeout 20m $$(go list ./... | grep -v -x -F $(E2E_PKG))
	go test -timeout 10m ./test/e2e/

# test/e2e under the race detector, one test at a time: about eleven minutes
# on 16 cores (2026-10).
test-e2e-race:
	go test $(RACE) -parallel 1 -timeout 40m ./test/e2e/

# Tests that need root: the guest's, which start processes as other users as
# PID 1 does, the image unpacker's, which give files owners, and vmm-exec's,
# which gives up root. They skip in make test unless it runs as root. CI runs
# this under sudo.
test-root:
	sudo -E env "PATH=$$PATH" go test $(RACE) -count=1 ./internal/guest/... ./internal/node/driver/chguest/ \
		./internal/node/driver/ocilayer/ ./internal/node/driver/registry/ ./internal/node/ \
		./internal/node/driver/chvm/

# test/e2e alone, as make test runs it.
e2e:
	go test -timeout 10m ./test/e2e/

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

e2e-host: guest-initramfs guest-initramfs-testbed guest-converter host-swtpm $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/ ./cmd/hangar-node ./cmd/hangar-apiserver ./cmd/hangar
	CGO_ENABLED=0 go build -tags testbed -o bin/hangar-edge ./cmd/hangar-edge
	CGO_ENABLED=0 go build -tags testbed -o bin/ch-probe ./test/testbed/vms/chprobe
	CGO_ENABLED=0 go build -tags testbed -o bin/vsock-probe ./test/testbed/guest/vsockprobe
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -p 1 -timeout 30m -v ./test/testbed/...

# The real hypervisor driver's tests alone, with every other real driver.
e2e-host-vms: guest-initramfs guest-converter host-swtpm $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/hangar-node ./cmd/hangar-node
	CGO_ENABLED=0 go build -tags testbed -o bin/ch-probe ./test/testbed/vms/chprobe
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -timeout 30m -v ./test/testbed/vms/ $(if $(RUN),-run '$(RUN)')

# The full flow alone: the real apiserver, node and CLI on every real driver.
e2e-host-flow: guest-initramfs guest-initramfs-testbed guest-converter $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/ ./cmd/hangar-node ./cmd/hangar-apiserver ./cmd/hangar
	CGO_ENABLED=0 go build -tags testbed -o bin/hangar-edge ./cmd/hangar-edge
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -timeout 30m -v ./test/testbed/flow/ $(if $(RUN),-run '$(RUN)')

# The image catalogue's VMs alone (deploy/catalog/images.yaml), one after
# another, minutes each: IMAGES names them, comma-separated, or all, which
# is the default (docs/runbook/vm-images.md).
e2e-host-images: guest-initramfs $(E2E_HOST_ROOTFS)
	CGO_ENABLED=0 go build -o bin/ ./cmd/hangar-node ./cmd/hangar-apiserver ./cmd/hangar
	CGO_ENABLED=0 go build -tags testbed -o bin/hangar-edge ./cmd/hangar-edge
	HANGAR_TESTBED_IMAGES=$(or $(IMAGES),all) \
	HANGAR_TESTBED_ROOTFS=$(or $(HANGAR_TESTBED_ROOTFS),$(abspath $(E2E_HOST_ROOTFS))) \
		go test -exec 'sudo -E' -tags testbed -count=1 -timeout 4h -v ./test/testbed/flow/ -run '^TestTheImageCatalogue$$'

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

check: lint breaking test test-web test-monitoring gen-check

# The console (web/): its types, its lint and its unit tests.
test-web: deps
	pnpm --dir web typecheck
	pnpm --dir web lint
	pnpm --dir web test

# The host's alerting rules and their tests, and the Prometheus and
# Alertmanager configurations deploy/host writes, by the promtool and amtool
# pinned with the host's binaries in deploy/host/versions.env.
test-monitoring:
	tools/check-monitoring.sh

dev:
	go run ./cmd/hangar-apiserver --auth-mode=dev

# A release, in bin/release/hangar-<revision>/: host/ is what a host runs,
# with SHA256SUMS (the binaries, the guest kernel and the initramfs, the
# console's files as console.tar and deploy/ as deploy.tar), and dist/ what
# the release workflow publishes, the CLI's archives, install.sh and
# install.ps1 among it
# (docs/runbook/release.md). HANGAR_RELEASE_TAG=TAG names it. A host
# installs only releases the release workflow published; this one is for a
# testbed, which takes host/. Builds the kernel, so it takes
# minutes and needs a C toolchain, and the console, so it needs Node and pnpm.
# OUT=DIR writes elsewhere; ALLOW_DIRTY=1 builds from a tree with uncommitted
# changes.
release:
	deploy/release.sh $(if $(ALLOW_DIRTY),--allow-dirty) $(OUT)

# Not part of check: it takes minutes and needs a C toolchain.
guest-kernel:
	guest/kernel/build.sh

# INIT_MAIN=DIR packs another main package as /init (see docs/runbook/guest-kernel.md).
guest-initramfs: guest-e2fsprogs
	guest/initramfs/build.sh $(if $(INIT_MAIN),--main $(INIT_MAIN))

# hangar-init with the testbed tag, whose FileShare servers run what the
# host asks, for the L4 tests alone (docs/runbook/testbed.md). Never on a
# host, never released.
guest-initramfs-testbed: guest-e2fsprogs
	HANGAR_INITRAMFS_OUT=$(abspath bin/guest-testbed) guest/initramfs/build.sh --tags testbed

# The static mke2fs both initramfs pack, built once before either, so that
# make -j does not build it twice into the same files.
guest-e2fsprogs:
	guest/e2fsprogs/build.sh

# The converter initramfs converter VMs boot: hangar-init and qemu-img from
# pinned Alpine packages (docs/runbook/converter.md). No C toolchain.
guest-converter:
	guest/converter/build.sh

# The swtpm bundle TPM VMs run: swtpm, its libraries and the musl loader
# from pinned Alpine packages (docs/runbook/swtpm.md), as bin/host/swtpm.tar
# and, for the L4 tests, unpacked as bin/host/swtpm. No C toolchain.
host-swtpm:
	deploy/swtpm/build.sh
	rm -rf bin/host/swtpm && tar -xf bin/host/swtpm.tar -C bin/host

tools:
	@go tool protoc-gen-go --version
	@go list -m -f '{{.Path}} {{.Version}}' connectrpc.com/connect
	@$(BUF) --version | sed 's/^/buf /'
	@$(TOOL) sqlc version | sed 's/^/sqlc /'
	@$(GOLANGCI_LINT) version --short | sed 's/^/golangci-lint /'
	@tools/prom-tool.sh promtool --version | sed -n 1p
	@tools/prom-tool.sh amtool --version 2>&1 | sed -n 1p
	@node --version | sed 's/^/node /'
	@pnpm --version | sed 's/^/pnpm /'
