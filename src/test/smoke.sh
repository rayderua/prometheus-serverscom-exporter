#!/usr/bin/env bash
# Smoke and regression test of the exporter against the mock API (test/mockapi):
#   0. optional, with PREVIOUS: the previous release fills the cache file, and
#      the new version must expose the same inventory after the upgrade;
#   1. the exporter fetches everything and exposes the expected metrics; the
#      set of series must match test/golden/metrics-shape.txt;
#   2. after a restart with the API down, it serves the same data from the
#      cache file;
#   3. with collectors disabled and a fixed interval, their metrics are gone,
#      the rest is still served.
#
# Usage (from src/):
#   test/smoke.sh <image>                          # test a Docker image
#   EXPORTER_BIN=./serverscom-exporter test/smoke.sh   # test a local binary
#   PREVIOUS=<image or binary> ...                  # also test the upgrade
#   UPDATE_GOLDEN=1 ...                             # rewrite the golden file
# Needs go, and docker for images; uses the host network and ports 18080 and
# 19920.
set -euo pipefail

IMAGE=${1:-}
EXPORTER_BIN=${EXPORTER_BIN:-}
PREVIOUS=${PREVIOUS:-}
[[ -n "$IMAGE" || -n "$EXPORTER_BIN" ]] || { echo "usage: test/smoke.sh <image>, or set EXPORTER_BIN" >&2; exit 2; }
API_ADDR=127.0.0.1:18080
EXPORTER_ADDR=127.0.0.1:19920
NAME=serverscom-exporter-smoke
VOLUME=serverscom-exporter-smoke
TIMEOUT=${SMOKE_TIMEOUT:-90}
GOLDEN=test/golden/metrics-shape.txt

workdir=$(mktemp -d)
mock_pid=""
exporter_pid=""
# The version under test; switched to PREVIOUS for the upgrade step.
target_image=$IMAGE
target_bin=$EXPORTER_BIN

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
  stop_mock || true
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

# start_exporter [extra flags...]
start_exporter() {
  : > "$workdir/exporter.log"
  if [[ -n "$EXPORTER_BIN" ]]; then
    mkdir -p "$workdir/data"
    # shellcheck disable=SC2046
    env SERVERSCOM_API_TOKEN=test "$target_bin" $(exporter_args "$workdir/data") "$@" >> "$workdir/exporter.log" 2>&1 &
    exporter_pid=$!
  else
    # shellcheck disable=SC2046
    docker run -d --name "$NAME" --network host \
      -e SERVERSCOM_API_TOKEN=test \
      -v "$VOLUME:/data" \
      "$target_image" $(exporter_args /data) "$@" > /dev/null
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

start_mock() {
  "$workdir/mockapi" --listen="$API_ADDR" --token=test >> "$workdir/mockapi.log" 2>&1 &
  mock_pid=$!
  wait_for "mock API" curl -sf "http://$API_ADDR/-/healthy"
}

stop_mock() {
  [[ -n "$mock_pid" ]] || return 0
  kill "$mock_pid" 2> /dev/null || true
  wait "$mock_pid" 2> /dev/null || true
  mock_pid=""
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

# count <file> <metric>: number of series of the metric.
count() { grep -c "^$2{" "$1" || true; }

# all_data_loaded: details of both mock servers (2 drive slots, 2 features,
# 2 power feeds and 2 uplinks each) and both L2 members are exposed.
all_data_loaded() {
  local m
  m=$(metrics) || return 1
  [[ $(count "$m" serverscom_host_info) -eq 2 \
    && $(count "$m" serverscom_host_drive_slot_info) -eq 4 \
    && $(count "$m" serverscom_host_feature_status) -eq 4 \
    && $(count "$m" serverscom_host_power_feed_up) -eq 4 \
    && $(count "$m" serverscom_host_uplink_redundant) -eq 4 \
    && $(count "$m" serverscom_l2_segment_member_info) -eq 2 ]]
}

# all_loops_done: all data loaded and every account-wide loop ran once, so
# the set of series no longer depends on timing.
all_loops_done() {
  all_data_loaded || return 1
  local m=$workdir/metrics.txt
  grep -q '^serverscom_exporter_last_refresh_success_timestamp_seconds{resource="l2-segments"}' "$m" \
    && grep -q '^serverscom_exporter_last_refresh_success_timestamp_seconds{resource="uplinks"}' "$m" \
    && grep -q '^serverscom_exporter_requests_per_hour_estimated{loop="l2-segments"} [1-9]' "$m"
}

# servers_loaded: both servers with ports and power feeds (step 3).
servers_loaded() {
  local m
  m=$(metrics) || return 1
  [[ $(count "$m" serverscom_host_info) -eq 2 \
    && $(count "$m" serverscom_host_connection_info) -eq 4 \
    && $(count "$m" serverscom_host_power_feed_up) -eq 4 ]]
}

# inventory prints the server, L2 and reference series with their values.
inventory() {
  grep -E '^serverscom_(host|l2|location|rack)_' "$1" | grep -v '^serverscom_host_details_last_refresh' | LC_ALL=C sort
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
start_mock
[[ -z "$EXPORTER_BIN" ]] && docker volume create "$VOLUME" > /dev/null

if [[ -n "$PREVIOUS" ]]; then
  echo "== previous release ($PREVIOUS) fills the cache file"
  if [[ -n "$EXPORTER_BIN" ]]; then target_bin=$PREVIOUS; else target_image=$PREVIOUS; fi
  start_exporter
  wait_for "all data from the previous release" all_data_loaded
  inventory "$workdir/metrics.txt" > "$workdir/inventory.previous"
  pass "previous release serves $(wc -l < "$workdir/inventory.previous") inventory series"
  sleep 3 # let the persister write at least once
  stop_exporter
  target_image=$IMAGE
  target_bin=$EXPORTER_BIN
fi

echo "== first start: fetch from the API"
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
expect "auto intervals" '^serverscom_exporter_refresh_interval_auto\{loop="drives"\} 1$'
expect_none "no refresh errors" '^serverscom_exporter_refresh_errors_total\{.*\} [1-9]'

if [[ -n "$PREVIOUS" ]]; then
  diff "$workdir/inventory.previous" <(inventory "$workdir/metrics.txt") \
    || fail "inventory differs from the previous release (lines above: < previous, > new)"
  pass "same inventory as the previous release after the upgrade"
fi

wait_for "every refresh loop to run once" all_loops_done
shape=$workdir/shape.txt
test/metrics-shape.sh "$workdir/metrics.txt" > "$shape"
if [[ -n "${UPDATE_GOLDEN:-}" ]]; then
  mkdir -p "$(dirname "$GOLDEN")"
  cp "$shape" "$GOLDEN"
  pass "golden file $GOLDEN updated"
else
  diff "$GOLDEN" "$shape" \
    || fail "series differ from $GOLDEN (< expected, > actual); if intended, rerun with UPDATE_GOLDEN=1"
  pass "series match $GOLDEN"
fi

echo "== restart with the API down: serve from the cache file"
sleep 3 # let the persister write at least once
stop_exporter
stop_mock
start_exporter
wait_for "/-/healthy after restart" curl -sf "http://$EXPORTER_ADDR/-/healthy"
wait_for "cached data in /metrics" all_data_loaded
check_inventory
exporter_logs | grep -q 'Loaded cache file' || fail "cache file was not loaded"
pass "cache file loaded"

echo "== restart with collectors disabled and a fixed interval"
stop_exporter
start_mock
start_exporter --no-collector.drives --no-collector.features --no-collector.l2-segments --refresh.ports-interval=15m \
  --label=account=smoke
wait_for "/-/healthy with collectors disabled" curl -sf "http://$EXPORTER_ADDR/-/healthy"
wait_for "servers in /metrics" servers_loaded
expect "ports" '^serverscom_host_connection_info\{'
expect "extra label on host metrics" '^serverscom_host_info\{account="smoke",'
expect "extra label on API metrics" '^serverscom_api_requests_total\{account="smoke",'
expect "extra label on build info" '^serverscom_exporter_build_info\{account="smoke",'
file=$(metrics) || fail "cannot fetch /metrics"
if grep '^serverscom_' "$file" | grep -v 'account="smoke"'; then
  fail "series above lack the extra label"
fi
pass "every serverscom_ series has the extra label"
expect "power feeds" '^serverscom_host_power_feed_up\{.*\} 1$'
expect "redundant uplink" '^serverscom_host_uplink_redundant\{.*\} 1$'
expect_none "no drives" '^serverscom_host_drive_'
expect_none "no features" '^serverscom_host_feature_'
expect_none "no aggregation state without features" '^serverscom_host_uplink_aggregated\{'
expect_none "no L2 segments" '^serverscom_l2_'
expect_none "no refresh timestamps of disabled collectors" 'resource="(drives|features|l2-segments)"'
expect "fixed ports interval" '^serverscom_exporter_refresh_interval_seconds\{.*loop="ports".*\} 900$'
expect "ports interval is fixed" '^serverscom_exporter_refresh_interval_auto\{.*loop="ports".*\} 0$'
expect_none "disabled collectors are not estimated" 'loop="(drives|features|l2-segments)"'
expect_none "no refresh errors" '^serverscom_exporter_refresh_errors_total\{.*\} [1-9]'

echo "== smoke test passed"
