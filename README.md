# BoxDwel

BoxDwel is an independent, **unofficial** development CLI for working with a
Linux guest in Oracle VirtualBox. It is not affiliated with, endorsed by, or
sponsored by Oracle. "Oracle VirtualBox" is used only to describe the
compatibility target.

## Status

This is a development source checkpoint, not a finished product.

- CLI (`cmd/boxdwel`): version/doctor checks and a Windows-only `enter test`
  path for one fixed test VM.
- Guest: a BusyBox-only Linux boot image with a single COM1 serial shell (M2).
- M3: an experimental COM2 transport probe and measurement harness. It is not
  the product agent protocol and does not provide production sessions,
  reconnect, or independent PTYs.
- `cmd/boxdwel-agent` is still a stub. Guest Agent, OCI/RootFS import,
  multi-terminal support, resize, and session recovery are **not implemented**.

The source is deliberately coupled to a fixed development VM identity and
paths (UUID/name/run ID, owner hash, and `%LOCALAPPDATA%\vboxdistro\m1-test`).
Those identifiers are not secrets, but they are test-environment values; see
`internal/enter/target.go`. This repository does not claim to be a universally
runnable VM manager.

Design notes: [architecture](docs/architecture.md) and the
[M3 COM2 experiment](docs/m3-transport-spike.md). The M3 experiment is not the
product Agent protocol; `m3_accepted` is false.

## Requirements

- Go 1.26 or newer (no external Go module dependencies).
- For the Windows path: Windows/amd64 and VirtualBox 7.2.x.
- For guest image work: the official Linux and BusyBox source archives listed
  in `boot/sources.lock`; no kernel/BusyBox source is vendored here.

## Build and test

```text
go test ./...
go vet ./...
go run ./scripts        # builds dist/windows/boxdwel.exe, dist/guest/boxdwel-agent
                        # and compiles the Windows enter test executable
```

Guest image build (downloads the official archives by HTTPS and verifies the
SHA-256 values in `boot/sources.lock`):

```text
bash scripts/build-boot.sh
```

M3 experiment image (optional; requires an already built probe binary):

```text
M3_PROBE_BIN=/absolute/path/to/m3-transport-probe bash scripts/build-boot.sh
```

Local M3 probe checks:

```text
go test ./internal/m3probe
go build -trimpath -o /tmp/m3-probe ./cmd/m3-transport-probe
python3 scripts/check-m3-probe-pty.py /tmp/m3-probe
python3 scripts/check-m3-runner.py scripts/test-m3-transport-windows.ps1
```

The PTY check is Linux-host-only and covers the local termios/all-256-byte path
and the guest deadline; the runner check is static. Neither is a substitute for the separate
Windows/VirtualBox execution, which is user-run and was not performed for this
checkpoint.

Windows validation scripts under `scripts/` are user-run; they are not part of
`go test`. They contain fixed test-VM identifiers and require a separately
prepared VM. Do not run them against an unrelated VM.

The M3 Windows runner is pinned to the SHA-256 of the original pre-rename
image. After rebuilding an image from this public source, the runner will
reject it until the pinned hash is deliberately updated with review. Do not
relax the ownership/checksum gates or treat the old package as a rebuilt one.

## Compatibility and names

The public CLI name is `boxdwel`, but legacy runtime identifiers and local paths
(`vboxdistro-m1-...`, `%LOCALAPPDATA%\vboxdistro`, named-pipe names) are kept
unchanged so the existing verified test environment and ownership checks keep
their meaning. Renaming those is a separate migration, not a source rename.

## License

The project's own source is MIT-licensed; see `LICENSE`. Third-party components
keep their own licenses; see `THIRD_PARTY.md`. In particular, the Linux kernel
and BusyBox are GPL-licensed and are **not** relicensed under MIT. No kernel or
BusyBox source is included in this repository.
