# Third-party components and licenses

BoxDwel's own source is MIT-licensed (see `LICENSE`). This file records the
third-party components that the build can use. Their licenses are not replaced
by MIT.

## Linux kernel

- Version and source: `boot/sources.lock` (official kernel.org tarball, HTTPS,
  SHA-256 pinned).
- License: GPL-2.0 with the Linux syscall note. The upstream `COPYING` states
  `SPDX-License-Identifier: GPL-2.0 WITH Linux-syscall-note`; the exception
  describes normal user-space syscalls and does not grant permission to copy
  kernel source into this project.
- This repository does not include kernel source. `scripts/build-boot.sh`
  downloads the official archive, checks its SHA-256, and builds it locally.

## BusyBox

- Version and source: `boot/sources.lock` (official busybox.net tarball, HTTPS,
  SHA-256 pinned).
- License: GPL-2.0 only. The upstream `LICENSE` and
  <https://busybox.net/license.html> state that version 2 is the only version
  under which current BusyBox may be distributed.
- This repository does not include BusyBox source. The build downloads the
  official archive and verifies its SHA-256.

## Distribution note

If you distribute a boot image or binary that contains Linux and/or BusyBox,
GPLv2 section 3 applies to those components. You must provide the complete
corresponding source for the exact binaries (including configuration and the
scripts used to build/install them), or use another section 3 option. A link to
an upstream download page by itself is not asserted here to satisfy every
distribution case. The repository's own MIT terms do not relicense the GPL
components.

## Other

The Go code uses only the Go standard library (`go.mod` has no external module
requirements). The M3 transport probe is project-authored experiment code; it
does not include third-party source.
