# BoxDwel

BoxDwel is a development CLI for Linux guests in Oracle VirtualBox. It is
unofficial and not affiliated with or endorsed by Oracle.

## Current state

This is a source checkpoint for one prepared test VM.

- `cmd/boxdwel` provides `version`, `doctor`, and the Windows-only
  `enter test` path.
- The guest is a BusyBox-only Linux image with a single COM1 serial shell.
- The M3 COM2 transport probe is an experiment and not the product Agent
  protocol; see [the M3 document](docs/m3-transport-spike.md).
- `cmd/boxdwel-agent` serves up to two interactive PTY shells over the product
  protocol on Linux; see [the agent protocol](docs/agent-protocol.md).
- `internal/relay` is the platform-neutral host relay core: one guest wire,
  two client streams, local/guest session ids, bounded queues, and liveness.
  The Windows named-pipe/console integration is not implemented.
- Not implemented: the Windows relay that drives the agent over COM2, VM image
  integration, OCI/RootFS import, and session recovery.

See [architecture](docs/architecture.md) for components and the COM1 flow.

## Requirements

- Go 1.26 or newer. The module has no external Go dependencies.
- Windows/amd64 and VirtualBox 7.2.x for the Windows CLI path.
- The official Linux and BusyBox archives in `boot/sources.lock` for guest
  image work. Kernel and BusyBox source are not vendored.

## Build

```text
go test ./...
go vet ./...
go run ./scripts        # Windows CLI + Linux Agent + Windows enter-test compile
bash scripts/build-boot.sh
```

`build-boot.sh` downloads the official archives over HTTPS, verifies their
SHA-256 values, and builds the image locally.

## Development VM

The code uses a fixed VM identity (UUID/name/run ID and owner hash) and legacy
local paths under `%LOCALAPPDATA%\vboxdistro\m1-test`; see
`internal/enter/target.go`. The Windows validation scripts target that prepared
VM and should not be used with an unrelated VM. Adapting the identity and paths
is a separate migration.

## License

BoxDwel's own source is MIT-licensed; see `LICENSE`. Third-party components
keep their licenses; see `THIRD_PARTY.md`.
