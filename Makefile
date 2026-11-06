.PHONY: build test test-integration test-coverage lint vuln tidy deps check-deps install-lint install-govulncheck install-hooks clean

# Tool versions. Keep in sync by hand with .github/workflows/ci.yml (and
# scheduled-security.yml for govulncheck); GitHub Actions cannot read them
# from here.
GOLANGCI_VERSION ?= v1.64.8
GOVULNCHECK_VERSION ?= v1.1.4

# Compile every package, then build each agent under cmd/<agent>/ into
# bin/<agent>. With no cmd/ directory yet, only the compile step runs.
build:
	go build ./...
	@for dir in cmd/*/; do \
		[ -d "$$dir" ] || continue; \
		name=$$(basename "$$dir"); \
		echo "==> bin/$$name"; \
		go build -o "bin/$$name" "./$$dir" || exit 1; \
	done

# Unit tests with the race detector and coverage. No network, no keys.
# Single test: make test TESTARGS='-run TestName ./...'
test:
	go test -race -cover $(TESTARGS) ./...

# Integration tests live behind the `integration` build tag and may need
# network access or provider keys; each test skips when its prerequisites
# are absent. Run them locally before review; do not punt them to CI.
test-integration:
	go test -race -tags=integration -count=1 $(TESTARGS) ./...

# HTML coverage report in coverage/.
test-coverage:
	@mkdir -p coverage
	go test -coverprofile=coverage/coverage.out ./...
	go tool cover -html=coverage/coverage.out -o coverage/coverage.html
	@echo "Coverage report: coverage/coverage.html"

# go vet plus golangci-lint (which also checks gofmt/goimports/gci).
# Read-only: it reports problems and never rewrites files.
lint: install-lint
	go vet ./...
	golangci-lint run

# Known-vulnerability scan of the module's dependencies.
vuln: install-govulncheck
	govulncheck ./...

tidy:
	go mod tidy

# Check for, and install where possible, the tools the targets above need.
deps: check-deps install-lint install-govulncheck install-hooks

check-deps:
	@command -v go > /dev/null || { echo "go is required: https://go.dev/dl/"; exit 1; }
	@echo "go: $$(go version)"

install-lint:
	@command -v golangci-lint > /dev/null || { \
		echo "Installing golangci-lint $(GOLANGCI_VERSION)..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_VERSION); \
	}

install-govulncheck:
	@command -v govulncheck > /dev/null || { \
		echo "Installing govulncheck $(GOVULNCHECK_VERSION)..."; \
		go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); \
	}

# Install git hooks (non-fatal on read-only checkouts / CI).
install-hooks:
	@if [ -d .git ] && [ -w .git/hooks ]; then \
		cp hooks/pre-push .git/hooks/pre-push && chmod +x .git/hooks/pre-push; \
		echo "Git hooks installed"; \
	fi

clean:
	rm -rf bin/ coverage/
