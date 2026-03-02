#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "=== Building integration test containers ==="
docker compose build --parallel

echo "=== Starting integration tests ==="
docker compose up --abort-on-container-exit --exit-code-from test-runner
exit_code=$?

echo "=== Cleaning up ==="
docker compose down -v --remove-orphans

exit $exit_code
