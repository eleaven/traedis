#!/usr/bin/env bash
# Benchmarks of the working tree, written as JSON to benchmark/results.json:
#   - "yaegi": benchmark_test.go, the handler called in-process under Yaegi (what
#     Traefik runs): time, bytes and allocations per operation;
#   - "k6": load.js, HTTP load through Traefik: each sample backend without the
#     cache (direct) and with it (hit, miss).
#
#   docker compose up -d --wait ingress cache whoami placeholder imaging
#   ./benchmark/run.sh
#
# BENCH (.) and BENCHTIME (2s) are passed to -bench and -benchtime; LOAD_VUS (10)
# and LOAD_SECONDS (10) are the concurrency and the duration of each k6 case.
set -euo pipefail

BENCH="${BENCH:-.}"
BENCHTIME="${BENCHTIME:-2s}"
export TRAEDIS_REDIS_DSN="${TRAEDIS_REDIS_DSN:-redis://localhost:6379/15}"

MODULE="github.com/eleaven/traedis"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RESULTS="$ROOT/benchmark/results.json"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if ! command -v yaegi >/dev/null; then
  echo "yaegi not found: go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1" >&2
  exit 1
fi

echo "# Yaegi: benchmark_test.go" >&2
# Yaegi only resolves the plugin's import path inside a GOPATH: a link to the
# working tree is enough.
mkdir -p "$TMP/src/$(dirname "$MODULE")"
ln -s "$ROOT" "$TMP/src/$MODULE"
(cd "$TMP/src/$MODULE/benchmark" &&
  GOPATH="$TMP" yaegi test -run '^$' -bench "$BENCH" -benchmem -benchtime "$BENCHTIME" .) | tee "$TMP/yaegi.txt" >&2

echo "# k6: load.js" >&2
(cd "$ROOT" && docker compose run --rm -T k6 run --quiet /benchmark/load.js) >"$TMP/k6.json"

{
  echo '{'
  echo '  "yaegi": {'
  # BenchmarkHit/1KB-12  1461  231135 ns/op  4224 B/op  51 allocs/op
  awk '/^Benchmark/ {
    name = $1
    sub(/^Benchmark/, "", name)
    sub(/-[0-9]+$/, "", name)
    printf "%s    \"%s\": { \"ns_per_op\": %s, \"bytes_per_op\": %s, \"allocs_per_op\": %s }", sep, name, $3, $5, $7
    sep = ",\n"
  } END { print "" }' <(LC_ALL=C sort -t/ -k1,1 -k2,2h "$TMP/yaegi.txt") # Yaegi runs them in random order: by name, then by size (1KB, 100KB, 1MB)
  echo '  },'
  printf '  "k6": '
  sed '1!s/^/  /' "$TMP/k6.json"
  echo '}'
} >"$RESULTS"
cat "$RESULTS"
