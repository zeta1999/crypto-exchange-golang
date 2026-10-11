#!/usr/bin/env bash
#
# ci.sh — local CI gate for crypto-exchange-golang.
# Not GitHub Actions: run this manually (or via `make ci`) before every commit.
#
# Stages: gofmt check -> go vet -> golangci-lint (if installed) -> build -> test -race.
# Exits non-zero on the first failing stage.
#
# golangci-lint: installed via `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`
# (needs a v2 binary; a v1 binary is rejected by config version "2"). If absent,
# the stage skips — install is recommended so the gate is uniform across machines.
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
LintBin=$(command -v golangci-lint || true)
if [ -z "$LintBin" ] && [ -x "${HOME}/go/bin/golangci-lint" ]; then
  LintBin="${HOME}/go/bin/golangci-lint" # go install default location
fi
if [ -n "$LintBin" ]; then
  "$LintBin" run ./... || fail "golangci-lint"
  ok "golangci-lint clean"
else
  printf "golangci-lint not installed — skipping (install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest; see ci.sh header)\n"
fi

step "go build ./..."
go build ./... || fail "go build"
ok "build clean"

step "go test ./... -race -count=1 -timeout=120s"
# -short: the gate must be deterministic on any machine/load. The three
# testing.Short()-gated tests are the LIVE ones (outbound Coinbase feed +
# saga subprocess): under ~12 parallel -race packages they blow their
# self-imposed 90s engine deadline via CPU contention (5.5s in isolation
# vs >90s under load, observed). They remain available for on-demand runs:
# go test ./internal/api/fix -run TestLive -v
go test ./... -short -race -count=1 -timeout=120s || fail "go test"
ok "tests pass"

step "float64 decimal backend A/B (build + vet + test -tags decimal_float64)"
# The float64 back-end (pkg/decimal, PLAN §9.7) must keep the WHOLE module
# compiling and vet-clean with zero call-site changes, and its tests must pass:
# that is the "callers are backend-agnostic" contract. Run the full test suite
# under the tag except two bit-exact golden tests that legitimately assert
# exact decimal strings (the float back-end is deliberately loose):
#   TestScenarioGoldenFile (emulator), TestHubWithdrawQuantizes (transfer).
# The -skip regex IS anchored: unanchored, 'TestHubWithdrawQuantizes' would
# also match TestHubWithdrawDebitsAndSends (substring) and silently skip a
# passing test — masking any future drift there.
# Any NEW failure there means the back-end introduced a type-level or
# semantic regression and blocks the gate.
go build -tags decimal_float64 ./... || fail "float64 build"
go vet -tags decimal_float64 ./... || fail "float64 vet"
go test -tags decimal_float64 -short -race -count=1 -timeout=300s \
  -skip '^(TestHubWithdrawQuantizes|TestScenarioGoldenFile)$' \
  ./... || fail "float64 test"
ok "float64 back-end A/B clean"

printf "\n%sCI PASSED%s\n" "$GREEN" "$RESET"
