TOOL := go tool -modfile=tools/tools.mod
BUF := $(TOOL) buf
GOLANGCI_LINT := $(TOOL) golangci-lint

# Paths whose contents are produced by `make gen` and must never drift.
GENERATED := gen internal/apiserver/servicegen internal/store/sqlite/db

.PHONY: all gen check lint lint-proto lint-go lint-sh breaking test test-root e2e gen-check dev tools deps guest-kernel guest-initramfs

# Every shell script in the repo, committed or not, outside ignored paths.
SHELL_SCRIPTS = $(shell git ls-files --cached --others --exclude-standard '*.sh' 'deploy/host/*.env' 'deploy/host/hosts/*.env' 'guest/kernel/*.env')

all: check

deps: node_modules/.modules.yaml

node_modules/.modules.yaml: package.json pnpm-lock.yaml gen/ts/package.json
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
	@if [ -n "$$(go list ./... 2>/dev/null)" ]; then $(GOLANGCI_LINT) run ./...; fi

# Compares against main; skipped while main has no schema to compare with.
breaking:
	@if git cat-file -e main:proto 2>/dev/null; then \
		$(BUF) breaking --against '.git#branch=main'; \
	else \
		echo "breaking: main has no proto/ yet"; \
	fi

test:
	@if [ -n "$$(go list ./... 2>/dev/null)" ]; then go test -race ./...; fi

# The guest's tests that start processes as other users, as PID 1 does; they
# skip in make test unless it runs as root. CI runs this under sudo.
test-root:
	sudo -E env "PATH=$$PATH" go test -race -count=1 ./internal/guest/... ./internal/node/driver/chguest/

e2e:
	go test -race -tags e2e ./test/e2e/...

# Regenerates into the working tree and fails if anything changed, so it holds
# for uncommitted work as well as in CI.
gen-check: deps
	@snap=$$(mktemp -d) && cp -r --parents $(GENERATED) $$snap/ && \
	$(MAKE) --no-print-directory gen >/dev/null && \
	if ! for d in $(GENERATED); do diff -r --exclude=node_modules $$snap/$$d $$d || exit 1; done; then \
		echo "gen-check: generated code is stale; run 'make gen' and commit the result"; \
		rm -rf $$snap; exit 1; \
	fi; rm -rf $$snap

check: lint breaking test gen-check

dev:
	go run ./cmd/hangar-apiserver --auth-mode=dev

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
