# BoxDwel architecture (public development checkpoint)

Status: development source checkpoint, not a finished product. The CLI name is
`boxdwel`; runtime VM identifiers and local paths intentionally keep their
historical `vboxdistro-*` values.

## Scope

BoxDwel is an independent, unofficial tool for a Linux guest in Oracle
VirtualBox. It is not affiliated with or endorsed by Oracle. The current
checkpoint covers a fixed test VM and a BusyBox-only guest image; it is not a
general-purpose VM manager.

## Components

- `cmd/boxdwel` — Windows/amd64 CLI with `version`, `doctor`, and
  `enter test`. `doctor` checks the host and can locate `VBoxManage`; it does
  not create or modify VMs.
- `cmd/boxdwel-agent` — guest Agent stub. It prints an unimplemented message
  and exits 1. READY, PTY, sessions, resize, and the product protocol are not
  implemented.
- `internal/host/vbox` — narrow `VBoxManage --version` detection with a
  timeout and explicit failure reporting.
- `internal/enter` — Windows COM1 single-session path: owner/M2 metadata
  checks, UUID/name/config/SATA validation, reparse rejection, and a
  first-instance named pipe with a user+SYSTEM DACL. The identity constants
  and `%LOCALAPPDATA%\vboxdistro\m1-test` paths are fixed test-environment
  values, not secrets.
- `cmd/m3-transport-probe` + `internal/m3probe` — a separate experimental
  COM2 transport probe. See [the M3 experiment](m3-transport-spike.md). It is
  not the product Agent protocol.
- `boot/` — Linux 6.18.58 and BusyBox 1.38.0 configuration fragments, source
  URLs/SHA-256 lock, and two init scripts: the M2 single-shell init and the
  M3 measurement init. No kernel or BusyBox source is vendored.
- `scripts/` — Go build/test scripts, the official-archive boot-image build,
  and Windows user-run validation scripts.

## M1/M2 state

The boot build produces an EFI-stub kernel, embedded initramfs, and a GPT/FAT
raw image from official source archives. The M2 guest runs a single COM1
BusyBox shell; `enter test` keeps the VM running across shell exit. The M2
Windows path was verified historically before the source rename. This
checkpoint does not re-run Windows or rebuild the image.

## M3 experiment state

M3 measures a framed COM2 transport between a guest probe and a Windows host
probe, including baseline echo RTT, two interleaved 256 KiB streams, pings
during output, and a clean-boundary reconnect in one boot. The fixed
measurement completed on the original pre-rename binaries. Aggregate values
are in [the experiment document](m3-transport-spike.md). `m3_accepted` remains
false; forced disconnect recovery, real PTY/UI latency, long-running load, and
the full product session implementation are not verified. Product M3
implementation is user-approved but paused for this public-source preparation.

## Build and checks

```text
go test ./...
go vet ./...
go run ./scripts        # Windows CLI + Linux Agent + Windows enter-test compile
bash scripts/build-boot.sh
```

The boot build downloads the official Linux/BusyBox archives listed in
`boot/sources.lock`, verifies SHA-256, and does not commit source archives or
binary images. GPL components keep their licenses; see `THIRD_PARTY.md`.

## Fixed test-environment boundary

The repository retains historical VM UUID/name/run ID, `m1-test`/`m2-unicode`
paths, and named-pipe names. This preserves the verified ownership and
protocol behavior; it is not a claim that the tool is portable. Changing these
values is a separate migration.

The Windows M3 runner pins the SHA-256 of the original pre-rename image. A
rebuilt image after the source/module rename will not be accepted until the
runner's pinned hash is deliberately updated with review; do not weaken the
check or reuse the old binary package as if it were rebuilt.
