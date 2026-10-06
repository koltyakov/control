.DEFAULT_GOAL := help
.PHONY: help tidy deps deps-update deps-check go-update fmt fmt-check lint lint-hint lint-hint-all vet test test-race check cov test-coverage test-cov-check bench build build-all bundle install uninstall release-local release-check ci ci-compose vuln clean proto test-compose test-compose-webrtc test-compose-relay update update-status

GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR ?= bin
DIST_DIR ?= dist
COVER_PROFILE ?= tmp/coverage.out
COVER_HTML ?= tmp/coverage.html
COVERAGE_MIN ?= 50.0
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0
ACTIONLINT_VERSION ?= v1.7.12
PLATFORMS ?= linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64,windows/arm64
EXE := $(if $(filter windows,$(shell $(GO) env GOOS)),.exe,)

help:
	@echo "Targets:"
	@echo "  make deps / tidy         - Download dependencies / tidy go.mod"
	@echo "  make deps-update         - Update dependencies to latest minor/patch versions"
	@echo "  make deps-check          - List available dependency updates"
	@echo "  make go-update           - Update the Go requirement to the latest stable release"
	@echo "  make fmt / fmt-check     - Format Go files / check formatting"
	@echo "  make lint                - Run pinned golangci-lint"
	@echo "  make lint-hint[-all]     - Run gopls hints on changed / all Go files"
	@echo "  make vet / test          - Run vet / native tests"
	@echo "  make test-race / check   - Race tests / vet plus race tests"
	@echo "  make cov                - Generate coverage profile and HTML"
	@echo "  make test-cov-check      - Enforce $(COVERAGE_MIN)% coverage"
	@echo "  make bench              - Run Go benchmarks"
	@echo "  make build / install    - Build / install the native CLI"
	@echo "  make build-all / bundle - Build all six platform binaries and a manifest"
	@echo "  make release-local      - Build and verify a local release bundle"
	@echo "  make release-check      - Verify the existing bundle in $(DIST_DIR)"
	@echo "  make vuln               - Scan dependencies for vulnerabilities"
	@echo "  make ci / ci-compose    - Local CI checks / CI plus Compose integration"
	@echo "  make test-compose       - Test WebRTC and relay deployments"
	@echo "  make update             - Push local build to gateway and all enrolled nodes"
	@echo "  make update-status      - Show managed rollout status"
	@echo "  make uninstall          - Run the per-user uninstaller"
	@echo "  make clean              - Remove local build and coverage output"

tidy:
	$(GO) mod tidy

deps:
	$(GO) mod download
	$(GO) mod tidy

deps-update:
	$(GO) get -u -t ./...
	$(GO) mod tidy

deps-check:
	$(GO) list -m -u all

go-update:
	$(GO) get go@latest

fmt:
	gofmt -w -s .

fmt-check:
	@files="$$(gofmt -s -l .)"; \
	if [ -n "$$files" ]; then printf 'Unformatted Go files:\n%s\n' "$$files"; exit 1; fi

lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m

lint-hint:
	@command -v gopls >/dev/null 2>&1 || { echo "Install gopls: go install golang.org/x/tools/gopls@latest"; exit 1; }
	@set -eu; files=$$(mktemp); trap 'rm -f "$$files"' EXIT HUP INT TERM; \
	{ git diff --name-only --diff-filter=ACMR -- '*.go'; \
	  git diff --cached --name-only --diff-filter=ACMR -- '*.go'; \
	  git ls-files --others --exclude-standard -- '*.go'; } | sort -u > "$$files"; \
	while IFS= read -r file; do gopls check -severity=hint "$$file"; done < "$$files"

lint-hint-all:
	@command -v gopls >/dev/null 2>&1 || { echo "Install gopls: go install golang.org/x/tools/gopls@latest"; exit 1; }
	@find cmd internal skills tests -name '*.go' -print0 | xargs -0 -n 1 gopls check -severity=hint

bundle:
	$(GO) run ./cmd/control-bundle --out "$(DIST_DIR)" --platforms "$(PLATFORMS)" $(if $(VERSION),--version "$(VERSION)",)

build-all: bundle

release-local: bundle
	$(MAKE) release-check

release-check:
	$(GO) run ./cmd/control-bundle --out "$(DIST_DIR)" --check

update: build
	"$(BIN_DIR)/control$(EXE)" update authorize --check
	$(MAKE) bundle
	"$(BIN_DIR)/control$(EXE)" update push "$(DIST_DIR)"

update-status: build
	"$(BIN_DIR)/control$(EXE)" update status

build:
	$(GO) run ./cmd/control-bundle --binary "$(BIN_DIR)/control$(EXE)" $(if $(VERSION),--version "$(VERSION)",)

install: build
	"$(BIN_DIR)/control$(EXE)" install-self

uninstall:
ifeq ($(OS),Windows_NT)
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/uninstall.ps1
else
	sh scripts/uninstall.sh
endif

test:
	$(GO) test ./... -timeout=120s

test-race:
	$(GO) test -race ./... -timeout=120s

vet:
	$(GO) vet ./...

check: vet test-race

cov:
	@mkdir -p "$(dir $(COVER_PROFILE))" "$(dir $(COVER_HTML))"
	$(GO) test -race -coverprofile="$(COVER_PROFILE)" -covermode=atomic ./... -timeout=120s
	$(GO) tool cover -html="$(COVER_PROFILE)" -o "$(COVER_HTML)"

test-coverage: cov

test-cov-check: cov
	@total="$$($(GO) tool cover -func="$(COVER_PROFILE)" | awk '/^total:/ { gsub(/%/, "", $$3); print $$3 }')"; \
	awk -v total="$$total" -v min="$(COVERAGE_MIN)" 'BEGIN { \
	  printf "Coverage %s%% (minimum %s%%)\n", total, min; exit !(total + 0 >= min + 0) }'

bench:
	$(GO) test ./... -run '^$$' -bench . -benchmem

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

ci: fmt-check lint check vuln build release-local
	$(GO) mod verify
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

ci-compose: ci
	CONTROL_TEST_SUITE=e2e $(MAKE) test-compose

clean:
	rm -rf "$(BIN_DIR)" "$(DIST_DIR)" "$(COVER_PROFILE)" "$(COVER_HTML)"

test-compose:
	sh tests/compose/test.sh

test-compose-webrtc:
	sh tests/compose/test.sh webrtc

test-compose-relay:
	sh tests/compose/test.sh relay

# Install protoc and protoc-gen-go before regenerating the checked-in bindings.
proto:
	protoc --go_out=. --go_opt=paths=source_relative internal/protocol/control.proto
