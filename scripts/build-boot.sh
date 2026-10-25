#!/usr/bin/env bash
# M1: Linux 커널 + 정적 BusyBox + 내장 initramfs + GPT/FAT EFI raw 이미지(파일) 빌드.
# sudo/패키지 설치/mount/loop/물리 디스크를 쓰지 않는다. 새 출력은 dist/m1 아래에만 만든다.
# 재현: 같은 입력이면 make가 incremental로 처리하고, 새 산출물은 실행별 exclusive 디렉터리에만 쓴다.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo"
# shellcheck source=boot/sources.lock
. boot/sources.lock

m1=$repo/dist/m1
src=$m1/sources
build=$m1/build
stamp=$(date -u +%Y%m%dT%H%M%SZ)
log=$m1/logs/build-$stamp
out=$m1/out/$stamp
kernel_src=$build/linux-$linux_version
busybox_src=$build/busybox-$busybox_version
kernel_tar=$src/linux-$linux_version.tar.xz
busybox_tar=$src/busybox-$busybox_version.tar.bz2

die() { echo "FAIL: $*"; exit 1; }
sha_of() { sha256sum "$1" | cut -d' ' -f1; }

# M3 사전 실험 분기: M3_PROBE_BIN(절대경로 기존 파일)이 있을 때만 전용 init과
# probe binary를 넣고, 산출물을 dist/m3/boot-<stamp> 아래 새 exclusive로 격리한다.
selected_init=$repo/boot/init
m3_mode=0
m3_probe=
if [ -n "${M3_PROBE_BIN:-}" ]; then
	m3_mode=1
	case $M3_PROBE_BIN in
	/*) ;;
	*) die "M3_PROBE_BIN must be an absolute path" ;;
	esac
	[ -f "$M3_PROBE_BIN" ] || die "M3_PROBE_BIN is not an existing file: $M3_PROBE_BIN"
	selected_init=$repo/boot/init-m3-probe
	[ -f "$selected_init" ] || die "missing M3 init: $selected_init"
	m3_probe=$M3_PROBE_BIN
	m3root=$repo/dist/m3/boot-$stamp
	log=$m3root/logs
	out=$m3root/out
fi

mkdir -p "$m1/logs" "$src" "$build" "$m1/out"
if [ "$m3_mode" = 1 ]; then mkdir -p "$m3root"; fi
mkdir "$log" 2>/dev/null || die "로그 디렉터리가 이미 있다: $log"
mkdir "$out" 2>/dev/null || die "출력 디렉터리가 이미 있다(덮어쓰지 않음): $out"
exec > >(tee -a "$log/build.log") 2>&1
echo "build_start=$(date -u +%FT%TZ)"
echo "out=$out"

fetch() { # url dest — 이미 있으면 그대로 검증에 재사용한다. 자동 재시도 없음.
	[ -s "$2" ] && return 0
	curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"
}

echo "== 1a. 공식 체크섬 자료"
fetch "$linux_checksum_url" "$src/sha256sums.asc"
fetch "$busybox_checksum_url" "$src/busybox-$busybox_version.tar.bz2.sha256"
grep -qxF "$linux_sha256  linux-$linux_version.tar.xz" "$src/sha256sums.asc" ||
	die "kernel.org 체크섬 자료에 linux-$linux_version.tar.xz $linux_sha256 가 없다"
grep -qxF "$busybox_sha256  busybox-$busybox_version.tar.bz2" \
	"$src/busybox-$busybox_version.tar.bz2.sha256" ||
	die "busybox.net 체크섬 자료가 lock과 다르다"
echo "note: 체크섬 파일은 HTTPS 원본이며 PGP 서명 검증은 하지 않는다(키 수집 금지)."

echo "== 1b. 소스 archive"
fetch "$linux_url" "$kernel_tar"
fetch "$busybox_url" "$busybox_tar"
[ "$(sha_of "$kernel_tar")" = "$linux_sha256" ] || die "SHA-256 불일치: $kernel_tar"
[ "$(sha_of "$busybox_tar")" = "$busybox_sha256" ] || die "SHA-256 불일치: $busybox_tar"
echo "verified kernel sha256=$linux_sha256"
echo "verified busybox sha256=$busybox_sha256"

echo "== 2. 소스 추출"
[ -d "$kernel_src" ] || tar -xJf "$kernel_tar" -C "$build"
[ -d "$busybox_src" ] || tar -xjf "$busybox_tar" -C "$build"

echo "== 3. 정적 BusyBox (-j4)"
make -C "$busybox_src" defconfig >"$log/busybox-config.log" 2>&1
# defconfig가 일부 신규 심볼을 비워 두므로 먼저 기본값으로 완성한다.
make -C "$busybox_src" oldconfig </dev/null >>"$log/busybox-config.log" 2>&1
while read -r line; do
	case $line in
	'') continue ;;
	'# '*) name=${line#\# }; name=${name% is not set} ;;
	*) name=${line%%=*} ;;
	esac
	case $name in CONFIG_*) ;; *) continue ;; esac
	sed -i "/^$name=/d; /^# $name is not set$/d" "$busybox_src/.config"
	printf '%s\n' "$line" >>"$busybox_src/.config"
done <boot/busybox.config.fragment
make -C "$busybox_src" oldconfig </dev/null >>"$log/busybox-config.log" 2>&1
make -C "$busybox_src" -j4 >"$log/busybox-build.log" 2>&1 ||
	die "BusyBox 빌드 실패($log/busybox-build.log)"
cp "$busybox_src/.config" "$out/busybox.config"
grep -qx "CONFIG_STATIC=y" "$out/busybox.config" || die "CONFIG_STATIC=y 아님"
grep -qx "# CONFIG_LOGIN is not set" "$out/busybox.config" || die "LOGIN applet 제외 안 됨"
grep -qx "CONFIG_LAST_SUPPORTED_WCHAR=0" "$out/busybox.config" || die "unicode cap 해제 안 됨"
grep -qx "CONFIG_UNICODE_WIDE_WCHARS=y" "$out/busybox.config" || die "wide wchar 꺼짐"
python3 scripts/check-guest-unicode.py "$busybox_src/busybox" >"$log/busybox-unicode-pty.log" 2>&1 ||
	die "BusyBox line editor UTF-8 selfcheck 실패($log/busybox-unicode-pty.log)"

echo "== 4. initramfs manifest"
manifest=$build/initramfs.list
if [ "$m3_mode" = 1 ]; then manifest=$build/initramfs-m3.list; fi
{
	echo "dir /dev 0755 0 0"
	echo "nod /dev/console 0600 0 0 c 5 1"
	echo "dir /proc 0755 0 0"
	echo "dir /sys 0755 0 0"
	echo "dir /bin 0755 0 0"
	echo "file /init $selected_init 0755 0 0"
	echo "file /bin/busybox $busybox_src/busybox 0755 0 0"
	echo "slink /bin/sh busybox 0777 0 0"
	for applet in uname sleep stty cat; do
		echo "slink /bin/$applet busybox 0777 0 0"
	done
	if [ "$m3_mode" = 1 ]; then
		echo "file /bin/m3-transport-probe $m3_probe 0755 0 0"
	fi
} >"$manifest"

echo "== 5. 커널 (-j4)"
kstart=$(date +%s)
make -C "$kernel_src" defconfig >"$log/kernel-config.log" 2>&1
"$kernel_src/scripts/kconfig/merge_config.sh" -m -O "$kernel_src" \
	"$kernel_src/.config" "$repo/boot/kernel.config.fragment" >>"$log/kernel-config.log" 2>&1
"$kernel_src/scripts/config" --file "$kernel_src/.config" \
	--set-str INITRAMFS_SOURCE "$manifest"
make -C "$kernel_src" olddefconfig >>"$log/kernel-config.log" 2>&1
make -C "$kernel_src" -j4 bzImage >"$log/kernel-build.log" 2>&1 ||
	die "커널 빌드 실패($log/kernel-build.log)"
echo "kernel_make_seconds=$(( $(date +%s) - kstart ))"
cp "$kernel_src/.config" "$out/kernel.config"
grep -qx "CONFIG_EFI_STUB=y" "$out/kernel.config" || die "EFI_STUB=y 아님"
grep -qx "CONFIG_DEVTMPFS=y" "$out/kernel.config" || die "DEVTMPFS=y 아님"
grep -q '^CONFIG_CMDLINE="console=ttyS0,115200 rdinit=/init earlycon=uart8250,io,0x3f8,115200"$' "$out/kernel.config" ||
	die "builtin cmdline 불일치"
grep -qx "CONFIG_DEBUG_INFO_NONE=y" "$out/kernel.config" || die "debug 정보 제거 안 됨"
grep -qxF "CONFIG_INITRAMFS_SOURCE=\"$manifest\"" "$out/kernel.config" ||
	die "INITRAMFS_SOURCE가 절대경로 manifest가 아님"

bzimage=$kernel_src/arch/x86/boot/bzImage
cp "$bzimage" "$out/BOOTX64.EFI"

echo "== 6. GPT/FAT raw 이미지"
raw=$out/m1-boot.img
esp=$out/esp.img
truncate -s 64M "$raw"
sgdisk --clear -n 1:2048:+62M -t 1:EF00 -c 1:ESP "$raw" >"$log/sgdisk-create.log" 2>&1
first=$(sgdisk -i 1 "$raw" | awk '/^First sector:/ {print $3}')
psize=$(sgdisk -i 1 "$raw" | awk '/^Partition size:/ {print $3}')
[ -n "$first" ] && [ -n "$psize" ] || die "GPT 파티션 offset/size 파싱 실패"
echo "esp first_sector=$first size_sectors=$psize"
truncate -s $((psize * 512)) "$esp"
mkfs.fat -F 32 -n M1ESP "$esp" >"$log/mkfs-fat.log" 2>&1
mmd -i "$esp" ::/EFI ::/EFI/BOOT
mcopy -i "$esp" "$out/BOOTX64.EFI" ::/EFI/BOOT/BOOTX64.EFI
dd if="$esp" of="$raw" bs=512 seek="$first" count="$psize" conv=notrunc status=none

echo "== 7. 기본 검사 1묶음"
bash -n "$selected_init" || die "selected init 구문 오류"
marker=$(grep -n '^echo "READY_BOOT_M1"$' "$selected_init" | cut -d: -f1)
[ -n "$marker" ] || die "selected init에 READY_BOOT_M1 marker 없음"
[ "$(grep -c '^echo "READY_BOOT_M1"$' "$selected_init")" = 1 ] || die "marker 출력이 1회가 아님"
for pat in 'mount -t devtmpfs' 'mount -t proc' 'mount -t sysfs' \
	'-c /dev/console' '-r /proc/1/cmdline' '-d /sys/kernel'; do
	line=$(grep -n -m1 -F -- "$pat" "$selected_init" | cut -d: -f1)
	[ -n "$line" ] && [ "$line" -lt "$marker" ] || die "$selected_init: '$pat' 확인이 marker 뒤/누락"
done
grep -q 'exit 1' "$selected_init" || die "selected init 실패 경로가 exit 1이 아님"
if [ "$m3_mode" = 1 ]; then
	grep -qF '/dev/ttyS1' "$selected_init" || die "M3 init: ttyS1 확인 없음"
	grep -qF 'stty -F /dev/ttyS1 raw 115200 -echo' "$selected_init" || die "M3 init: ttyS1 raw 설정 없음"
	grep -qF '/bin/m3-transport-probe -dev /dev/ttyS1 -deadline 120s' "$selected_init" || die "M3 init: probe 실행 줄 없음"
	[ "$(grep -c '^run_phase [12]$' "$selected_init")" = 2 ] || die "M3 init: phase 실행이 정확히 2회가 아님"
	grep -qF 'M3_PROBE_DONE' "$selected_init" || die "M3 init: 완료 marker 없음"
	! grep -q 'cttyhack' "$selected_init" || die "M3 init: cttyhack이 남아 있음"
	! grep -q 'sh -i' "$selected_init" || die "M3 init: 대화형 셸이 남아 있음"
	! grep -q 'M2_SHELL_EXIT' "$selected_init" || die "M3 init: 고정 종료 marker가 남아 있음"
	applets="mount sleep stty cat sh"
else
	grep -q 'cttyhack /bin/sh -i' "$selected_init" || die "/init: 제어 터미널 셸 시작 없음"
	grep -q 'stty -F /dev/ttyS0 sane' "$selected_init" || die "/init: tty sane 초기화 없음"
	grep -q 'setsid "\$BB" cttyhack' "$selected_init" || die "/init: setsid cttyhack 없음"
	! grep -q "M2_SHELL_EXIT" "$selected_init" || die "/init: 고정 종료 marker가 남아 있음"
	m2line=$(grep -n -m1 -F 'cttyhack /bin/sh -i' "$selected_init" | cut -d: -f1)
	[ -n "$m2line" ] && [ "$m2line" -gt "$marker" ] || die "/init: 셸 시작이 marker 뒤가 아님"
	applets="cttyhack uname sleep stty cat sh"
fi
for applet in $applets; do
	"$busybox_src/busybox" --list | grep -qx "$applet" || die "BusyBox applet 누락: $applet"
done
[ -x "$busybox_src/busybox" ] || die "BusyBox 실행 파일 없음"
file "$busybox_src/busybox" >"$log/file.log"
file "$out/BOOTX64.EFI" >>"$log/file.log"
grep -q "statically linked" "$log/file.log" || die "BusyBox가 정적 링크가 아님"
[ "$(readelf -l "$busybox_src/busybox" | grep -c INTERP)" = 0 ] || die "BusyBox에 interpreter가 있음"
objdump -p "$out/BOOTX64.EFI" >"$log/objdump-efi.log"
grep -q "Subsystem.*EFI application" "$log/objdump-efi.log" || die "EFI subsystem 아님"
sgdisk -v "$raw" >"$log/sgdisk-verify.log" 2>&1
grep -q "No problems found" "$log/sgdisk-verify.log" || die "GPT 검증 실패"
dd if="$raw" of="$out/esp-readback.img" bs=512 skip="$first" count="$psize" status=none
cmp "$esp" "$out/esp-readback.img" || die "raw 이미지 ESP 영역이 esp.img와 다름"
mdir -i "$out/esp-readback.img" ::/EFI/BOOT >"$log/mdir.log" 2>&1
mcopy -i "$out/esp-readback.img" ::/EFI/BOOT/BOOTX64.EFI "$out/BOOTX64-esp-copy.efi"
cmp "$out/BOOTX64.EFI" "$out/BOOTX64-esp-copy.efi" || die "ESP 내 EFI 파일이 빌드 커널과 다름"

echo "== 8. 초기화 archive 검사"
# 커널은 usr/initramfs_inc_data(gzip) 또는 usr/initramfs_data.cpio(raw)를 .init.data에 그대로 넣는다.
if grep -qx "CONFIG_INITRAMFS_COMPRESSION_GZIP=y" "$out/kernel.config"; then
	blob=$kernel_src/usr/initramfs_inc_data
	[ -s "$blob" ] || die "생성된 initramfs gzip archive 없음"
	cp "$blob" "$out/initramfs.gz"
	python3 - "$blob" "$kernel_src/vmlinux" <<'PY' || die "생성 archive가 vmlinux에 그대로 들어있지 않음"
import sys
blob = open(sys.argv[1], "rb").read()
sys.exit(0 if blob and blob in open(sys.argv[2], "rb").read() else 1)
PY
	gzip -cd "$out/initramfs.gz" >"$out/initramfs.cpio" || die "initramfs gzip 해제 실패"
elif grep -qx "CONFIG_INITRAMFS_COMPRESSION_NONE=y" "$out/kernel.config"; then
	blob=$kernel_src/usr/initramfs_data.cpio
	[ -s "$blob" ] || die "생성된 initramfs cpio archive 없음"
	cp "$blob" "$out/initramfs.cpio"
	python3 - "$blob" "$kernel_src/vmlinux" <<'PY' || die "생성 archive가 vmlinux에 그대로 들어있지 않음"
import sys
blob = open(sys.argv[1], "rb").read()
sys.exit(0 if blob and blob in open(sys.argv[2], "rb").read() else 1)
PY
else
	die "지원하지 않는 initramfs 압축 설정"
fi
( cd "$out" && cpio -i --to-stdout init <"$out/initramfs.cpio" >"$out/init-from-cpio" )
cmp "$out/init-from-cpio" "$selected_init" || die "내장 initramfs의 /init이 selected init과 다름"
cpio -it <"$out/initramfs.cpio" >"$out/initramfs.list.out" 2>/dev/null
for entry in bin/sh bin/uname bin/sleep bin/stty bin/cat; do
	grep -qx "$entry" "$out/initramfs.list.out" || die "initramfs 항목 누락: $entry"
done
if [ "$m3_mode" = 1 ]; then
	grep -qx "bin/m3-transport-probe" "$out/initramfs.list.out" || die "initramfs probe 항목 누락"
	( cd "$out" && cpio -i --to-stdout bin/m3-transport-probe <"$out/initramfs.cpio" >"$out/probe-from-cpio" )
	cmp "$out/probe-from-cpio" "$m3_probe" || die "initramfs의 probe가 M3_PROBE_BIN과 다름"
fi

echo "== 9. 기록"
{
	sha256sum "$out/BOOTX64.EFI" "$out/m1-boot.img" "$out/esp.img" \
		"$out/esp-readback.img" "$out/BOOTX64-esp-copy.efi" \
		"$out/kernel.config" "$out/busybox.config" \
		"$out/initramfs.cpio" "$out/init-from-cpio" "$busybox_src/busybox"
	if [ "$m3_mode" = 1 ]; then sha256sum "$out/probe-from-cpio"; fi
} >"$out/SHA256SUMS.txt"
{
	echo "build_stamp=$stamp"
	echo "built_at=$(date -u +%FT%TZ)"
	echo "linux=$linux_version $linux_url sha256=$linux_sha256"
	echo "busybox=$busybox_version $busybox_url sha256=$busybox_sha256"
	echo "kernel_config=x86_64_defconfig + boot/kernel.config.fragment"
	echo "busybox_config=defconfig + boot/busybox.config.fragment"
	echo "init_mode=$([ "$m3_mode" = 1 ] && echo m3-probe || echo m2-shell)"
	echo "selected_init=$selected_init"
	if [ "$m3_mode" = 1 ]; then echo "probe_sha256=$(sha_of "$m3_probe") m3_probe_bin=$m3_probe"; fi
	echo "cc=$(gcc -dumpfullversion) ld=$(ld --version | sed -n 1p)"
	echo "gpt_first_sector=$first gpt_size_sectors=$psize"
	echo "sizes: $(stat -c '%n=%s' "$out"/*.img "$out"/BOOTX64.EFI | tr '\n' ' ')"
} >"$out/build-info.txt"

echo "== done"
cat "$out/build-info.txt"
cat "$out/SHA256SUMS.txt"
echo "log=$log"
echo "build_end=$(date -u +%FT%TZ)"
