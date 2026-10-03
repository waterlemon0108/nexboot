#!/bin/sh
# ndiskless 数据盘挂载：导入 Linux 镜像时写入，每次开机由 nd-datadisks.service 运行。
# 从 iBFT 取服务器地址与本机 MAC，向服务器要「LUN -> 挂载点」，按 LUN 找到 iSCSI 盘挂上。
# 任何失败只写日志，不阻塞开机。日志：/var/log/nd-datadisks.log（每次开机覆盖）。
port='__ND_API_PORT__'
exec >/var/log/nd-datadisks.log 2>&1
echo "start $(date -Is)"

ibft=/sys/firmware/ibft
[ -d "$ibft" ] || modprobe iscsi_ibft 2>/dev/null
server=$(cat "$ibft"/target0/ip-addr 2>/dev/null)
mac=$(cat "$ibft"/ethernet0/mac 2>/dev/null)
if [ -z "$server" ] || [ -z "$mac" ]; then
	echo "no iBFT target/mac; skip"
	exit 0
fi

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 10 "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 10 -O - "$1"
	else
		echo "neither curl nor wget" >&2
		return 1
	fi
}

url="http://$server:$port/boot/data-disks?mac=$mac&format=text"
list=$(fetch "$url") || { echo "fetch $url failed; skip"; exit 0; }
echo "disks from $server mac=$mac:"
echo "$list"

# 盘可能比服务晚出现：最多等 30 秒。
find_disk() {
	i=0
	while [ $i -lt 30 ]; do
		for d in /dev/disk/by-path/*-iscsi-*-lun-"$1"; do
			[ -e "$d" ] && { echo "$d"; return 0; }
		done
		sleep 1
		i=$((i + 1))
	done
	return 1
}

echo "$list" | while IFS="$(printf '\t')" read -r lun target; do
	[ -n "$lun" ] && [ -n "$target" ] || continue
	disk=$(find_disk "$lun") || { echo "lun $lun: disk not found"; continue; }
	dev="$disk"
	[ -e "$disk-part1" ] && dev="$disk-part1"
	if mountpoint -q "$target"; then
		echo "lun $lun: $target already mounted"
		continue
	fi
	mkdir -p "$target" && mount "$dev" "$target" && echo "lun $lun: $dev -> $target" || echo "lun $lun: mount $dev on $target failed"
done
echo "done $(date -Is)"
