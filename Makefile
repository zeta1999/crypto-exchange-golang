# Local developer targets. CI is ./ci.sh (NOT GitHub Actions).

.PHONY: ci fmt lint vet build test run feedcat tidy

ci:        ## Run the full local CI gate
	./ci.sh

fmt:       ## Format all Go code
	gofmt -w .

vet:       ## go vet
	go vet ./...

lint:      ## golangci-lint (if installed)
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed"

build:     ## Build all packages
	go build ./...

test:      ## Race tests
	go test ./... -race -count=1

run:       ## Run the native exchange node against dev config
	GOENV=dev go run ./cmd/exchange --config configs/dev.yaml

feedcat:   ## Run the feed inspector (added in Phase 1)
	go run ./cmd/feedcat

tidy:      ## Tidy modules
	go mod tidy
