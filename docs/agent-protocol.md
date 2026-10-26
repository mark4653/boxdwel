# Agent protocol

Status: implemented for the Linux guest agent. The Windows relay and VM
integration are not implemented yet.

The guest agent (`cmd/boxdwel-agent`) serves one interactive shell session per
`Open` over a byte stream, normally the COM2 serial device. The Windows relay
will speak the same frames. The M3 measurement protocol in `internal/m3probe`
is a separate experiment and is rejected by the frame magic.

## Framing

Every message is one frame: a 10-byte header followed by 0–4096 payload bytes,
all integers big-endian.

| field | size | meaning |
|---|---|---|
| magic | 2 | `0x42 0x77`; anything else closes the connection |
| version | 1 | `1`; other versions close the connection without a reply |
| type | 1 | message type below |
| control | 2 | correlation id for control messages, otherwise 0 |
| session | 2 | session id for session messages, otherwise 0 |
| length | 2 | payload bytes, at most 4096 |

A control message has `session == 0`; a session message has `control == 0` and
`session != 0`. Unknown types, a wrong direction, a bad header shape, a
truncated frame, or an oversized length is a protocol error. There is no
resynchronization, version downgrade, or ANSI skip.

| type | direction | scope | payload |
|---|---|---|---|
| Hello | host → guest | control | empty |
| Ready | guest → host | control | max sessions (1), reserved (1, must be 0), max payload (2), max output chunk (2) |
| Ping | host → guest | control | 8-byte nonce |
| Pong | guest → host | control | the same 8 bytes |
| Open | host → guest | session | rows (2), columns (2) |
| Opened | guest → host | session | rows (2), columns (2) after the PTY is running |
| Input | host → guest | session | 1–4096 bytes for the PTY |
| Output | guest → host | session | 1–1024 bytes read from the PTY |
| Resize | host → guest | session | rows (2), columns (2) |
| Close | host → guest | session | empty |
| Exited | guest → host | session | reason (1), exit code (4, signed) |
| Error | guest → host | control or session | code (1), message (0–255 bytes) |

`Ready`/`Pong` echo the control id of the request. `Open` uses a session id
chosen by the host; `Opened` repeats that id.

## Lifecycle

1. The host sends `Hello`. The guest replies `Ready` with its limits. No other
   first frame is accepted, and no shell exists before the handshake.
2. `Open` asks for a new session. The guest starts `/bin/sh -i` on a new PTY,
   replies `Opened` with the applied size, and then streams `Output`.
   `Input` goes into a bounded per-session queue (8 frames) drained by one
   worker, so a stalled PTY never blocks control frames or other sessions. A
   queue overflow, a blocked write over the guest input timeout (5 s), or a
   short write ends that session with `Error` code `5` instead of dropping
   bytes.
3. `Resize` applies terminal rows/columns (1–500 each) to the PTY and returns
   no reply. `stty size` in the shell reflects it.
4. The shell's own exit produces `Exited` with reason `0` (shell exit) and its
   exit code. Closing the session from the host (`Close`) kills the shell and
   signals the PTY's foreground process group; the guest then sends `Exited`
   with reason `1` (closed).
   Either way remaining `Output` is sent before `Exited`. After the shell
   exits the guest keeps draining output while it makes progress and ends the
   drain after 0.5 s without output, so a slow wire finishes the tail and an
   idle background process holding the slave cannot hold the session. If
   output is still arriving after 30 s, the session ends with `Error` code `4`
   instead of `Exited`; the cap is reported, not hidden. A forced `Close` may
   discard output still buffered in the PTY. Input or resize that arrives while
   the PTY is already closing is discarded; the reader still reports the
   session end.
5. `Error` is sent by the guest only. A session-scoped `Error` ends that
   session and no `Exited` follows; a control-scoped `Error` is followed by
   connection close. `Output` is serialized with this final frame, so no
   `Output` for the session follows `Error` or `Exited`. `Error` codes: `1` protocol/state, `2` session limit,
   `3` PTY start failure, `4` internal timeout, `5` input overflow, write
   timeout, or short write.
6. A clean wire EOF after the handshake closes all sessions and exits 0. Any
   read error or protocol violation is fatal for the agent process; a new agent
   start is a new connection.

Session ids are never reused within one connection. After `Exited` the id is
retired: later `Input`, `Resize`, or `Close` frames for it are discarded so a
late frame cannot reach another session, and a later `Open` with that id is a
protocol error. A refused or failed `Open` (limit reached, PTY start failure)
does not retire its id, so the host may retry it. Frames for an unknown
session id, like an unknown `Close`, are protocol errors.

## Limits

- At most 2 sessions at a time. A third `Open` returns `Error` code `2` and
  leaves the two running sessions alone.
- One frame payload is at most 4096 bytes; PTY output is emitted in chunks of
  at most 1024 bytes.
- Terminal dimensions are 1–500 rows and columns.
- A frame write waits at most 10 s before the whole connection is closed; a
  blocked input write waits at most 5 s. After the shell exits, output is
  drained until 0.5 s of quiet, with a 30 s cap that reports `Error` code `4`;
  the reaper waits at most 3 s. There is no total session lifetime limit.
- The stream's `Close` must release a pending read or write (a pollable
  `os.File` as opened by the agent, or a `net.Conn`). The agent closes the wire
  before joining its bounded readers and writers.

## Guest environment

The shell receives only `TERM=xterm-256color`, a fixed `PATH`, `HOME=/root` for
uid 0 (otherwise `/tmp`), `SHELL=/bin/sh`, and `PWD=/`. Its working directory
is `/`. No host or agent environment is inherited and no client environment is
accepted on the wire. The agent sends diagnostics to its own stderr (the guest
console), never to the wire.

## Cleanup

Each session owns its PTY master, slave and child process. On `Close`, wire
error, or agent exit the agent closes the PTY master, kills the shell through
`os.Process`, and signals the foreground process group read from its own PTY
(SIGKILL, so a job that ignores hangup dies too). `Wait` is owned by one
goroutine and bounded. Unrelated processes are never searched for or killed.

## Relay and client

The host relay (`internal/relay`) owns one guest wire and at most two client
streams. It completes the guest Hello/Ready handshake and relays one terminal
session per client. The Windows adapter (`internal/winpipe`, `internal/winrelay`)
implements the secure pipe listener/streams and the console client. VM-free
native Windows checks passed for pipe server/client round trips, the listener
and identity gates, two-client relay routing, and console mode/size/restore;
daemon, CLI, and VM/UART integration are not implemented yet. A pipe client
verifies the server PID, executable, and user before the handshake; the
listener keeps a pending pipe instance so the name has no gap between accepts.

- Attaching fails with `ErrBusy` while two clients are attached, and with
  `ErrClosed` after shutdown. `ErrClosed` wins even when both slots are still
  occupied; the future CLI reports it to the user. On an `Attach` error the
  caller still owns the stream.
- A client sends Hello/Ready (the relay reports one session), then exactly one
  Open with its own local session id. The relay allocates a separate guest id
  and never sends it to the client. A second Open, or any frame carrying
  another session id, ends that client stream.
- The relay validates client frames first: terminal sizes must be 1–500, and
  Input/Resize/Close before Opened are rejected. Only the offending client is
  closed; the guest and the other client are untouched. A client Ping is
  answered locally and is never forwarded to the guest.
- Guest Opened/Output/Exited and session Error are rewritten to the local id
  and delivered only to the owning client. Client Input/Resize/Close are
  rewritten to the guest id. One Close is sent per session; duplicate Close
  frames and input after a close are ignored. A guest must send Opened (or a
  session Error) and Exited within 5 s of the actual write; a missing
  confirmation stops the relay as a peer lifecycle error.
- Guest ids are allocated once per connection and never reused. Frames for a
  retired id are dropped; a guest id that was never allocated is a protocol
  error. A guest control error or transport failure stops the relay and closes
  every client with the cause.
- A client EOF, read/write error, or queue overflow closes only that client and
  its guest session. The relay and guest wire survive the last client, so a new
  client can attach. Each client has a bounded output queue; overflow closes
  that client without a success frame, never as a silent successful exit.
- Each client may have at most 16 queued guest input frames. A flood beyond
  that closes only that client; input still queued for a closing session is
  dropped instead of forwarded. The minted slot is reused only after the guest
  final, the client's stream/workers, and all queued guest input for that
  client have finished (written or dropped), so neither a slow final reader nor
  stale input can create clients beyond the two-slot limit or fill the shared
  guest queue.
- Limits: two clients/sessions; 64 client output frames; 64 guest data frames
  plus 8 control frames with a 16-frame per-client input budget; handshake and
  Open/Close confirmation 5 s; write 5 s per frame; a guest Ping every 5 s with
  a 2 s Pong deadline measured from the completed Ping write. The relay itself
  has no total lifetime limit.
