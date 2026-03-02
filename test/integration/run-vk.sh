#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

# Parse VK call link from argument or .env
if [ $# -ge 1 ]; then
    link="$1"
    # Extract ID from full URL (https://vk.com/call/join/<id>)
    link="${link##*/call/join/}"
    # Strip trailing slashes or query params
    link="${link%%[?#]*}"
    link="${link%%/}"
else
    if [ -f .env ]; then
        # shellcheck source=/dev/null
        source .env
    fi
    if [ -z "${VK_CALL_LINK:-}" ]; then
        echo "Usage: $0 [<vk-call-link-url-or-id>]"
        echo "Or set VK_CALL_LINK in test/integration/.env"
        exit 1
    fi
    link="$VK_CALL_LINK"
fi

export VK_CALL_LINK="$link"
echo "=== VK TURN integration test ==="
echo "Call link ID: $VK_CALL_LINK"

echo "=== Building containers ==="
docker compose -f docker-compose.vk.yml build --parallel

echo "=== Starting tests ==="
docker compose -f docker-compose.vk.yml up --abort-on-container-exit --exit-code-from test-runner
exit_code=$?

echo "=== Cleaning up ==="
docker compose -f docker-compose.vk.yml down -v --remove-orphans

exit $exit_code
