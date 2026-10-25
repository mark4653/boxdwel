# M3 COM2 transport experiment (public summary)

Status: **experimental transport measurement completed on the original
pre-rename binaries; product M3 is not accepted.** `m3_accepted=false`.
This document is a concise public description, not a product protocol
specification and not a raw run log.

## Framing and scope

The experiment asks whether one COM2 serial link can carry a small framed
protocol with bounded memory and time. The frame format is experimental:

```
6-byte header: version(1) type(1) id(2, big-endian) length(2, big-endian)
payload: 0..4096 bytes
```

| type | purpose |
|---|---|
| Hello / Ready | host nonce → guest reflects the same nonce |
| Echo / EchoReply | baseline RTT and pings during output |
| Start / Stream / StreamEnd | two logical streams; 256 KiB each in the fixed run |
| Finish / Ack | clean session end |

Unknown version/type, oversized, truncated, bad-id, duplicate-start,
short-write, and Finish-while-active frames fail closed. There is no
resync/ANSI-skip path. Stream bytes use a per-id fixed seed; a frame moved to
the wrong id fails the receiver's byte/hash check.

## Startup gate

The first measured attempt failed about 421 ms into the phase-1 handshake with
`unsupported version: 0x1b`, before the guest was ready. The host pipe was
connected before guest readiness. EFI/ANSI startup bytes are the suspicion, but
the full COM2 trace was not captured, so the byte origin is not confirmed; the
recovered COM1 evidence was only 58 bytes of EFI-style firmware output. After
the connection-time gate was fixed, the fixed run succeeded. The corrected
sequence is:

1. Keep COM2 `disconnected` and boot the VM.
2. Wait for the exact COM1 line `M3_PHASE1_START`.
3. Start the host pipe, wait for `M3_PIPE_READY`, re-check the VM/VDI, then
   connect COM2 (`changeuartmode2 client`).
4. After a clean phase 1, disconnect COM2, wait for `M3_PHASE2_START`, start a
   fresh host probe, and connect again.

The host never starts before the guest marker. Phase 2 runs only after the
whole phase 1 succeeds. No second boot or automatic retry is performed.

## Fixed measurement (historical, before source rename)

The numbers below are aggregates from the successful fixed run on the original
image and binaries. They are **historical measurements**, not a re-run of this
public checkpoint and not performance acceptance thresholds. Raw logs, result
identifiers, and personal paths are intentionally not included.

- Baseline 32-echo RTT p95: about 0.502 / 0.501 ms (two phases).
- RTT during output p95: about 50.510 / 59.509 ms; max about 59.009 / 86.515 ms.
- Two-stream aggregate payload: about 166042 / 166305 B/s.
- Each phase: two ids × 262144 bytes / 256 chunks, stream SHA-256 matched
  between sender and receiver, 512 interleaved switches, zero missed bulk
  pings.
- Total fixed payload about 1 MiB across two measurements in one boot.

The measurement used a clean boundary between two in-boot probes. It did not
inject a mid-frame disconnect, exercise a real PTY/UI, or run a long-duration
load. Those remain **NOT RUN**. A successful transport experiment does not mean
the M3 product session/Agent is complete.

## Reproduce locally

```text
go test ./internal/m3probe
go build -trimpath -o /tmp/m3-probe ./cmd/m3-transport-probe
python3 scripts/check-m3-probe-pty.py /tmp/m3-probe
```

The PTY check is **Linux-host-only**. It uses the real `-dev` termios path with
a local PTY, performs the Hello/Ready handshake, sends all 256 byte values once,
and verifies that the guest deadline releases a blocked read. It is not a UART
performance result.

The Windows host probe can be cross-compiled with:

```text
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go build -o /tmp/m3-host.exe ./cmd/m3-transport-probe
```

Static checks for the user-run Windows runner:

```text
python3 scripts/check-m3-runner.py scripts/test-m3-transport-windows.ps1
```

The real Windows/VirtualBox execution is user-run and is **NOT RUN** in this
public checkpoint.

## Windows runner limitation

`scripts/test-m3-transport-windows.ps1` pins the SHA-256 of the original
pre-rename image. After the module/source rename, a locally rebuilt image has a
different hash and will be rejected until the pinned value is deliberately
updated with review. Do not relax the check, reuse the old package as if it
were the new build, or treat this document as a new validation package.

## Limits

- Fixed, non-secret test data only; no throughput commitment for arbitrary
  payloads.
- One local VM/boot; no real UART loss/flow-control study beyond the fixed link.
- No forced mid-frame recovery, no automatic resynchronization, no independent
  PTYs/resize/session survival.
- No claim about Windows Terminal latency or production Agent readiness.
