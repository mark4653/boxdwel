#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist/logs
log="dist/logs/${1}-$(date -u +%Y%m%dT%H%M%S)-$$.log"
echo "Log: $log"
run() {
  echo "+ $*"
  local rc=0
  "$@" || rc=$?
  echo "exit_code=$rc"
  return "$rc"
}
{
  run date -u +%FT%TZ
  run go version
  run go env GOOS GOARCH CGO_ENABLED
  case "${1:-}" in
    test)
      run go fmt ./...
      run go vet ./...
      run go test -count=1 -v ./...
      ;;
    build) run go run ./scripts ;;
    *) echo 'usage: scripts/check.sh test|build'; exit 2 ;;
  esac
} 2>&1 | tee "$log"
