#!/bin/bash
# 造一个能过导入体检的最小「Windows 形状」镜像，给 boot_matrix / backup_matrix 当镜像源。
# 阻断项只有 Windows/System32 和 drivers/msiscsi.sys（见 internal/storage/local/inspect.go 的
# inspectWindows）；网卡 INF 和 Boot/BCD 只是 WARN，一并放进去让体检全绿。
# 它不能真正启动，那两个矩阵也不依赖镜像内容。1G 稀疏文件，实占几 MB，省得宿主机多备 60G。
# 用法：sudo ./mkwin.sh /var/lib/ndiskless/imports/winmin.raw 1G
# 依赖：parted、ntfs-3g（mkfs.ntfs）、losetup
set -euo pipefail
OUT=${1:-/var/lib/ndiskless/imports/winmin.raw}
SIZE=${2:-1G}
[ "$(id -u)" -eq 0 ] || { echo "要 root：挂载与 losetup 都需要" >&2; exit 1; }
for c in parted mkfs.ntfs losetup; do
  command -v "$c" >/dev/null || { echo "缺 $c（apt install parted ntfs-3g）" >&2; exit 1; }
done

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
truncate -s "$SIZE" "$OUT"
parted -s "$OUT" mklabel msdos mkpart primary ntfs 1MiB 100%
LOOP=$(losetup --find --show --partscan "$OUT")
MNT=$(mktemp -d)
trap 'umount "$MNT" 2>/dev/null || true; losetup -d "$LOOP" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true' EXIT
sleep 1
mkfs.ntfs -Q -F -L WINMIN "${LOOP}p1" >/dev/null
mount -t ntfs-3g "${LOOP}p1" "$MNT"

mkdir -p "$MNT/Windows/System32/drivers"
mkdir -p "$MNT/Windows/System32/DriverStore/FileRepository/e1000.inf_amd64"
mkdir -p "$MNT/Boot"
printf 'test fixture, not a real driver\n' > "$MNT/Windows/System32/drivers/msiscsi.sys"
printf 'fixture\n' > "$MNT/Boot/BCD"
cat > "$MNT/Windows/System32/DriverStore/FileRepository/e1000.inf_amd64/e1000.inf" <<'INF'
[Version]
Signature="$Windows NT$"
Class=Net
ClassGUID={4d36e972-e325-11ce-bfc1-08002be10318}
Provider=%Fixture%
[Manufacturer]
%Fixture%=Fixture,NTamd64
[Fixture.NTamd64]
%E1000.DeviceDesc%=E1000.ndi,PCI\VEN_8086&DEV_100E
%E1000.DeviceDesc%=E1000.ndi,PCI\VEN_8086&DEV_10D3
[Strings]
Fixture="e2e fixture"
E1000.DeviceDesc="Intel(R) PRO/1000 (fixture)"
INF
sync
umount "$MNT"
losetup -d "$LOOP"
rmdir "$MNT"
trap - EXIT
echo "  已生成 $OUT  表观 $(du -h --apparent-size "$OUT" | cut -f1)  实占 $(du -h "$OUT" | cut -f1)"
