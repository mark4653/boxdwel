# Third-party components and licenses

BoxDwel's own source is MIT-licensed (see `LICENSE`). The components below keep
their licenses.

| Component | Source | License |
|---|---|---|
| Linux kernel 6.18.58 | Official kernel.org tarball pinned in `boot/sources.lock` (HTTPS URL and SHA-256) | GPL-2.0 with the Linux syscall note (`SPDX-License-Identifier: GPL-2.0 WITH Linux-syscall-note`) |
| BusyBox 1.38.0 | Official busybox.net tarball pinned in `boot/sources.lock` (HTTPS URL and SHA-256) | GPL-2.0 only; see upstream `LICENSE` and <https://busybox.net/license.html> |

The syscall note covers normal user-space syscalls; copied kernel source keeps
its upstream file-level license. `scripts/build-boot.sh` downloads both official
archives, verifies their SHA-256 values, and builds locally; the repository
does not vendor the source.

## Distribution

A boot image or binary containing Linux or BusyBox is an object-code
distribution of GPLv2 components. GPLv2 section 3 requires the complete
corresponding source for the exact binaries, including configuration and the
scripts used to build and install them, or another section 3 option. An
upstream download link alone may not cover every case. BoxDwel's MIT terms do
not relicense those components.

The Go code uses only the standard library; `go.mod` has no external module
requirements.
