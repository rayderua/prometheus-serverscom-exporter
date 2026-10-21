#!/usr/bin/env bash
# Prints the shape of a Prometheus text exposition: one line per series with
# the metric name and its labels, sorted, without values. Go runtime, process
# and promhttp series are dropped, and so are label values that change between
# builds (build info) or with timing (API request status codes and endpoints
# are kept, as they are part of the contract).
#
# Usage: test/metrics-shape.sh metrics.txt > shape.txt
set -euo pipefail

grep -Ev '^(#|go_|process_|promhttp_|$)' "$1" |
  sed -E 's/ [^ ]+( [0-9]+)?$//' |
  sed -E 's/^(serverscom_exporter_build_info)\{.*\}$/\1{...}/' |
  sed -E '/^serverscom_api_request_duration_seconds_bucket/d' |
  LC_ALL=C sort -u
