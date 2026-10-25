# M3 COM2 transport experiment

Status: experimental measurement from the original pre-rename binaries.
`m3_accepted=false`.

## Frame

```
6-byte header: version(1) type(1) id(2, big-endian) length(2, big-endian)
payload: 0..4096 bytes
```

| type | purpose |
|---|---|
| Hello / Ready | host nonce; guest reflects the same nonce |
| Echo / EchoReply | baseline RTT and pings during output |
| Start / Stream / StreamEnd | two logical streams; 256 KiB each in the fixed run |
| Finish / Ack | clean session end |

Unknown version/type, oversized, truncated, bad-id, duplicate-start,
short-write, and Finish-while-active frames close the session. There is no
resync or ANSI-skip path. Stream bytes use a per-id seed, so a frame moved to
the wrong id fails the receiver's byte/hash check.

## Startup order

The first attempt failed about 421 ms into the phase-1 handshake with
`unsupported version: 0x1b`, before the guest was ready. COM1 showed 58 bytes
of EFI-style output, but no COM2 capture exists, so the origin of `0x1b` is
unconfirmed.

The working sequence:

1. Leave COM2 `disconnected` and boot the VM.
2. Wait for the exact COM1 line `M3_PHASE1_START`.
3. Start the host pipe, wait for `M3_PIPE_READY`, re-check the VM/VDI, then
   `changeuartmode2 client`.
4. After phase 1, disconnect COM2, wait for `M3_PHASE2_START`, start a fresh
   host probe, and connect again.

The host starts only after the guest marker; phase 2 follows a successful
phase 1. There is no second boot or automatic retry.

## Historical measurement

Aggregates from the successful fixed run on the original image and binaries.
They were not re-run for this checkpoint.

| metric | phase 1 | phase 2 |
|---|---|---|
| baseline 32-echo RTT p95 | 0.502 ms | 0.501 ms |
| RTT during output p95 | 50.510 ms | 59.509 ms |
| RTT during output max | 59.009 ms | 86.515 ms |
| two-stream aggregate | 166042 B/s | 166305 B/s |

Each phase carried two ids × 262144 bytes / 256 chunks with matching
sender/receiver SHA-256, 512 interleaved switches, and zero missed bulk pings.
The two measurements in one boot used about 1 MiB of fixed payload.

## Reproduce locally

```text
go test ./internal/m3probe
go build -trimpath -o /tmp/m3-probe ./cmd/m3-transport-probe
python3 scripts/check-m3-probe-pty.py /tmp/m3-probe
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go build -o /tmp/m3-host.exe ./cmd/m3-transport-probe
python3 scripts/check-m3-runner.py scripts/test-m3-transport-windows.ps1

# optional guest image build (slow; separate from the local checks above)
M3_PROBE_BIN=/tmp/m3-probe bash scripts/build-boot.sh
```

The PTY check is Linux-host-only: it exercises termios, a 256-byte round trip,
and the guest deadline. The runner check is static. Windows/VirtualBox
execution is a separate user-run step.

`scripts/test-m3-transport-windows.ps1` pins the original pre-rename image
SHA-256. A rebuilt image is rejected until that value is deliberately updated
and reviewed; the ownership and checksum gates stay in place.

## Measured and pending

| measured | pending |
|---|---|
| byte/hash preservation, two interleaved streams, pings during output, baseline RTT, clean-boundary reconnect in one boot | forced mid-frame disconnect, automatic recovery, real PTY/UI latency, resize, session survival, long-running load, product Agent implementation |
