#!/usr/bin/env bash
# E2E test script for call-vpn relay-to-relay mode
#
# Usage: ./test-e2e.sh [--n=4] [--monitor=5] [--download-url=URL]
#
# Options:
#   --n=N             Number of parallel connections (default: 4)
#   --monitor=MIN     Enable periodic connectivity checks every 60s for MIN minutes.
#                     Without this flag, the script runs speed tests and exits.
#   --download-url=U  URL for large file download test (default: https://linkmeter.net)
#
# Examples:
#   ./test-e2e.sh                           # quick: 4 conns, speed test only
#   ./test-e2e.sh --n=1                     # baseline: 1 conn, speed test only
#   ./test-e2e.sh --n=4 --monitor=5         # 4 conns + 5 min monitoring
#   ./test-e2e.sh --download-url=http://example.com/100MB.bin

set -euo pipefail
cd "$(dirname "$0")"

# --- Config ---
CONNS=4
MONITOR_MIN=0
SOCKS_PORT=2080
HTTP_PORT=3080
DOWNLOAD_URL="https://linkmeter.net"

for arg in "$@"; do
  case $arg in
    --n=*) CONNS="${arg#*=}" ;;
    --monitor=*) MONITOR_MIN="${arg#*=}" ;;
    --download-url=*) DOWNLOAD_URL="${arg#*=}" ;;
  esac
done

# --- Load .env ---
if [[ ! -f .env ]]; then
  echo "FATAL: .env not found. See GET_TOKEN.md"
  exit 1
fi
# Parse .env safely (handles values with $ and special chars)
while IFS='=' read -r key val; do
  [[ -z "$key" || "$key" =~ ^# ]] && continue
  val="${val%\"}" ; val="${val#\"}"
  val="${val%\'}" ; val="${val#\'}"
  export "$key=$val"
done < <(grep -v '^\s*#' .env | grep '=')

for var in VK_CALL_LINK VPN_TOKEN VK_TOKEN_1; do
  if [[ -z "${!var:-}" ]]; then
    echo "FATAL: $var not set in .env"
    exit 1
  fi
done

# --- Helpers ---
ts() { date +"%H:%M:%S"; }

speed_fmt() {
  local bps=$1
  if (( $(echo "$bps > 1000000" | bc -l 2>/dev/null || echo 0) )); then
    echo "$(echo "scale=2; $bps * 8 / 1000000" | bc) Mbps"
  elif (( $(echo "$bps > 1000" | bc -l 2>/dev/null || echo 0) )); then
    echo "$(echo "scale=1; $bps / 1024" | bc) KB/s"
  else
    echo "${bps} B/s"
  fi
}

check_connectivity() {
  local result
  result=$(curl -x socks5://127.0.0.1:$SOCKS_PORT -s -o /dev/null \
    -w "%{http_code} %{time_total}" \
    --connect-timeout 10 --max-time 15 \
    http://httpbin.org/get 2>/dev/null) || true
  local code=$(echo "$result" | awk '{print $1}')
  local time=$(echo "$result" | awk '{print $2}')
  if [[ "$code" == "200" ]]; then
    echo "[$(ts)] CONNECTIVITY: OK (${time}s)"
    return 0
  else
    echo "[$(ts)] CONNECTIVITY: FAIL (code=$code, ${time}s)"
    return 1
  fi
}

# run_curl_speed: runs a single curl speed measurement
# Args: $1=label, $2=url, $3=max_time
run_curl_speed() {
  local label=$1 url=$2 max_time=${3:-30}
  echo -n "[$(ts)] ${label}: "
  local r
  r=$(curl -x socks5://127.0.0.1:$SOCKS_PORT -s -o /dev/null \
    -w "%{size_download} %{speed_download} %{time_total}" \
    --connect-timeout 15 --max-time "$max_time" \
    "$url" 2>/dev/null) || true
  local size=$(echo "$r" | awk '{print $1}')
  local speed=$(echo "$r" | awk '{print $2}')
  local ttime=$(echo "$r" | awk '{print $3}')
  if [[ -n "$speed" && "$speed" != "0" && "$speed" != "0.000" ]]; then
    echo "${size}b in ${ttime}s — $(speed_fmt "$speed")"
  else
    echo "FAILED (${size:-0}b, ${ttime:-timeout}s)"
  fi
}

run_speed_test() {
  local label=$1
  echo ""
  echo "====== SPEED TEST ($label) ======"
  echo "[$(ts)] conns=$CONNS"
  run_curl_speed "linkmeter.net" "https://linkmeter.net" 30
  run_curl_speed "httpbin 100KB" "http://httpbin.org/stream-bytes/102400" 30
  echo "================================="
}

run_download_test() {
  echo ""
  echo "====== DOWNLOAD TEST ======"
  echo "[$(ts)] conns=$CONNS url=$DOWNLOAD_URL"

  # Test via HTTP proxy (more reliable for large downloads)
  echo -n "[$(ts)] HTTP proxy: "
  local rh
  rh=$(curl -x http://127.0.0.1:$HTTP_PORT -s -o /dev/null \
    -w "%{size_download} %{speed_download} %{time_total}" \
    --connect-timeout 15 --max-time 60 \
    "$DOWNLOAD_URL" 2>/dev/null) || true
  local sizeh=$(echo "$rh" | awk '{print $1}')
  local speedh=$(echo "$rh" | awk '{print $2}')
  local ttimeh=$(echo "$rh" | awk '{print $3}')
  if [[ -n "$speedh" && "$speedh" != "0" && "$speedh" != "0.000" ]]; then
    local human_sizeh
    if (( $(echo "$sizeh > 1048576" | bc -l 2>/dev/null || echo 0) )); then
      human_sizeh="$(echo "scale=1; $sizeh / 1048576" | bc) MB"
    elif (( $(echo "$sizeh > 1024" | bc -l 2>/dev/null || echo 0) )); then
      human_sizeh="$(echo "scale=0; $sizeh / 1024" | bc) KB"
    else
      human_sizeh="${sizeh}b"
    fi
    echo "${human_sizeh} in ${ttimeh}s — $(speed_fmt "$speedh")"
  else
    echo "FAILED (${sizeh:-0}b, ${ttimeh:-timeout}s)"
  fi

  # Test via SOCKS5 proxy
  echo -n "[$(ts)] SOCKS5:     "
  local r
  r=$(curl -x socks5://127.0.0.1:$SOCKS_PORT -s -o /dev/null \
    -w "%{size_download} %{speed_download} %{time_total}" \
    --connect-timeout 15 --max-time 60 \
    "$DOWNLOAD_URL" 2>/dev/null) || true
  local size=$(echo "$r" | awk '{print $1}')
  local speed=$(echo "$r" | awk '{print $2}')
  local ttime=$(echo "$r" | awk '{print $3}')
  if [[ -n "$speed" && "$speed" != "0" && "$speed" != "0.000" ]]; then
    local human_size
    if (( $(echo "$size > 1048576" | bc -l 2>/dev/null || echo 0) )); then
      human_size="$(echo "scale=1; $size / 1048576" | bc) MB"
    elif (( $(echo "$size > 1024" | bc -l 2>/dev/null || echo 0) )); then
      human_size="$(echo "scale=0; $size / 1024" | bc) KB"
    else
      human_size="${size}b"
    fi
    echo "${human_size} in ${ttime}s — $(speed_fmt "$speed")"
  else
    echo "FAILED (${size:-0}b, ${ttime:-timeout}s)"
  fi
  echo "==========================="
}

cleanup() {
  echo ""
  echo "[$(ts)] Shutting down gracefully (SIGINT → wait 5s → SIGKILL)..."
  kill -INT $CLIENT_PID 2>/dev/null || true
  sleep 3
  kill -INT $SERVER_PID 2>/dev/null || true
  sleep 5
  kill -9 $CLIENT_PID 2>/dev/null || true
  kill -9 $SERVER_PID 2>/dev/null || true
  wait $CLIENT_PID 2>/dev/null || true
  wait $SERVER_PID 2>/dev/null || true
  echo "[$(ts)] Done."
}
trap cleanup EXIT

# --- Build ---
echo "[$(ts)] Building binaries..."
go build -o callvpn-server.exe ./cmd/server
go build -o callvpn-client.exe ./cmd/client
echo "[$(ts)] Build OK"

# --- Kill stale processes ---
taskkill //F //IM callvpn-server.exe >/dev/null 2>&1 || true
taskkill //F //IM callvpn-client.exe >/dev/null 2>&1 || true
sleep 1

# --- Start server ---
echo ""
echo "[$(ts)] Starting server (n=$CONNS)..."
./callvpn-server.exe \
  --link="$VK_CALL_LINK" \
  --tcp=true \
  --n="$CONNS" \
  --token="$VPN_TOKEN" \
  2>&1 | sed "s/^/  [server] /" &
SERVER_PID=$!
sleep 12

if ! kill -0 $SERVER_PID 2>/dev/null; then
  echo "FATAL: server died"
  exit 1
fi
echo "[$(ts)] Server running (PID $SERVER_PID)"

# --- Start client ---
echo ""
echo "[$(ts)] Starting client (n=$CONNS)..."
./callvpn-client.exe \
  --link="$VK_CALL_LINK" \
  --n="$CONNS" \
  --tcp=true \
  --token="$VPN_TOKEN" \
  --socks5-port=$SOCKS_PORT \
  --http-port=$HTTP_PORT \
  --vk-token="$VK_TOKEN_1" \
  2>&1 | sed "s/^/  [client] /" &
CLIENT_PID=$!

# --- Wait for proxy ---
echo ""
echo "[$(ts)] Waiting for proxy (socks5://127.0.0.1:$SOCKS_PORT)..."
READY=0
for i in $(seq 1 90); do
  if ! kill -0 $CLIENT_PID 2>/dev/null; then
    echo "FATAL: client died"
    exit 1
  fi
  if curl -x socks5://127.0.0.1:$SOCKS_PORT -s -o /dev/null \
       -w "%{http_code}" --connect-timeout 3 --max-time 5 \
       http://httpbin.org/get 2>/dev/null | grep -q 200; then
    READY=1
    echo "[$(ts)] Proxy ready after ${i} attempts (~$((i*2))s)"
    break
  fi
  sleep 2
done

if [[ $READY -ne 1 ]]; then
  echo "FATAL: proxy not ready after 3 minutes"
  exit 1
fi

# --- Speed test ---
run_speed_test "n=$CONNS"

# --- Download test ---
run_download_test

# --- Monitoring (optional) ---
if [[ $MONITOR_MIN -gt 0 ]]; then
  echo ""
  echo "====== MONITORING (${MONITOR_MIN} min, check every 60s) ======"
  START_TIME=$(date +%s)
  END_TIME=$((START_TIME + MONITOR_MIN * 60))
  MINUTE=0

  while true; do
    sleep 60
    MINUTE=$((MINUTE + 1))
    NOW=$(date +%s)

    if ! kill -0 $SERVER_PID 2>/dev/null; then
      echo "[$(ts)] WARNING: server died at T+${MINUTE}m"
      break
    fi
    if ! kill -0 $CLIENT_PID 2>/dev/null; then
      echo "[$(ts)] WARNING: client died at T+${MINUTE}m"
      break
    fi

    echo -n "T+${MINUTE}m "
    check_connectivity

    if [[ $NOW -ge $END_TIME ]]; then
      break
    fi
  done

  # Final speed test after monitoring
  if kill -0 $CLIENT_PID 2>/dev/null; then
    run_speed_test "final (T+${MONITOR_MIN}m)"
  else
    echo "[$(ts)] Skipping final speed test — client not running"
  fi
fi

echo ""
echo "====== TEST COMPLETE ======"
