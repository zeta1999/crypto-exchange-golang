#!/usr/bin/env bash
#
# ci.sh — local CI gate for crypto-exchange-golang.
# Not GitHub Actions: run this manually (or via `make ci`) before every commit.
#
# Stages: gofmt check -> go vet -> golangci-lint (if installed) -> build -> test -race.
# Exits non-zero on the first failing stage.
#
set -euo pipefail

cd "$(dirname "$0")"

# Colors (fall back to plain if not a tty).
if [ -t 1 ]; then
  BOLD=$(printf '\033[1m'); GREEN=$(printf '\033[32m'); RED=$(printf '\033[31m'); RESET=$(printf '\033[0m')
else
  BOLD=""; GREEN=""; RED=""; RESET=""
fi

step() { printf "\n%s==> %s%s\n" "$BOLD" "$1" "$RESET"; }
ok()   { printf "%s[ok]%s %s\n" "$GREEN" "$RESET" "$1"; }
fail() { printf "%s[FAIL]%s %s\n" "$RED" "$RESET" "$1"; exit 1; }

step "gofmt (formatting check)"
unformatted=$(gofmt -l . 2>/dev/null | grep -v '/grpc/' || true)
if [ -n "$unformatted" ]; then
  printf "These files are not gofmt-clean:\n%s\n" "$unformatted"
  fail "gofmt"
fi
ok "gofmt clean"

step "go vet"
go vet ./... || fail "go vet"
ok "go vet clean"

step "golangci-lint (optional)"
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run ./... || fail "golangci-lint"
  ok "golangci-lint clean"
else
  printf "golangci-lint not installed — skipping (install: https://golangci-lint.run)\n"
fi

step "go build ./..."
go build ./... || fail "go build"
ok "build clean"

step "go test ./... -race -count=1"
go test ./... -race -count=1 || fail "go test"
ok "tests pass"

printf "\n%sCI PASSED%s\n" "$GREEN" "$RESET"
