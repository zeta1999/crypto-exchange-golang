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
# Exclude generated protobuf under grpc/ by filtering the file list BEFORE gofmt,
# so we never rely on grep's exit code (no-match => exit 1) under `set -e`/pipefail.
gofmt_targets=$(find . -name '*.go' -not -path './grpc/*' -not -path './.codegraph/*' -not -path './.stash/*')
unformatted=$(gofmt -l $gofmt_targets 2>/dev/null)
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

step "go test ./... -race -count=1 -timeout=120s"
go test ./... -race -count=1 -timeout=120s || fail "go test"
ok "tests pass"

step "float64 decimal backend A/B (build + vet + test -tags decimal_float64)"
# The float64 back-end (pkg/decimal, PLAN §9.7) must keep the WHOLE module
# compiling and vet-clean with zero call-site changes, and its tests must pass:
# that is the "callers are backend-agnostic" contract. Run the full test suite
# under the tag except two bit-exact golden tests that legitimately assert
# exact decimal strings (the float back-end is deliberately loose):
#   TestScenarioGoldenFile (emulator), TestHubWithdrawQuantizes (transfer).
# Any NEW failure there means the back-end introduced a type-level or
# semantic regression and blocks the gate.
go build -tags decimal_float64 ./... || fail "float64 build"
go vet -tags decimal_float64 ./... || fail "float64 vet"
go test -tags decimal_float64 -race -count=1 -timeout=300s \
  -skip 'TestHubWithdrawQuantizes|TestScenarioGoldenFile' \
  ./... || fail "float64 test"
ok "float64 back-end A/B clean"

printf "\n%sCI PASSED%s\n" "$GREEN" "$RESET"
