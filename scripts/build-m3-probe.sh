#!/usr/bin/env bash
# M3 transport spike: guest probe(linux), Windows host probe, M2 native pipe test exe를
# 새 exclusive 출력에 빌드한다. 기존 dist/m1, dist/m2는 건드리지 않는다.
set -euo pipefail
cd "$(dirname "$0")/.."

stamp=$(date -u +%Y%m%dT%H%M%SZ)
out=dist/m3/$stamp
mkdir -p dist/m3
mkdir "$out"
commit=$(git rev-parse --verify HEAD 2>/dev/null || echo unavailable)

echo "+ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build ./cmd/m3-transport-probe"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -trimpath -buildvcs=false \
	-ldflags="-X github.com/mark4653/boxdwel/internal/buildinfo.Commit=$commit" \
	-o "$out/m3-transport-probe" ./cmd/m3-transport-probe

echo "+ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go build ./cmd/m3-transport-probe"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go build -trimpath -buildvcs=false \
	-ldflags="-X github.com/mark4653/boxdwel/internal/buildinfo.Commit=$commit" \
	-o "$out/m3-transport-probe.exe" ./cmd/m3-transport-probe

echo "+ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go test -c ./internal/enter"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go test -c \
	-o "$out/boxdwel-enter.test.exe" ./internal/enter

python3 - "$out/m3-transport-probe.exe" "$out/boxdwel-enter.test.exe" <<'PY'
import struct
import sys

for path in sys.argv[1:]:
    data = open(path, 'rb').read()
    if data[:2] != b'MZ':
        sys.exit('%s: not an MZ image' % path)
    off = struct.unpack_from('<I', data, 0x3c)[0]
    if data[off:off + 4] != b'PE\0\0':
        sys.exit('%s: missing PE signature' % path)
    machine = struct.unpack_from('<H', data, off + 4)[0]
    if machine != 0x8664:
        sys.exit('%s: not PE/amd64: %#x' % (path, machine))
    print('pe_amd64=OK %s' % path)
PY

file "$out/m3-transport-probe" >"$out/file.txt"
( cd "$out" && sha256sum m3-transport-probe m3-transport-probe.exe boxdwel-enter.test.exe >SHA256SUMS )
{
	echo "built_at=$(date -u +%FT%TZ)"
	echo "commit=$commit"
	echo "go=$(go version)"
	echo "guest_size=$(stat -c '%s' "$out/m3-transport-probe")"
	echo "host_size=$(stat -c '%s' "$out/m3-transport-probe.exe")"
	echo "test_size=$(stat -c '%s' "$out/boxdwel-enter.test.exe")"
} >"$out/identity.txt"
echo "out=$out"
cat "$out/SHA256SUMS" "$out/identity.txt" "$out/file.txt"
