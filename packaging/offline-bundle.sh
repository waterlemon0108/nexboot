#!/bin/bash
# 打完全离线的安装包：ndiskless .deb 加全部依赖 .deb，供不能上网的 PXE/iSCSI 隔离网段安装。
# 必须在与目标相同的 Ubuntu 版本（24.04）上运行，依赖包与版本绑定；只有打包机需要联网。
#
# 用法：DEB=<path-to-ndiskless.deb> packaging/offline-bundle.sh [outdir]
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)
deb=${DEB:?set DEB to the ndiskless .deb (from make deb)}
outdir=${1:-$repo/dist}
[ -f "$deb" ] || { echo "deb not found: $deb" >&2; exit 1; }
command -v apt-get >/dev/null || { echo "apt-get missing (build this on Ubuntu)" >&2; exit 1; }

. /etc/os-release
rel=${VERSION_ID:-unknown}
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/packages/deps"
cp "$deb" "$stage/packages/"

# 按 Depends 下载每个依赖及其递归依赖。
deps=$(dpkg-deb -f "$deb" Depends | tr ',' '\n' | sed -E 's/\|.*//; s/\(.*\)//; s/^[[:space:]]*//; s/[[:space:]]*$//' | awk 'NF')
echo "resolving dependencies for: $(echo "$deps" | tr '\n' ' ')"
( cd "$stage/packages/deps"
  # --reinstall 让本机已装的包也会被下载；再补上递归闭包，目标机不需要访问镜像源。
  apt-get install --yes --download-only --reinstall \
    -o Dir::Cache::archives="$(pwd)" $deps >/dev/null 2>&1 || true
  apt-get download $(apt-cache depends --recurse --no-recommends --no-suggests \
    --no-conflicts --no-breaks --no-replaces --no-enhances $deps \
    | grep '^\w' | sort -u) 2>/dev/null || true
  rm -f lock partial/* 2>/dev/null || true; rmdir partial 2>/dev/null || true )

count=$(find "$stage/packages/deps" -name '*.deb' | wc -l | tr -d ' ')
echo "bundled $count dependency package(s)"

cat > "$stage/install.sh" <<'INSTALL'
#!/bin/bash
# Offline install entry point. Run on the target (isolated segment, no internet).
#   1. installs every bundled .deb (deps first, then ndiskless)
#   2. hands you the configure command to run next
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
echo "installing bundled packages (offline)…"
dpkg -i "$here"/packages/deps/*.deb 2>/dev/null || true   # ordering resolved by the retry
dpkg -i "$here"/packages/deps/*.deb                        # second pass settles deps
dpkg -i "$here"/packages/*.deb
echo
echo "installed. now configure this node:"
echo "  sudo ndiskless-configure --bootstrap-password '<password>' \\"
echo "         --create-pool tank --pool-disks /dev/sdX"
echo "  (HA pair: add --peer <ip> --vip <ip> --cluster-token <s> --chap-secret <s>)"
INSTALL
chmod 0755 "$stage/install.sh"

cat > "$stage/现场卡片.txt" <<CARD
NexBoot 离线安装（Ubuntu $rel / amd64）

前提：这台机器在专用的管理/PXE 网段里，不需要联网。

1) 解压后进目录，装：
     sudo ./install.sh
2) 配置这台机器（选盘、设管理员口令）：
     sudo ndiskless-configure --bootstrap-password '你的口令' \\
            --create-pool tank --pool-disks /dev/sdX
   组主备再加： --peer 对端IP --vip 虚IP \\
            --cluster-token 同一个令牌 --chap-secret 同一个密钥
3) 浏览器打开 http://本机IP:8080

升级：先在备机 dpkg -i 新包并重启，确认接管后再动主机。
CARD

install -d -m 0755 "$outdir"
out="$outdir/nexboot-offline-ubuntu${rel}-amd64.tar.gz"
tar -C "$stage" -czf "$out" .
echo "$out"
