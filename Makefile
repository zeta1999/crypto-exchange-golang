# Local developer targets. CI is ./ci.sh (NOT GitHub Actions).

.PHONY: ci fmt lint vet build test run feedcat tidy

ci:        ## Run the full local CI gate
	./ci.sh

fmt:       ## Format all Go code
	gofmt -w .

vet:       ## go vet
	go vet ./...

lint:      ## golangci-lint (if installed) — fails on real violations
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed"; fi

build:     ## Build all packages
	go build ./...

test:      ## Race tests
	go test ./... -race -count=1

run:       ## Run the native exchange node against dev config
	GOENV=dev go run ./cmd/exchange --config configs/dev.yaml

feedcat:   ## Run the feed inspector (lands in Phase 1)
	@if [ -d cmd/feedcat ]; then go run ./cmd/feedcat; else echo "feedcat lands in Phase 1 (cmd/feedcat not built yet)"; fi

tidy:      ## Tidy modules
	go mod tidy
