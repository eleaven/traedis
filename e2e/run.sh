#!/usr/bin/env bash
# End-to-end tests: the plugin running inside Traefik (Yaegi), with Redis and the
# sample backends of compose.yml. Hurl (compose service `hurl`) runs the *.hurl
# files, in phases: it cannot stop Redis itself.
#
#   docker compose up -d --wait ingress cache cache-tls whoami placeholder imaging
#   ./e2e/run.sh
#
# The results are also written to e2e/junit.xml (see .github/scripts/e2e-summary.mjs).
# One file, with the stack up:
#   docker compose run --rm hurl --test /e2e/tests/key.hurl
set -euo pipefail

cd "$(dirname "$0")/.."
rm -f e2e/junit.xml # Hurl appends to an existing report
failures=0

# hurl <files or directories…>: the files of a directory run in parallel.
hurl() {
  # --user: the report belongs to the caller, not to root
  docker compose --progress quiet run --rm --user "$(id -u):$(id -g)" hurl \
    --test --max-time 10 --report-junit /report/junit.xml "$@" || failures=$((failures + 1))
}

# The stack has just started: its first requests may be too slow for
# `redis.dialTimeout`, which bypasses the cache. Waiting does not help, they have
# to be made: warmup.hurl asks until the cache answers.
echo "# Warm-up"
hurl /e2e/warmup.hurl

echo "# Redis up"
hurl /e2e/tests

echo "# Redis down"
docker compose --progress quiet stop cache
hurl /e2e/redis/fail-open.hurl
docker compose --progress quiet up -d --wait cache

echo "# Redis back"
hurl /e2e/redis/recovery.hurl

if [[ "$failures" -gt 0 ]]; then
  echo "$failures phase(s) failed"
  exit 1
fi
echo "All phases passed"
