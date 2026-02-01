# Repository Guidelines

## Project Structure & Module Organization
- `cmd/exchange/` holds runnable entrypoints (matching engine, risk daemon, gateway). Keep binaries slim; delegate logic to `internal/`.
- `internal/engine/`, `internal/risk/`, and `internal/liquidity/` contain matching, risk, and market data domains. Use clear package names (`orderbook`, `positions`) and keep cross-domain calls behind interfaces.
- Shared clients, math helpers, and protocol codecs go in `pkg/` so they remain importable by future tooling.
- Configuration templates live in `configs/` (`dev.yaml`, `prod.yaml`). Store fixtures under `testdata/`, and keep infrastructure manifests (Dockerfiles, Terraform) under `deploy/`.

## Build, Test, and Development Commands
- `go build ./cmd/exchange` compiles the primary binary with current module deps.
- `go test ./... -race -count=1` runs all unit tests with the race detector for deterministic results.
- `go test ./internal/engine -run OrderBook -cover` is useful for targeted work; keep example commands like this in PR descriptions.
- `golangci-lint run ./...` enforces formatting (`gofmt`), vet, ineffassign, and staticcheck in one pass.
- `GOENV=dev go run ./cmd/exchange --config configs/dev.yaml` starts a local node against mock feeds.

## Coding Style & Naming Conventions
- Always run `gofmt` or `goimports` on save; use tabs for indentation as required by Go tooling.
- Favor small, explicit interfaces (e.g., `type MatchingEngine interface { Submit(*Order) error }`).
- Use CamelCase for exported symbols, lower_snake for JSON/YAML keys, and suffix goroutine helpers with `Loop` to show long-lived behavior.
- Return struct pointers for mutable domain entities (`*Order`, `*Position`) and keep constructor helpers in the same package.

## Testing Guidelines
- Co-locate `_test.go` files with the code they cover and use table-driven tests named `Test<Component>_<Scenario>`.
- Use `testify/require` for invariants and keep golden books in `testdata/<package>/`.
- Target ≥85% package coverage; add `go test ./... -coverprofile coverage.out` to vet complex refactors before review.

## Commit & Pull Request Guidelines
- Follow Conventional Commits (`feat(engine): add iceberg support`). Keep subject < 72 chars and explain the why in the body.
- Every PR must describe behavior changes, include `go test` output, and link relevant issues. Attach screenshots or logs for API surfaces.
- Rebase before merging; avoid merge commits on `main` to keep bisects clean.

## Security & Configuration Tips
- Never commit live API keys; load them via environment variables referenced in `configs/*.yaml` placeholders.
- Keep secrets encrypted with `sops` or your preferred KMS under `deploy/secrets/` and document decryption steps in the PR when needed.
