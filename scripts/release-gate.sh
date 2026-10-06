#!/usr/bin/env bash
# CI-system-agnostic DeployGuard release gate, demonstrated against the local
# fixture service. Any CI that can run bash can call it.
#
#   scripts/release-gate.sh            # clean candidate  -> expected exit 0
#   scripts/release-gate.sh regressed  # regressed candidate -> expected exit 1
#
# The script exits with DeployGuard's own exit code, so a FAIL (1) or
# INCONCLUSIVE (2) fails the calling CI job. The JSON report is always written
# to $DG_REPORT before the script exits.
#
# Environment overrides:
#   DG_CANDIDATE_VARIANT  clean|regressed (default clean; first argument wins)
#   DG_CONFIG             workload (default examples/deployguard.yaml)
#   DG_REPORT             report path (default reports/release-gate.json)
#   DG_BASE_PORT          baseline fixture port (default 18080)
#   DG_CAND_PORT          candidate fixture port (default 18081)
#   DG_BIN_DIR            where binaries are built (default bin)
set -euo pipefail

variant="${1:-${DG_CANDIDATE_VARIANT:-clean}}"
config="${DG_CONFIG:-examples/deployguard.yaml}"
report="${DG_REPORT:-reports/release-gate.json}"
base_port="${DG_BASE_PORT:-18080}"
cand_port="${DG_CAND_PORT:-18081}"
bin_dir="${DG_BIN_DIR:-bin}"

case "$variant" in
  clean|regressed) ;;
  *) echo "release-gate: candidate variant must be 'clean' or 'regressed' (got '$variant')" >&2; exit 3 ;;
esac

exe=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) exe=".exe" ;; esac

echo "==> Building deployguard and dg-fixture"
mkdir -p "$bin_dir"
go build -o "$bin_dir/deployguard$exe" ./cmd/deployguard
go build -o "$bin_dir/dg-fixture$exe" ./cmd/dg-fixture

pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Starting baseline fixture on :$base_port and '$variant' candidate on :$cand_port"
"$bin_dir/dg-fixture$exe" --variant baseline --addr "127.0.0.1:$base_port" &
pids+=("$!")
"$bin_dir/dg-fixture$exe" --variant "$variant" --addr "127.0.0.1:$cand_port" &
pids+=("$!")

# Wait until each fixture answers /health with the expected variant, so a
# stale process on the same port cannot impersonate it.
wait_ready() {
  local port="$1" want="$2" body
  for _ in $(seq 1 100); do
    body="$(curl -fsS "http://127.0.0.1:$port/health" 2>/dev/null || true)"
    if [[ "$body" == *"\"variant\":\"$want\""* ]]; then
      echo "    ready: :$port ($want)"
      return 0
    fi
    sleep 0.1
  done
  echo "release-gate: fixture on :$port did not become ready as '$want'" >&2
  return 1
}
wait_ready "$base_port" baseline || exit 3
wait_ready "$cand_port" "$variant" || exit 3

echo "==> Running DeployGuard"
set +e
"$bin_dir/deployguard$exe" compare \
  --config "$config" \
  --baseline "http://127.0.0.1:$base_port" \
  --candidate "http://127.0.0.1:$cand_port" \
  --report "$report"
status=$?
set -e

case "$status" in
  0) echo "==> Release gate PASSED (exit 0). Report: $report" ;;
  1) echo "==> Release gate BLOCKED: regression detected (exit 1). Report: $report" ;;
  2) echo "==> Release gate BLOCKED: inconclusive, cannot approve (exit 2). Report: $report" ;;
  *) echo "==> Release gate ERROR (exit $status)." ;;
esac
exit "$status"
