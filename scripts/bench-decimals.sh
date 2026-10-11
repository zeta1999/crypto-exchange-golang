#!/usr/bin/env bash
#
# Bench the two pkg/decimal backends against each other (PLAN §9.7 A/B):
# the exact 128-bit core (default build) vs the float64 back-end
# (build tag decimal_float64). Prints a small table and appends the
# result to docs/BENCHMARKS.md under a machine-managed marker block so the
# doc always shows the latest numbers.
#
# Usage: scripts/bench-decimals.sh
set -euo pipefail
cd "$(dirname "$0")/.."

TIME="${1:-10000000x}"   # override: scripts/bench-decimals.sh 200000000x

run_bench() { # $1=tags (empty or "decimal_float64"), $2=bench regex
  local out
  out=$(go test ${1:+-tags $1} -run '^$' -bench "$2" -benchtime "$TIME" \
        -count=1 -benchmem ./pkg/decimal/ 2>&1 | grep -E '^Benchmark.*-?[0-9]+ ' \
        | sort)
  # ns/op per bench name (strip the -N CPU-suffix golang appends)
  echo "$out" | awk '{n=$1; sub(/-[0-9]+$/, "", n); printf "%s|%s\n", n, $3}'
}

exact=$(run_bench '' 'Benchmark(Mul|Div)$')
float=$(run_bench 'decimal_float64' 'BenchmarkFloat(Mul|AddDiv)$')

get() { grep "^$1|" <<<"$2" | cut -d'|' -f2; }

em=$(get BenchmarkMul "$exact");        ed=$(get BenchmarkDiv "$exact")
fm=$(get BenchmarkFloatMul "$float");   fa=$(get BenchmarkFloatAddDiv "$float")

echo "== pkg/decimal A/B ($TIME/op, $(date -u +%Y-%m-%dT%H:%M:%SZ)) =="
printf '%-18s %s\n' "op" "ns/op"
printf '%-18s %s\n' "exact Mul"     "${em:-n/a}"
printf '%-18s %s\n' "exact Div"     "${ed:-n/a}"
printf '%-18s %s\n' "float Mul"     "${fm:-n/a}"
printf '%-18s %s\n' "float Add+Div" "${fa:-n/a}"

# Append/replace the machine-managed block in docs/BENCHMARKS.md.
mkdir -p docs
DOC=docs/BENCHMARKS.md
[ -f "$DOC" ] || cp /dev/null "$DOC"
awk '/<!-- BEGIN:DECIMAL-AB-AUTO/{skip=1}
     /<!-- END:DECIMAL-AB-AUTO/{skip=0; next}
     !skip' "$DOC" > "$DOC.tmp"
cat >> "$DOC.tmp" <<EOF
<!-- BEGIN:DECIMAL-AB-AUTO (managed by scripts/bench-decimals.sh — do not edit) -->
\`\`\`
pkg/decimal A/B — $(date -u +%Y-%m-%dT%H:%M:%SZ), benchtime $TIME/op
exact 128-bit core   Mul   ${em:-n/a}   Div   ${ed:-n/a}
float64 back-end     Mul   ${fm:-n/a}   Add+Div ${fa:-n/a}
\`\`\`
<!-- END:DECIMAL-AB-AUTO -->
EOF
mv "$DOC.tmp" "$DOC"
echo "updated $DOC"
