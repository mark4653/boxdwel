# BoxDwel architecture

## Components

| Path | Role |
|---|---|
| `cmd/boxdwel` | Windows/amd64 CLI: `version`, `doctor`, `enter test`. `doctor` checks the host and locates `VBoxManage`; it does not create or modify VMs. |
| `cmd/boxdwel-agent` | Linux guest agent: product protocol, session manager, and real PTY shells. |
| `internal/host/vbox` | `VBoxManage --version` detection with a timeout and explicit errors. |
| `internal/enter` | Windows COM1 session, ownership checks, and VM validation. |
| `internal/agentproto` | Product frame codec shared by the relay and the agent. |
| `internal/agent` | Linux-only PTY, fixed guest environment, and session lifecycle. |
| `internal/relay` | Platform-neutral host relay: guest handshake, two client streams, id mapping, routing, queues, and Ping/Pong liveness. |
| `internal/winpipe` | Windows secure named-pipe transport: user+SYSTEM DACL, first-instance namespace, overlapped streams, server identity check, console mode/size. |
| `internal/winrelay` | Windows relay adapter: accept/attach loop and the console terminal client. |
| `cmd/m3-transport-probe` + `internal/m3probe` | Experimental COM2 transport; separate from the product Agent. See [M3](m3-transport-spike.md). |
| `boot/` | Linux 6.18.58 and BusyBox 1.38.0 config fragments, source URL/SHA lock, and M2/M3 init scripts. |
| `scripts/` | Go build/test scripts, boot-image build, and Windows user-run validation scripts. |

## COM1 flow

1. `enter test` reads the original owner record and M2 metadata, then checks
   the fixed UUID, name, config path, CPU/RAM/firmware, NICs, UART, SATA VDI,
   and the prepared image SHA-256. Every path in the chain is checked for
   reparse points.
2. The host creates a first-instance named pipe with a current-user+SYSTEM
   DACL and rejects remote clients. The single instance is the session lock.
3. A powered-off VM starts headless. A running M2 VM is reused by reconnecting
   only UART1; the VM is not rebooted.
4. The guest `/init` mounts devtmpfs/proc/sysfs, verifies `/dev/console`,
   `/proc/1/cmdline`, and `/sys/kernel`, then prints `READY_BOOT_M1`, runs
   `stty sane`, and respawns `setsid busybox cttyhack /bin/sh -i`. PID 1 stays
   alive.
5. The host injects a nonce `trap`/ready command and accepts only the exact
   ready and exit lines for that nonce.
6. Console raw/VT/UTF-8 mode and the code page are restored on success and
   error. The VM stays running after the shell exits.

After locking, and again before each VM-changing operation, `showvminfo` is
re-read and the UUID/name/config, SATA target, and resources are revalidated. A
failure stops the next operation. This is a pre-operation check, not a rollback
of earlier changes. VM identity, local paths, and pipe names keep their
historical `vboxdistro-*` values.

## Boot configuration

`scripts/build-boot.sh` uses the pinned archives to build an EFI-stub kernel,
an embedded initramfs, and a GPT/FAT raw image. The M2 init provides the single
COM1 shell. The separate M3 init runs the measurement probe and has no shell.
The script fetches sources at build time; the repository contains only config,
init, and the URL/SHA lock.

## Product agent

`cmd/boxdwel-agent` opens the serial device (`/dev/ttyS1` by default) as a raw
115200 8N1 link and serves one product-protocol connection. After `Hello` it
accepts up to two `Open` requests and starts `/bin/sh -i` on a new PTY for each,
with a fixed `TERM`/`PATH`/`HOME`/`SHELL` environment and no inherited host
variables. Input, output, resize, ping, close, and exit use the frames in
[the agent protocol](agent-protocol.md). Input runs through a bounded
per-session queue, so a stalled PTY cannot block the receive loop; overflow
and blocked input writes are reported and end that session. A slow wire is
bounded instead by the per-frame write timeout, which closes the whole
connection. Each session owns its PTY and child process group; close, wire
error, or agent exit closes the master, kills the terminal's foreground group
and the shell through `os.Process`, and waits a bounded time for `Wait`. After
a natural shell exit, remaining output is drained while it progresses, so a
slow wire keeps the tail.

The relay core (`internal/relay`) connects one guest wire to two client
streams and keeps running after the last client. The Windows adapter
(`internal/winpipe`, `internal/winrelay`) provides the secure pipe listener,
overlapped streams, server identity verification, and the console client.
VM-free native Windows checks passed for pipe server/client round trips, the
listener and identity gates, two-client relay routing, and console
mode/size/restore. Daemon, CLI, VM/UART, and Agent integration are not
implemented. The boot image does not start the agent yet. The M3 probe remains
a separate experiment.

## M3

The transport probe, startup gate, historical measurements, and limits are in
[the M3 document](m3-transport-spike.md).
