#!/usr/bin/env python3
"""Bounded guest-like PTY self-check: BusyBox ash line editing must keep UTF-8.

Usage: check-guest-unicode.py <path-to-busybox>
"""
import fcntl
import os
import pty
import select
import signal
import struct
import sys
import termios
import time

COMMAND = 'echo "한글 UTF-8"'.encode()
EXPECT = "한글 UTF-8".encode()


def read_until(fd, quiet=0.25, limit=3.0):
    buf = b""
    end = time.monotonic() + limit
    last = time.monotonic()
    while time.monotonic() < end:
        ready, _, _ = select.select([fd], [], [], max(0.0, min(quiet, end - time.monotonic())))
        if not ready:
            if buf and time.monotonic() - last >= quiet:
                break
            continue
        try:
            chunk = os.read(fd, 4096)
        except OSError:
            break
        if not chunk:
            break
        buf += chunk
        last = time.monotonic()
    return buf


def main():
    if len(sys.argv) != 2 or not os.path.isfile(sys.argv[1]):
        print("usage: check-guest-unicode.py <busybox>", file=sys.stderr)
        return 2
    bb = os.path.abspath(sys.argv[1])
    pid, fd = pty.fork()
    if pid == 0:
        env = {"TERM": "linux", "PATH": "/bin", "HOME": "/", "PS1": "# "}
        os.execve(bb, [bb, "sh", "-i"], env)
    reaped = False
    try:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
        read_until(fd)
        os.write(fd, COMMAND)
        edit = read_until(fd)
        os.write(fd, b"\n")
        run = read_until(fd)
        # One ASCII delete: "echo AB<DEL>" must execute as "echo A".
        os.write(fd, b"echo AB\x7f")
        edit_bs = read_until(fd)
        os.write(fd, b"\n")
        run_bs = read_until(fd)
        os.write(fd, b"exit\n")
        read_until(fd)
        for _ in range(20):
            done, _ = os.waitpid(pid, os.WNOHANG)
            if done:
                reaped = True
                break
            time.sleep(0.05)
        if not reaped:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
            reaped = True
        print("edit_display_repr =", repr(edit))
        print("run_output_repr =", repr(run))
        print("backspace_edit_repr =", repr(edit_bs))
        print("backspace_run_repr =", repr(run_bs))
        checks = {
            "edit_display_utf8": EXPECT in edit,
            "run_output_utf8": EXPECT in run,
            "backspace_edit_ascii": b"AB" in edit_bs,
            "backspace_run_ascii": b"\r\nA\r\n" in run_bs,
        }
        for name, ok in checks.items():
            print("%s = %s" % (name, ok))
        ok = all(checks.values())
        print("RESULT =", "PASS" if ok else "FAIL")
        return 0 if ok else 1
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
        if not reaped:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                os.waitpid(pid, 0)
            except ChildProcessError:
                pass


if __name__ == "__main__":
    sys.exit(main())
