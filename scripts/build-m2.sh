#!/usr/bin/env bash
# M2 Windows CLI + enter test exe를 새 exclusive 출력에 빌드한다. 기존 dist/windows(M0)는 건드리지 않는다.
set -euo pipefail
cd "$(dirname "$0")/.."

stamp=$(date -u +%Y%m%dT%H%M%SZ)
out=dist/m2/$stamp
mkdir -p dist/m2
mkdir "$out"
commit=$(git rev-parse --verify HEAD 2>/dev/null || echo unavailable)

echo "+ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go build ./cmd/boxdwel"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go build -trimpath -buildvcs=false \
	-ldflags="-X github.com/mark4653/boxdwel/internal/buildinfo.Commit=$commit" \
	-o "$out/boxdwel.exe" ./cmd/boxdwel

echo "+ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go test -c ./internal/enter"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go test -c \
	-o "$out/boxdwel-enter.test.exe" ./internal/enter

python3 - "$out/boxdwel.exe" "$out/boxdwel-enter.test.exe" <<'PY'
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

( cd "$out" && sha256sum boxdwel.exe boxdwel-enter.test.exe >SHA256SUMS )
{
	echo "built_at=$(date -u +%FT%TZ)"
	echo "commit=$commit"
	echo "go=$(go version)"
	echo "size=$(stat -c '%s' "$out/boxdwel.exe")"
	echo "test_size=$(stat -c '%s' "$out/boxdwel-enter.test.exe")"
} >"$out/identity.txt"
echo "out=$out"
cat "$out/SHA256SUMS"
cat "$out/identity.txt"
