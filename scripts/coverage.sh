#!/usr/bin/env bash
# Usage: scripts/coverage.sh PROFILE MIN_PERCENT
# Prints per-package statement coverage of the core packages (merging the
# blocks reported by every test binary) and fails if the total is below
# MIN_PERCENT.
set -euo pipefail
profile=$1
min=$2
summary=$(awk 'NR > 1 {
  key = $1; stmts[key] = $2; if ($3 > 0) hit[key] = 1
} END {
  for (k in stmts) {
    file = k; sub(/:.*/, "", file)
    pkg = file; sub(/\/[^\/]*$/, "", pkg); sub(/^github.com\/archit2501\/plait\//, "", pkg)
    total[pkg] += stmts[k]; all += stmts[k]
    if (k in hit) { covered[pkg] += stmts[k]; allc += stmts[k] }
  }
  for (p in total) printf "  %-30s %6.1f%%\n", p, 100 * covered[p] / total[p]
  printf "TOTAL %.1f\n", 100 * allc / all
}' "$profile")
grep -v '^TOTAL' <<<"$summary" | sort
total=$(awk '/^TOTAL/ { print $2 }' <<<"$summary")
echo "total core coverage: ${total}% of statements (minimum ${min}%)"
awk -v t="$total" -v m="$min" 'BEGIN { exit (t + 0 >= m + 0) ? 0 : 1 }' || { echo "coverage below ${min}%"; exit 1; }
