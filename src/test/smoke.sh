#!/usr/bin/env bash
# Smoke test of the exporter image against the mock API (test/mockapi):
#   1. the exporter fetches everything and exposes the expected metrics;
#   2. after a restart with the API down, it serves the same data from the
#      cache file.
#
# Usage (from src/):
#   test/smoke.sh <image>                          # test a Docker image
#   EXPORTER_BIN=./serverscom-exporter test/smoke.sh   # test a local binary
# Needs go, and docker for images; uses the host network and ports 18080 and
# 19920.
set -euo pipefail

IMAGE=${1:-}
EXPORTER_BIN=${EXPORTER_BIN:-}
[[ -n "$IMAGE" || -n "$EXPORTER_BIN" ]] || { echo "usage: test/smoke.sh <image>, or set EXPORTER_BIN" >&2; exit 2; }
API_ADDR=127.0.0.1:18080
EXPORTER_ADDR=127.0.0.1:19920
NAME=serverscom-exporter-smoke
VOLUME=serverscom-exporter-smoke
TIMEOUT=${SMOKE_TIMEOUT:-90}

workdir=$(mktemp -d)
mock_pid=""
exporter_pid=""

cleanup() {
  status=$?
  if [[ $status -ne 0 ]]; then
    echo "--- exporter logs"
    exporter_logs | tail -50 || true
    echo "--- mock API log"
    tail -30 "$workdir/mockapi.log" 2>/dev/null || true
  fi
  stop_exporter || true
  [[ -z "$EXPORTER_BIN" ]] && docker volume rm -f "$VOLUME" > /dev/null 2>&1 || true
  [[ -n "$mock_pid" ]] && kill "$mock_pid" 2> /dev/null || true
  rm -rf "$workdir"
  exit $status
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

exporter_args() {
  local data_dir=$1
  echo --api.url="http://$API_ADDR/v1" \
    --api.page-size=1 \
    --cache.file="$data_dir/cache.json" \
    --cache.save-interval=2s \
    --web.listen-address="$EXPORTER_ADDR"
}

start_exporter() {
  if [[ -n "$EXPORTER_BIN" ]]; then
    mkdir -p "$workdir/data"
    # shellcheck disable=SC2046
    SERVERSCOM_API_TOKEN=test "$EXPORTER_BIN" $(exporter_args "$workdir/data") >> "$workdir/exporter.log" 2>&1 &
    exporter_pid=$!
  else
    # shellcheck disable=SC2046
    docker run -d --name "$NAME" --network host \
      -e SERVERSCOM_API_TOKEN=test \
      -v "$VOLUME:/data" \
      "$IMAGE" $(exporter_args /data) > /dev/null
  fi
}

# stop_exporter stops it with SIGTERM, so it saves the cache on exit.
stop_exporter() {
  if [[ -n "$EXPORTER_BIN" ]]; then
    [[ -n "$exporter_pid" ]] || return 0
    kill "$exporter_pid" 2> /dev/null || true
    wait "$exporter_pid" 2> /dev/null || true
    exporter_pid=""
  else
    docker stop -t 10 "$NAME" > /dev/null 2>&1 || true
    docker rm -f "$NAME" > /dev/null 2>&1 || true
  fi
}

exporter_logs() {
  if [[ -n "$EXPORTER_BIN" ]]; then
    cat "$workdir/exporter.log"
  else
    docker logs "$NAME" 2>&1
  fi
}

# wait_for <description> <command...>: retries the command until it succeeds.
wait_for() {
  local what=$1; shift
  local deadline=$((SECONDS + TIMEOUT))
  until "$@" > /dev/null 2>&1; do
    ((SECONDS < deadline)) || fail "timed out waiting for $what"
    sleep 1
  done
}

# metrics saves /metrics to a file and prints its path. Checks grep the file:
# piping curl into "grep -q" fails under pipefail when grep exits early and
# curl gets SIGPIPE.
metrics() {
  curl -sf -o "$workdir/metrics.txt" "http://$EXPORTER_ADDR/metrics" || return 1
  echo "$workdir/metrics.txt"
}

# expect <description> <grep -E pattern>: the pattern must match /metrics.
expect() {
  local file
  file=$(metrics) || fail "$1: cannot fetch /metrics"
  grep -Eq "$2" "$file" || fail "$1: no line matches /$2/"
  pass "$1"
}

# expect_none <description> <grep -E pattern>: no line of /metrics may match.
expect_none() {
  local file
  file=$(metrics) || fail "$1: cannot fetch /metrics"
  if grep -E "$2" "$file"; then
    fail "$1: lines above match /$2/"
  fi
  pass "$1"
}

all_data_loaded() {
  local m
  m=$(metrics) || return 1
  [[ $(grep -c '^serverscom_host_info{' "$m") -eq 2 ]] \
    && grep -q '^serverscom_host_drive_capacity{' "$m" \
    && grep -q '^serverscom_host_feature_status{' "$m" \
    && grep -q '^serverscom_l2_segment_member_info{' "$m" \
    && grep -q '^serverscom_host_uplink_redundant{' "$m"
}

check_inventory() {
  expect "two servers" '^serverscom_host_info\{.*host_id="srv-2"'
  expect "powered off server" '^serverscom_host_powered_on\{.*host_id="srv-2".*\} 0$'
  expect "powered on server" '^serverscom_host_powered_on\{.*host_id="srv-1".*\} 1$'
  expect "host labels" '^serverscom_host_labels\{.*label_env="test"'
  expect "drive capacity" '^serverscom_host_drive_capacity\{.*\} 960$'
  expect "power feeds" '^serverscom_host_power_feed_up\{.*\} 1$'
  expect "redundant uplink" '^serverscom_host_uplink_redundant\{.*\} 1$'
  expect "L2 member with VLAN" '^serverscom_l2_segment_member_info\{.*vlan="100"'
  expect "location" '^serverscom_location_info\{.*location="AMS1"'
  expect "rack" '^serverscom_rack_info\{.*rack="R1"'
}

echo "== start mock API"
go build -o "$workdir/mockapi" ./test/mockapi
"$workdir/mockapi" --listen="$API_ADDR" --token=test > "$workdir/mockapi.log" 2>&1 &
mock_pid=$!
wait_for "mock API" curl -sf "http://$API_ADDR/-/healthy"

echo "== first start: fetch from the API"
[[ -z "$EXPORTER_BIN" ]] && docker volume create "$VOLUME" > /dev/null
start_exporter
wait_for "/-/healthy" curl -sf "http://$EXPORTER_ADDR/-/healthy"
pass "/-/healthy"
wait_for "all data in /metrics" all_data_loaded
check_inventory
expect "build info" '^serverscom_exporter_build_info\{'
expect "successful API requests" '^serverscom_api_requests_total\{.*code="200".*\} [1-9]'
expect_none "no failed API requests" '^serverscom_api_requests_total\{.*code="[^2]'
expect "account rate limit" '^serverscom_api_ratelimit_remaining [0-9]'
expect "hosts refreshed" '^serverscom_exporter_last_refresh_success_timestamp_seconds\{resource="hosts"\}'
expect_none "no refresh errors" '^serverscom_exporter_refresh_errors_total\{.*\} [1-9]'

echo "== restart with the API down: serve from the cache file"
sleep 3 # let the persister write at least once
stop_exporter
kill "$mock_pid"
wait "$mock_pid" 2> /dev/null || true
mock_pid=""
start_exporter
wait_for "/-/healthy after restart" curl -sf "http://$EXPORTER_ADDR/-/healthy"
wait_for "cached data in /metrics" all_data_loaded
check_inventory
exporter_logs | grep -q 'Loaded cache file' || fail "cache file was not loaded"
pass "cache file loaded"

echo "== smoke test passed"
