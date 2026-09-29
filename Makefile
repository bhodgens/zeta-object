# mini-s3 Makefile — meept-grade quality gates.
# Module is at the repo root (single package main): no cd needed anywhere.

BINARY_NAME := zeta-object-server

# Coverage floor. Measured 47.9% aggregate (go test -cover ./...) after the
# frontend-interface split (2026-09) moved code from package main into
# internal/frontend/s3 (own-package coverage there is 24.3%). The old floor of
# 70 was set when the whole S3 layer lived in package main and broke the moment
# the split landed. This floor must ratchet back up as tests land in the new
# packages - see AGENTS.md "Coverage floors move with code".
COVER_MIN := 47

# Lint only issues introduced since NEW_FROM_REV (any rev/ref):
#   make lint NEW_FROM_REV=HEAD
NEW_FROM_REV ?=

GO_TOOLS_MISSING :=

.PHONY: help build run certs data_dir clean \
        test test-verbose test-race test-cover test-cover-enforce fuzz bench \
        lint vet fmt fmt-check mod-tidy mod-tidy-check mod-verify \
        precommit check vuln secrets e2e hooks

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Build & run:"
	@echo "  build            Compile the Go server to ./$(BINARY_NAME)"
	@echo "  run              Build and start the server (HTTPS on :8443; certs must exist — run 'make certs' first)"
	@echo "  certs            Generate self-signed SSL certificates in certs/ (if missing)"
	@echo "  clean            Remove the compiled binary and coverage artifacts"
	@echo ""
	@echo "Testing:"
	@echo "  test             Run tests (count=1, coverage summary)"
	@echo "  test-verbose     Run tests with -v"
	@echo "  test-race        Run tests with the race detector"
	@echo "  test-cover       Run tests with HTML coverage report"
	@echo "  test-cover-enforce  Fail if coverage drops below $(COVER_MIN)%"
	@echo "  fuzz             Run fuzz targets (30s each)"
	@echo ""
	@echo "Quality gates:"
	@echo "  precommit        build + vet + fmt-check + lint + test + mod-tidy-check"
	@echo "  check            precommit + test-race + vuln + secrets (full local gate)"
	@echo "  lint             golangci-lint run ./... (NEW_FROM_REV=<rev> limits to new issues)"
	@echo "  vet              go vet ./..."
	@echo "  fmt              gofmt + goimports -local github.com/bhodgens/zeta-object"
	@echo "  fmt-check        Verify formatting without modifying files"
	@echo "  vuln             govulncheck ./..."
	@echo "  secrets          gitleaks detect (scripts/.gitleaks.toml)"
	@echo "  e2e              Run scripts/e2e/run-e2e.sh (created by leaf 3.6)"
	@echo "  conformance      Run ceph/s3-tests subset (leaf 5.1); ratchets vs scripts/conformance/baseline.txt"
	@echo ""
	@echo "Modules:"
	@echo "  mod-tidy         go mod tidy"
	@echo "  mod-tidy-check   Verify go.mod/go.sum are tidy"
	@echo "  mod-verify       go mod verify"
	@echo ""
	@echo "Hooks:"
	@echo "  hooks            Install git hooks (core.hooksPath -> .githooks); or run scripts/install-hooks.sh"

# =============================================================================
# Build & run
# =============================================================================

build:
	@echo "Building $(BINARY_NAME)..."
	@go build -o $(BINARY_NAME) .
	@echo "$(BINARY_NAME) built successfully."

# HTTPS on :8443. Requires certs/cert.pem + certs/key.pem — run 'make certs' first.
run: build
	@echo "Starting $(BINARY_NAME)..."
	@./$(BINARY_NAME)

certs:
	@if [ ! -f certs/cert.pem ] || [ ! -f certs/key.pem ]; then \
		echo "Generating self-signed SSL certificates (SAN: localhost, 127.0.0.1)..."; \
		mkdir -p certs; \
		openssl req -x509 -newkey rsa:4096 -nodes -out certs/cert.pem -keyout certs/key.pem -days 365 -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"; \
		echo "Certificates generated in certs/ directory."; \
	else \
		echo "Certificates already exist in certs/ directory (delete them and re-run to regenerate with SANs)."; \
	fi

# Ensure data directory exists (optional, as main.go also creates it)
data_dir:
	@mkdir -p data

clean:
	@echo "Cleaning up..."
	@rm -f $(BINARY_NAME) coverage.out coverage.html
	@rm -rf coverage
	@echo "Cleanup complete."

# =============================================================================
# Testing
# =============================================================================

test:
	@go test -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

test-verbose:
	@go test -v -count=1 ./...

test-race:
	@go test -race -count=1 ./...

test-cover:
	@mkdir -p coverage
	@go test -count=1 -coverprofile=coverage/coverage.out ./...
	@go tool cover -html=coverage/coverage.out -o coverage/coverage.html
	@go tool cover -func=coverage/coverage.out | tail -1
	@echo "Coverage report: coverage/coverage.html"

# Hard floor: the campaign must never regress below the current baseline.
test-cover-enforce:
	@mkdir -p coverage
	@go test -count=1 -coverprofile=coverage/coverage.out ./...
	@TOTAL=$$(go tool cover -func=coverage/coverage.out | tail -1 | awk '{print $$3}' | tr -d '%'); \
	echo "Total coverage: $$TOTAL% (floor: $(COVER_MIN)%)"; \
	awk -v t="$$TOTAL" -v m="$(COVER_MIN)" 'BEGIN { exit (t+0 >= m+0) ? 0 : 1 }' || { \
		echo "FAIL: coverage $$TOTAL% is below the $(COVER_MIN)% floor"; exit 1; }

# =============================================================================
# Quality gates
# =============================================================================

lint:
	@which golangci-lint > /dev/null 2>&1 || { echo "Install: brew install golangci-lint"; exit 1; }
	@echo "Running golangci-lint$(if $(NEW_FROM_REV), (new issues since $(NEW_FROM_REV)) only,)..."
	golangci-lint run ./... $(if $(NEW_FROM_REV),--new-from-rev=$(NEW_FROM_REV),)

vet:
	@echo "Running go vet..."
	@go vet ./...

fmt:
	@echo "Formatting code (gofmt + goimports -local github.com/bhodgens/zeta-object)..."
	@go fmt ./...
	@goimports -local github.com/bhodgens/zeta-object -w $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go')
	@echo "Done"

fmt-check:
	@echo "Checking formatting..."
	@UNFMT=$$(gofmt -l $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go')); \
	if [ -n "$$UNFMT" ]; then \
		echo "FAIL: files not gofmt-clean:"; \
		echo "$$UNFMT" | sed 's/^/  /'; \
		echo "Run 'make fmt' and re-stage."; \
		exit 1; \
	fi; \
	echo "gofmt clean."

mod-tidy:
	@echo "Tidying Go modules..."
	@go mod tidy

mod-tidy-check:
	@echo "Checking go.mod/go.sum are tidy..."
	@cp go.mod go.mod.bak-check && cp go.sum go.sum.bak-check 2>/dev/null || true
	@go mod tidy
	@DIFF=0; \
	cmp -s go.mod go.mod.bak-check || DIFF=1; \
	if [ -f go.sum.bak-check ]; then cmp -s go.sum go.sum.bak-check || DIFF=1; fi; \
	mv go.mod.bak-check go.mod; \
	[ -f go.sum.bak-check ] && mv go.sum.bak-check go.sum; \
	if [ "$$DIFF" -ne 0 ]; then \
		echo "FAIL: go.mod/go.sum are not tidy. Run 'make mod-tidy' and commit."; \
		exit 1; \
	fi; \
	echo "go.mod/go.sum tidy."

mod-verify:
	@echo "Verifying Go modules..."
	@go mod verify

# Fast gate: what every commit should pass.
precommit: build vet fmt-check lint test test-cover-enforce parity-test mod-tidy-check
	@echo ""
	@echo "precommit gate passed."

# S3 parity gate: the canonical S3 metadata surface must be identical for
# plain-FS and provider-attached buckets (metadata TestParity* suite), and
# objectmodel snapshot/header parity must hold (objectmodel parity suite).
parity-test: ## Run the S3 parity gates: metadata (FS vs provider-enabled) and objectmodel header parity
	go test ./internal/metadata/ -run 'TestParity' -count=1 -v
	go test ./internal/objectmodel/ -run 'TestSnapshotHeaders|TestAssertHeaderParity' -count=1 -v

# Full local gate: what a push should pass.
check: precommit test-race vuln secrets
	@echo ""
	@echo "check gate passed (build, vet, fmt, lint, test, tidy, race, vuln, secrets)."

vuln:
	@which govulncheck > /dev/null 2>&1 || { echo "govulncheck not installed, skipping. Install: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 0; }
	@echo "Running govulncheck..."
	@govulncheck ./...

secrets:
	@which gitleaks > /dev/null 2>&1 || { echo "gitleaks not installed, skipping. Install: brew install gitleaks"; exit 0; }
	@echo "Running gitleaks..."
	@gitleaks detect --source . --config scripts/.gitleaks.toml

# =============================================================================
# E2E (suite lands with leaf 3.6 — fail with a clear message until then)
# =============================================================================

e2e:
	@if [ -f scripts/e2e/run-e2e.sh ]; then \
		bash scripts/e2e/run-e2e.sh; \
	else \
		echo "FAIL: scripts/e2e/run-e2e.sh not found."; \
		echo "The e2e suite is created by leaf 3.6 (hardening plan) and will exist by end of campaign."; \
		exit 1; \
	fi

# =============================================================================
# Conformance (leaf 5.1 — ceph/s3-tests baseline, informational, NOT in check)
# =============================================================================

.PHONY: conformance
conformance: ## Run the vendored ceph/s3-tests subset; fails on regressions vs baseline.txt
	@bash scripts/conformance/run-conformance.sh

# =============================================================================
# Fuzz (leaf 4.7 — native fuzzing over the hand-rolled parsers)
# =============================================================================

fuzz: ## Run fuzz targets briefly (30s each). Full runs: go test -fuzz=. -fuzztime=10m ./...
	go test -fuzz=FuzzStripJSON5Comments -fuzztime=30s .
	go test -fuzz=FuzzParseRangeHeader    -fuzztime=30s .
	go test -fuzz=FuzzMatchPathGlob       -fuzztime=30s .
	go test -fuzz=FuzzParseCopySource     -fuzztime=30s .

# =============================================================================
# Benchmarks (leaf 4.10)
# =============================================================================

bench: ## Run all benchmarks once with allocation stats.
	go test -bench=. -benchmem -run='^$$' .

# =============================================================================
# Git hooks
# =============================================================================

hooks:
	@chmod +x .githooks/*
	@git config core.hooksPath .githooks
	@echo "Git hooks installed (core.hooksPath -> .githooks)."
	@echo "Bypass with --no-verify for emergencies only."
	@echo "Tip: scripts/install-hooks.sh also checks tool availability."
