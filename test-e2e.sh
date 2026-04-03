#!/usr/bin/env bash
# E2E test script for call-vpn relay-to-relay mode
#
# Usage: ./test-e2e.sh [--n=4] [--links=1] [--monitor=5] [--download-url=URL] [--vk-token[=TOKEN]]
#
# Options:
#   --n=N             Number of parallel connections per call (default: 4)
#   --links=N         Number of call links to use (default: 1, reads VK_CALL_LINK_1..N from .env)
#   --monitor=MIN     Enable periodic connectivity checks every 60s for MIN minutes.
#                     Without this flag, the script runs speed tests and exits.
#   --download-url=U  URL for large file download test (default: https://linkmeter.net)
#   --vk-token        Use VK tokens from .env (VK_TOKEN_1, VK_TOKEN_2)
#   --vk-token=TOKEN  Use specified VK token for both server and client
#
# Examples:
#   ./test-e2e.sh --n=1                     # anonymous: 1 call × 1 conn
#   ./test-e2e.sh --n=4                     # anonymous: 1 call × 4 conns
#   ./test-e2e.sh --n=1 --vk-token          # token from .env: 1 call × 1 conn
#   ./test-e2e.sh --n=4 --vk-token=vk1.a.X  # explicit token: 1 call × 4 conns
#   ./test-e2e.sh --n=4 --monitor=5         # anonymous + 5 min monitoring
#   ./test-e2e.sh --n=2 --links=2           # 2 calls × 2 conns = 4 total
#   ./test-e2e.sh --download-url=http://example.com/100MB.bin

set -euo pipefail
cd "$(dirname "$0")"

# --- Config ---
CONNS=4
LINKS=1
MONITOR_MIN=0
SOCKS_PORT=2080
HTTP_PORT=3080
DOWNLOAD_URL="https://cdn.kernel.org/pub/linux/kernel/v6.x/patch-6.12.xz"
USE_VK_TOKEN=""    # empty=anonymous, "env"=from .env, "vk1.a.X"=explicit
VERBOSE=""
STRIPING=""
CAPTCHA_PORT=18090

for arg in "$@"; do
  case $arg in
    --n=*) CONNS="${arg#*=}" ;;
    --links=*) LINKS="${arg#*=}" ;;
    --monitor=*) MONITOR_MIN="${arg#*=}" ;;
    --download-url=*) DOWNLOAD_URL="${arg#*=}" ;;
    --vk-token=*) USE_VK_TOKEN="${arg#*=}" ;;
    --vk-token) USE_VK_TOKEN="env" ;;
    --verbose) VERBOSE="--verbose" ;;
    --striping) STRIPING=1 ;;
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

# Build link args based on --links=N
LINK_ARGS=""
if [[ "$LINKS" -gt 1 ]]; then
  for i in $(seq 1 "$LINKS"); do
    varname="VK_CALL_LINK_$i"
    if [[ -z "${!varname:-}" ]]; then
      echo "FATAL: $varname not set in .env (needed for --links=$LINKS)"
      exit 1
    fi
    LINK_ARGS="$LINK_ARGS --link=${!varname}"
  done
else
  if [[ -z "${VK_CALL_LINK:-}" ]]; then
    echo "FATAL: VK_CALL_LINK not set in .env"
    exit 1
  fi
  LINK_ARGS="--link=$VK_CALL_LINK"
fi

# Determine auth mode.
if [[ -z "${VPN_TOKEN:-}" ]]; then
  echo "FATAL: VPN_TOKEN not set in .env"
  exit 1
fi

AUTH_MODE="anonymous"
VK_TOKEN_ARGS_SERVER=""
VK_TOKEN_ARGS_CLIENT=""

if [[ -n "$USE_VK_TOKEN" ]]; then
  if [[ "$USE_VK_TOKEN" == "env" ]]; then
    # --vk-token (no value) → load from .env
    if [[ -z "${VK_TOKEN_1:-}" || -z "${VK_TOKEN_2:-}" ]]; then
      echo "FATAL: --vk-token requires VK_TOKEN_1 and VK_TOKEN_2 in .env"
      exit 1
    fi
    AUTH_MODE="token (env)"
    VK_TOKEN_ARGS_SERVER="--vk-token=$VK_TOKEN_2"
    VK_TOKEN_ARGS_CLIENT="--vk-token=$VK_TOKEN_1"
  else
    # --vk-token=VALUE → use explicit token for both
    AUTH_MODE="token (explicit)"
    VK_TOKEN_ARGS_SERVER="--vk-token=$USE_VK_TOKEN"
    VK_TOKEN_ARGS_CLIENT="--vk-token=$USE_VK_TOKEN"
  fi
fi

echo "=== E2E Test ==="
echo "  Mode:        $AUTH_MODE"
echo "  Connections:  $CONNS"
echo "  Links:        $LINKS"
echo "  Monitor:      ${MONITOR_MIN}m"
echo "  Verbose:      $([ -n "$VERBOSE" ] && echo yes || echo no)"
echo "  Striping:     $([ -n "$STRIPING" ] && echo forced || echo adaptive)"
echo ""

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

run_max_throughput_test() {
  local total=$TOTAL_CONNS
  local max_time=60
  local tmpdir
  tmpdir=$(mktemp -d)

  echo ""
  echo "====== MAX THROUGHPUT TEST ======"
  echo "[$(ts)] $total parallel downloads via SOCKS5, url=$DOWNLOAD_URL"

  # Launch N parallel curl processes
  local pids=()
  for i in $(seq 1 "$total"); do
    curl -x socks5://127.0.0.1:$SOCKS_PORT -s -o /dev/null \
      -w "%{size_download} %{speed_download} %{time_total}" \
      --connect-timeout 15 --max-time "$max_time" \
      "$DOWNLOAD_URL" > "$tmpdir/result_$i" 2>/dev/null &
    pids+=($!)
  done

  # Wait for all to finish
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done

  # Aggregate results
  local total_bytes=0
  local total_speed=0
  local max_time_taken=0
  local ok=0
  local failed=0

  for i in $(seq 1 "$total"); do
    local res
    res=$(cat "$tmpdir/result_$i" 2>/dev/null) || true
    local sz=$(echo "$res" | awk '{print $1}')
    local sp=$(echo "$res" | awk '{print $2}')
    local tt=$(echo "$res" | awk '{print $3}')

    if [[ -n "$sp" && "$sp" != "0" && "$sp" != "0.000" ]]; then
      total_bytes=$(awk "BEGIN{printf \"%.0f\", $total_bytes + ${sz:-0}}")
      total_speed=$(awk "BEGIN{printf \"%.3f\", $total_speed + $sp}")
      if awk "BEGIN{exit !(${tt:-0} > $max_time_taken)}" 2>/dev/null; then
        max_time_taken=$tt
      fi
      ok=$((ok + 1))
      local mbps_i=$(awk "BEGIN{printf \"%.2f\", $sp * 8 / 1000000}")
      echo "  [stream $i] ${mbps_i} Mbps (${tt}s)"
    else
      failed=$((failed + 1))
      echo "  [stream $i] FAILED"
    fi
  done

  echo ""
  if [[ $ok -gt 0 ]]; then
    local human_total
    if awk "BEGIN{exit !($total_bytes > 1048576)}" 2>/dev/null; then
      human_total="$(awk "BEGIN{printf \"%.1f\", $total_bytes / 1048576}") MB"
    elif awk "BEGIN{exit !($total_bytes > 1024)}" 2>/dev/null; then
      human_total="$(awk "BEGIN{printf \"%.0f\", $total_bytes / 1024}") KB"
    else
      human_total="${total_bytes}b"
    fi
    local mbps_total=$(awk "BEGIN{printf \"%.2f\", $total_speed * 8 / 1000000}")
    echo "  AGGREGATE: ${human_total} in ${max_time_taken}s — ${mbps_total} Mbps"
    echo "  Streams: $ok ok, $failed failed"
  else
    echo "  ALL STREAMS FAILED"
  fi

  rm -rf "$tmpdir"
  echo "================================="
}

run_builtin_speedtest() {
  echo ""
  echo "====== BUILT-IN SPEED TEST ======"
  echo "[$(ts)] Running speed test through tunnel..."

  local tmpfile
  tmpfile=$(mktemp)

  # Stream JSON lines from the running client's HTTP proxy
  curl -s -N http://127.0.0.1:$HTTP_PORT/__speedtest__ > "$tmpfile" 2>/dev/null || true

  # Display progress lines (all but the last line)
  local line_count
  line_count=$(wc -l < "$tmpfile")
  if [[ "$line_count" -gt 1 ]]; then
    head -n $((line_count - 1)) "$tmpfile" | while IFS= read -r line; do
      local phase=$(echo "$line" | jq -r '.phase // empty' 2>/dev/null)
      local current_mbps=$(echo "$line" | jq -r '.current_mbps // empty' 2>/dev/null)
      local elapsed=$(echo "$line" | jq -r '.elapsed_s // empty' 2>/dev/null)
      local error=$(echo "$line" | jq -r '.error // empty' 2>/dev/null)

      if [[ -n "$error" ]]; then
        echo "[$(ts)] ERROR: $error"
      elif [[ -n "$current_mbps" && -n "$elapsed" ]]; then
        echo "[$(ts)] $phase: ${current_mbps} Mbps (${elapsed}s)"
      elif [[ -n "$phase" ]]; then
        echo "[$(ts)] Phase: $phase"
      fi
    done
  fi

  # Parse final result (last line)
  local result
  result=$(tail -n 1 "$tmpfile")
  rm -f "$tmpfile"

  if echo "$result" | jq -e '.ping' >/dev/null 2>&1; then
    echo ""
    local ping_avg=$(echo "$result" | jq -r '.ping.avg_ms')
    local ping_jitter=$(echo "$result" | jq -r '.ping.jitter_ms')
    local down_mbps=$(echo "$result" | jq -r '.download.mbps')
    local up_mbps=$(echo "$result" | jq -r '.upload.mbps')
    local num_conns=$(echo "$result" | jq -r '.connections | length')

    echo "  Ping:     ${ping_avg}ms (jitter ${ping_jitter}ms)"
    echo "  Download: ${down_mbps} Mbps"
    echo "  Upload:   ${up_mbps} Mbps"
    echo "  Connections: ${num_conns}"

    echo "$result" | jq -r '.connections[] | "    #\(.index): \(.down_mbps)/\(.up_mbps) Mbps, \(.latency_ms)ms"' 2>/dev/null
  else
    echo "[$(ts)] Speed test failed or returned no result"
  fi
  echo "================================="
}

cleanup() {
  echo ""
  echo "[$(ts)] Shutting down gracefully..."
  if $IS_WINDOWS; then
    # Kill the actual binaries (pipe makes $PID point to `sed`, not .exe).
    taskkill //F //IM "callvpn-client${EXE}" >/dev/null 2>&1 || true
    taskkill //F //IM "callvpn-server${EXE}" >/dev/null 2>&1 || true
    taskkill //F //IM "callvpn-captcha${EXE}" >/dev/null 2>&1 || true
    # Kill the sed pipe processes so `wait` doesn't hang.
    kill -9 $CLIENT_PID 2>/dev/null || true
    kill -9 $SERVER_PID 2>/dev/null || true
    kill -9 $CAPTCHA_PID 2>/dev/null || true
  else
    kill -INT $CLIENT_PID 2>/dev/null || true
    kill -INT $SERVER_PID 2>/dev/null || true
    kill -INT $CAPTCHA_PID 2>/dev/null || true
    sleep 3
    kill -9 $CLIENT_PID 2>/dev/null || true
    kill -9 $SERVER_PID 2>/dev/null || true
    kill -9 $CAPTCHA_PID 2>/dev/null || true
  fi
  wait $CLIENT_PID 2>/dev/null || true
  wait $SERVER_PID 2>/dev/null || true
  wait $CAPTCHA_PID 2>/dev/null || true
  echo "[$(ts)] Done."
}
# --- Platform detection ---
if [[ "$(uname -s)" == MINGW* || "$(uname -s)" == MSYS* || "$(uname -s)" == CYGWIN* ]]; then
  IS_WINDOWS=true
  EXE=".exe"
else
  IS_WINDOWS=false
  EXE=""
fi

trap cleanup EXIT

# --- Build ---
echo "[$(ts)] Building binaries..."
go build -o "callvpn-server${EXE}" ./cmd/server
go build -o "callvpn-client${EXE}" ./cmd/client
go build -o "callvpn-captcha${EXE}" ./cmd/captcha-service
echo "[$(ts)] Build OK"

# --- Kill stale processes ---
if $IS_WINDOWS; then
  taskkill //F //IM "callvpn-server${EXE}" >/dev/null 2>&1 || true
  taskkill //F //IM "callvpn-client${EXE}" >/dev/null 2>&1 || true
  taskkill //F //IM "callvpn-captcha${EXE}" >/dev/null 2>&1 || true
else
  pkill -f "callvpn-server" 2>/dev/null || true
  pkill -f "callvpn-client" 2>/dev/null || true
  pkill -f "callvpn-captcha" 2>/dev/null || true
fi
sleep 1

# --- Start captcha-service ---
echo "[$(ts)] Starting captcha-service on port $CAPTCHA_PORT..."
./callvpn-captcha${EXE} \
  --port="$CAPTCHA_PORT" \
  --max-concurrent=1 \
  --solve-timeout=90s \
  --request-timeout=2m \
  $VERBOSE \
  2>&1 | sed "s/^/  [captcha] /" &
CAPTCHA_PID=$!
sleep 2

if ! kill -0 $CAPTCHA_PID 2>/dev/null; then
  echo "FATAL: captcha-service died"
  exit 1
fi
echo "[$(ts)] Captcha-service running (PID $CAPTCHA_PID)"

CAPTCHA_ENDPOINT="http://127.0.0.1:$CAPTCHA_PORT"

# --- Start server ---
echo ""
TOTAL_CONNS=$((CONNS * LINKS))
echo "[$(ts)] Starting server (links=$LINKS, n=$CONNS, total=$TOTAL_CONNS)..."
ENABLE_STRIPING=${STRIPING:-} ./callvpn-server${EXE} \
  $LINK_ARGS \
  --tcp=true \
  --n="$CONNS" \
  --token="$VPN_TOKEN" \
  --captcha-endpoint="$CAPTCHA_ENDPOINT" \
  $VK_TOKEN_ARGS_SERVER \
  $VERBOSE \
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
echo "[$(ts)] Starting client (links=$LINKS, n=$CONNS, total=$TOTAL_CONNS)..."
ENABLE_STRIPING=${STRIPING:-} ./callvpn-client${EXE} \
  $LINK_ARGS \
  --n="$CONNS" \
  --tcp=true \
  --token="$VPN_TOKEN" \
  --socks5-port=$SOCKS_PORT \
  --http-port=$HTTP_PORT \
  --captcha-endpoint="$CAPTCHA_ENDPOINT" \
  $VK_TOKEN_ARGS_CLIENT \
  $VERBOSE \
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

# --- Max throughput test ---
run_max_throughput_test

# --- Built-in speed test ---
run_builtin_speedtest

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
