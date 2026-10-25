#!/usr/bin/env python3
"""Bounded PTY self-check for the M3 transport probe.

Usage: check-m3-probe-pty.py <probe-binary>

Runs the real guest probe on a local PTY (-dev <pts>), performs the
Hello/Ready handshake, checks raw termios, sends all 256 byte values once and
requires the exact echo. Then it leaves the probe idle and requires the guest
self-deadline to end it (Close must release the blocked tty read).
This is a local termios/byte/deadline check, not a UART measurement.
"""
import os
import pty
import select
import struct
import subprocess
import sys
import termios
import time

HEADER = struct.Struct(">BBHH")
NONCE = b"pty-all-bytes"
MAX_PAYLOAD = 4096
READ_LIMIT = 10.0


def read_exact(fd, n, deadline):
    buf = b""
    while len(buf) < n:
        left = deadline - time.monotonic()
        if left <= 0:
            raise TimeoutError("read deadline")
        ready, _, _ = select.select([fd], [], [], left)
        if not ready:
            raise TimeoutError("read deadline")
        chunk = os.read(fd, n - len(buf))
        if not chunk:
            raise TimeoutError("unexpected eof")
        buf += chunk
    return buf


def read_frame(fd, deadline):
    version, ftype, ident, length = HEADER.unpack(read_exact(fd, 6, deadline))
    if length > MAX_PAYLOAD:
        raise ValueError("payload length %d" % length)
    return version, ftype, ident, read_exact(fd, length, deadline)


def main():
    if len(sys.argv) != 2:
        print("usage: check-m3-probe-pty.py <probe-binary>", file=sys.stderr)
        return 2
    binary = os.path.abspath(sys.argv[1])
    if not os.path.isfile(binary):
        print("missing binary: %s" % binary, file=sys.stderr)
        return 2

    master, slave = pty.openpty()
    slave_path = os.ttyname(slave)
    begin = time.monotonic()
    proc = subprocess.Popen(
        [binary, "-dev", slave_path, "-deadline", "1s"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        close_fds=True,
    )
    results = {}
    deadline = begin + READ_LIMIT
    try:
        # Wait until the probe has put the tty in raw mode, otherwise the
        # line discipline would echo our Hello back before the child reads it.
        raw = False
        while time.monotonic() < deadline:
            try:
                if not (termios.tcgetattr(slave)[3] & termios.ECHO):
                    raw = True
                    break
            except termios.error:
                pass
            time.sleep(0.01)
        results["raw_before_hello"] = raw
        if not raw:
            raise TimeoutError("probe did not set raw mode")
        os.write(master, HEADER.pack(1, 7, 0, len(NONCE)) + NONCE)  # Hello
        version, ftype, ident, payload = read_frame(master, deadline)
        results["ready_reflects_nonce"] = (version, ftype, ident) == (1, 1, 0) and payload == NONCE

        iflag, oflag, cflag, lflag, ispeed, ospeed, _cc = termios.tcgetattr(slave)
        iflags = termios.IGNBRK | termios.BRKINT | termios.PARMRK | termios.ISTRIP
        iflags |= termios.INLCR | termios.IGNCR | termios.ICRNL | termios.IXON | termios.IXOFF
        results["raw_input_flags"] = (iflag & iflags) == 0
        results["raw_no_echo_canon"] = (lflag & (termios.ECHO | termios.ICANON | termios.ISIG)) == 0
        results["raw_no_output_flags"] = oflag == 0
        results["raw_8n1"] = (
            (cflag & termios.CSIZE) == termios.CS8
            and (cflag & (termios.PARENB | termios.CSTOPB)) == 0
        )
        results["raw_115200"] = ispeed == termios.B115200 and ospeed == termios.B115200
        if hasattr(termios, "CRTSCTS"):
            results["raw_no_flow_control"] = (cflag & termios.CRTSCTS) == 0

        payload = bytes(range(256))
        os.write(master, HEADER.pack(1, 2, 0, len(payload)) + payload)
        version, ftype, ident, echoed = read_frame(master, deadline)
        results["echo_all_256_bytes"] = (version, ftype, ident) == (1, 3, 0) and echoed == payload

        # Stay idle: the guest's own 1s deadline must close the wire and end it.
        try:
            code = proc.wait(timeout=6)
        except subprocess.TimeoutExpired:
            proc.kill()
            code = None
        elapsed = time.monotonic() - begin
        stderr = proc.stderr.read().decode("utf-8", "replace").strip()
        results["deadline_exit"] = code == 1 and "guest deadline reached" in stderr
        results["hard_deadline_unused"] = "hard deadline" not in stderr
        results["bounded"] = elapsed < 8.0
    except Exception as exc:  # noqa: BLE001 - report the failure, never hang
        results["error:%s" % exc] = False
        try:
            proc.kill()
            proc.wait(timeout=2)
        except (OSError, subprocess.TimeoutExpired):
            pass
    finally:
        for fd in (master, slave):
            try:
                os.close(fd)
            except OSError:
                pass

    for name, ok in results.items():
        print("%s = %s" % (name, ok))
    ok = bool(results) and all(results.values())
    print("RESULT =", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
