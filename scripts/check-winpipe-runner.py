#!/usr/bin/env python3
"""Static and hash check for the Windows pipe native-test package.

Usage:
  check-winpipe-runner.py <package-dir> [--source <scripts/run-winpipe-tests.ps1>]
  check-winpipe-runner.py --self-test

Checks the UTF-8 BOM runner, required/forbidden runner content, the exact
SHA256SUMS file set, duplicates, malformed lines, traversal, missing files,
and every listed hash. It never executes PowerShell, the runner, or any
Windows binary; the PowerShell parser is NOT RUN here.
"""
import hashlib
import os
import re
import sys
import tempfile
from pathlib import Path

REQUIRED_FILES = {
    "winpipe.test.exe",
    "winrelay.test.exe",
    "run-winpipe-tests.ps1",
    "README.txt",
}
REQUIRED_TOKENS = [
    "SHA256SUMS",
    "System.Diagnostics.Process",
    "RedirectStandardOutput",
    "RedirectStandardError",
    "WaitForExit",
    "ExitCode",
    "CopyToAsync",
    "WaitAll",
    "Kill",
    "TimedOut",
    "KillFailed",
    "JoinTimedOut",
    "stderr",
    "results-winpipe-",
    "-test.v",
    "-test.timeout",
    "$CheckOnly",
    "$SelfTest",
    "exit 1",
    "exit 2",
]
FORBIDDEN = [
    "vboxmanage",
    "schtasks",
    "register-scheduledtask",
    "new-service",
    "sc.exe",
    "reg add",
    "remove-item",
    "del /f",
    "[console]::outputencoding",
]
LINE = re.compile(r"^([0-9a-fA-F]{64}) {2}([A-Za-z0-9._-]+)$")
# PowerShell does not continue an expression when a binary operator starts the
# next line (PS5.1 and PS7). The r4 runner failed exactly this way; keep a small
# static guard while the real parser remains the authority.
LEADING_OPERATOR = re.compile(r"^\s*(?:\+|-and\b|-or\b|\*|/|%)")


def load_manifest(pkg: Path) -> dict:
    sums = pkg / "SHA256SUMS"
    if not sums.is_file():
        raise ValueError("SHA256SUMS is missing")
    entries = {}
    for line in sums.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        m = LINE.match(line)
        if not m:
            raise ValueError(f"malformed SHA256SUMS line: {line!r}")
        digest, name = m.group(1).lower(), m.group(2)
        if name in {".", ".."} or ".." in name:
            raise ValueError(f"invalid manifest name: {name}")
        if name in entries:
            raise ValueError(f"duplicate manifest entry: {name}")
        if name not in REQUIRED_FILES:
            raise ValueError(f"unexpected manifest entry: {name}")
        entries[name] = digest
    missing = REQUIRED_FILES - set(entries)
    if missing:
        raise ValueError(f"manifest is missing {sorted(missing)}")
    for name, digest in entries.items():
        path = pkg / name
        if not path.is_file():
            raise ValueError(f"missing file: {name}")
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError(f"hash mismatch: {name}")
    return entries


def check_runner(pkg: Path) -> None:
    runner = pkg / "run-winpipe-tests.ps1"
    data = runner.read_bytes()
    if not data.startswith(b"\xef\xbb\xbf"):
        raise ValueError("runner has no UTF-8 BOM")
    text = data.decode("utf-8-sig")
    lower = text.lower()
    for token in REQUIRED_TOKENS:
        if token.lower() not in lower:
            raise ValueError(f"runner missing {token!r}")
    for token in FORBIDDEN:
        if token in lower:
            raise ValueError(f"runner contains forbidden {token!r}")
    for lineno, line in enumerate(text.splitlines(), 1):
        if LEADING_OPERATOR.match(line):
            raise ValueError(f"runner line {lineno} starts with a binary operator: {line.strip()!r}")


def self_test() -> int:
    with tempfile.TemporaryDirectory() as tmp:
        pkg = Path(tmp)
        bodies = {name: f"body-{name}".encode() for name in REQUIRED_FILES}
        for name, body in bodies.items():
            (pkg / name).write_bytes(body)
        good = "".join(
            f"{hashlib.sha256(bodies[n]).hexdigest()}  {n}\n" for n in sorted(REQUIRED_FILES)
        )
        cases = {
            "valid": good,
            "duplicate": good + f"{hashlib.sha256(bodies['README.txt']).hexdigest()}  README.txt\n",
            "extra": good + f"{'a' * 64}  extra.exe\n",
            "malformed": good + "not-a-hash  bad\n",
            "traversal": good.replace("README.txt", "../README.txt"),
            "missing-entry": "\n".join(good.splitlines()[:-1]) + "\n",
            "wrong-hash": good.replace(hashlib.sha256(bodies["README.txt"]).hexdigest(), "b" * 64),
        }
        for name, text in cases.items():
            (pkg / "SHA256SUMS").write_text(text, encoding="utf-8")
            try:
                load_manifest(pkg)
                ok = name == "valid"
            except ValueError:
                ok = name != "valid"
            if not ok:
                print(f"self-test: FAIL ({name})", file=sys.stderr)
                return 1
        bad_leading = "Write-Host ('a '\n    + 'b')\n"
        good_leading = "Write-Host ('a ' +\n    'b')\n"
        if not [l for l in bad_leading.splitlines() if LEADING_OPERATOR.match(l)]:
            print("self-test: FAIL (leading-operator guard missed the r4 pattern)", file=sys.stderr)
            return 1
        if [l for l in good_leading.splitlines() if LEADING_OPERATOR.match(l)]:
            print("self-test: FAIL (leading-operator guard false positive)", file=sys.stderr)
            return 1
    print("self-test: PASS (manifest cases + leading-operator guard)")
    return 0


def main() -> int:
    args = sys.argv[1:]
    if "--self-test" in args:
        return self_test()
    source = None
    if "--source" in args:
        i = args.index("--source")
        if i + 1 >= len(args):
            print("FAIL: --source needs a path", file=sys.stderr)
            return 2
        source = Path(args[i + 1])
        del args[i : i + 2]
    if len(args) != 1:
        print("usage: check-winpipe-runner.py <package-dir> [--source path]", file=sys.stderr)
        return 2
    pkg = Path(args[0])
    try:
        check_runner(pkg)
        entries = load_manifest(pkg)
    except (ValueError, OSError) as err:
        print(f"FAIL: {err}", file=sys.stderr)
        return 1
    if source is not None:
        if not source.is_file() or source.read_bytes() != (pkg / "run-winpipe-tests.ps1").read_bytes():
            print(f"FAIL: package runner differs from source {source}", file=sys.stderr)
            return 1
    print(f"PASS: static/hash/leading-operator check ({len(entries)} files); PowerShell parser NOT RUN")
    return 0


if __name__ == "__main__":
    sys.exit(main())
