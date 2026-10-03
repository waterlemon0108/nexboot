#!/usr/bin/env python3
"""开机链路的功能矩阵：载体命名、烘焙、数据盘 LUN 与盘符、合并中断续跑。

需要一个能通过体检的真实 Windows 镜像源文件（zfs 流最快），路径由
ND_WINDOWS_SOURCE 指定；没有它体检会阻断建组，K/M 两段无法跑。

本脚本会创建并删除分组、终端与若干配置——不要对生产池运行。
"""
import os
import shlex
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import (IMPORT_DIR, POOL, SSH_HOST, call, check, configs, reductions,  # noqa: E402
                   section, skip, ssh, summary, task_of)

SOURCE = os.environ.get("ND_WINDOWS_SOURCE", f"{IMPORT_DIR}/isharedisk.gzip")
IMG = "e2e-win"
CFG = f"{IMG}_default"
MAC = "AA:BB:CC:11:22:33"
TERMINAL = "terminal-AABBCC112233"


def cleanup():
    call("DELETE", f"/api/terminals/{TERMINAL}")
    call("DELETE", "/api/terminals/terminal-AABBCC112244")
    for g in call("GET", "/api/groups")[1]["items"]:
        if g["Name"].startswith("e2e-"):
            call("DELETE", f"/api/groups/{g['ID']}")
    for img in (IMG, "e2e-data"):
        if call("GET", f"/api/images/{img}")[0] == 200:
            for c in configs(img):
                call("DELETE", f"/api/configs/{c['ID']}")
                time.sleep(2)
            call("DELETE", f"/api/images/{img}")
            time.sleep(3)


# 网段按 ND_SSH_HOST（被测节点）所在网段推：产品会硬拒不在客户机网卡网段内的分组，
# 写死会让后面每一段都失败，且错误指向别处。
SEG = ".".join((os.environ.get("ND_SSH_HOST") or SSH_HOST or "192.168.50.1").split(".")[:3])
GW = f"{SEG}.1"


def preflight():
    """前置不成立就整段跳过，别空跑。

    这个矩阵要一个能过导入体检的 Windows 镜像源；没有它，建组会被体检拦住，
    随后每一条断言都在报「开机失败」「超管保存失败」——指向别处，排查会走很远。
    一条明确的跳过比十条误导的失败有用。
    """
    if not SOURCE:
        skip("0", "开机链路全部检查", "未提供 ND_WINDOWS_SOURCE，没有可导入的镜像源")
        return False
    if not Path(SOURCE).name:
        skip("0", "开机链路全部检查", f"ND_WINDOWS_SOURCE 不是文件路径：{SOURCE}")
        return False
    out = ssh(f"test -f {shlex.quote(SOURCE)} && echo yes || echo no")
    if "yes" not in out:
        skip("0", "开机链路全部检查", f"{SSH_HOST} 上找不到镜像源 {SOURCE}")
        return False
    return True


if not preflight():
    sys.exit(summary())

cleanup()

section("A 导入与烘焙")
call("POST", "/api/images/import", {"name": IMG, "source_path": SOURCE, "os_type": "windows"})
task = task_of("task-import_image", timeout=900)
check("A1", "真实 Windows 镜像导入完成", task.get("Status") == "success", (task.get("Error") or "")[:160])
image = call("GET", f"/api/images/{IMG}")[1].get("image", {})
# 版本为空说明跳过了烘焙，每台客户机每次开机都要注入，这正是本检查要发现的问题。
check("A2", "启动脚本已烘焙进镜像（版本号非空）", len(str(image.get("MountScriptVersion", ""))) == 16,
      str(image.get("MountScriptVersion")) + " " + ssh("journalctl -u ndiskless --since '-10min' --no-pager | grep -i 'bake skipped' | tail -1"))
time.sleep(10)  # 先等体检的临时克隆释放

section("B 开机：克隆命名与导出")
st, group = call("POST", "/api/groups", {
    "name": "e2e-开机班", "start_ip": f"{SEG}.100", "client_max": 20,
    "gateway": GW, "netmask": "255.255.255.0", "dns1": GW,
    "system_image_id": IMG, "system_config_id": CFG,
    "system_reduction_id": reductions(CFG)[0]["ID"]})
check("B1", "建分组并绑定还原点（镜像体检通过）", st == 201, str(group)[:160])
gid = group["ID"] if st == 201 else ""
# 一开始就是离线：后面超管关机要等机器离线才动会话，而这里不会发心跳。
st, _ = call("POST", "/api/terminals", {"mac": MAC, "name": "一号机", "group_id": gid, "state": "offline"})
check("B2", "分组下建终端", st in (200, 201))

st, script = call("GET", f"/boot?mac={MAC}")
check("B3", "取到 iPXE 启动脚本", st == 200 and "sanboot" in str(script), str(script)[:110].replace("\n", " | "))
check("B4", "initiator IQN 与 target 同一权威", "iqn.2026-06.local.ndiskless:initiator-aabbcc112233" in str(script))
clones = ssh("zfs list -H -o name | grep CLIENT-")
check("B5", "开机克隆命名为 CLIENT-<MAC>", "CLIENT-AABBCC112233" in clones, clones.replace("\n", " | ")[:160])
check("B6", "脚本带失败回报", "imgfetch /boot/failed?mac=" in str(script) and "|| goto nd-boot-failed" in str(script))

st, _ = call("GET", f"/boot/failed?mac={MAC}&stage=nonsense")
check("B7", "未知阶段的回报被拒", st == 400, f"HTTP {st}")
st, _ = call("GET", f"/boot/failed?mac={MAC}&stage=sanhook&err=1058021379&platform=pcbios")
check("B8", "挂盘失败回报已记录", st == 200, f"HTTP {st}")


def boot_alarm(timeout=90):
    deadline = time.time() + timeout
    while time.time() < deadline:
        _, body = call("GET", "/api/alarms?status=open&size=200")
        for a in (body or {}).get("items", []):
            if a.get("Type") == "开机失败" and a.get("Resource") == "一号机":
                return a
        time.sleep(5)
    return {}


alarm = boot_alarm()
check("B9", "告警中心出现开机失败，写明连不上系统盘和错误码",
      "连不上系统盘" in alarm.get("Message", "") and "0x3f102003" in alarm.get("Message", ""), alarm.get("Message", "没有告警"))

section("C 数据盘：LUN 与盘符出自同一次遍历")
# 数据盘来自数据盘镜像：页面新建的空白盘（GPT + NTFS，稀疏），再分出一个配置给第二个盘符。
DATA = "e2e-data"
st, body = call("POST", "/api/images/blank", {"name": DATA, "size_bytes": 2 * 1024 ** 3, "filesystem": "ntfs"})
task = task_of("task-create_blank_image", timeout=120)
check("C0", "新建空数据盘镜像（2 GiB NTFS）", st == 202 and task.get("Status") == "success", f"HTTP {st} " + (task.get("Error") or "")[:160])
dimg = call("GET", f"/api/images/{DATA}")[1].get("image", {})
check("C0a", "数据盘镜像用途为 data、来源 blank、逻辑 2 GiB", dimg.get("Purpose") == "data" and dimg.get("Origin") == "blank" and dimg.get("Size") == 2 * 1024 ** 3, str(dimg)[:160])
part = ssh(f"lsblk -no NAME,FSTYPE,PARTTYPENAME,LABEL /dev/zvol/{POOL}/nd/{DATA} 2>/dev/null; sgdisk -p /dev/zvol/{POOL}/nd/{DATA} 2>/dev/null | tail -2")
check("C0b", "卷上是 GPT 单分区 + NTFS，卷标就是盘名", "ntfs" in part.lower() and "Microsoft basic data" in part and DATA in part, part.replace("\n", " | ")[:200])
# 系统镜像不能用作数据盘，必须明确拒绝。
st, body = call("POST", f"/api/groups/{gid}/disks", {"mount_target": "D:", "image_id": IMG, "config_id": CFG})
check("C0c", "系统盘镜像不能作数据盘", st == 400 and "系统盘镜像" in str(body), f"HTTP {st} {str(body)[:120]}")
call("POST", f"/api/images/{DATA}/configs", {"name": "e2e-data-b"})
time.sleep(3)
by_name = {c["Name"]: c for c in configs(DATA)}
for name, letter in (("default", "D:"), ("e2e-data-b", "F:")):
    st, _ = call("POST", f"/api/groups/{gid}/disks",
                 {"mount_target": letter, "image_id": DATA, "config_id": by_name[name]["ID"]})
    check(f"C1-{letter[0]}", f"挂 {letter} 数据盘", st in (200, 201))
call("GET", f"/boot?mac={MAC}")
clones = ssh("zfs list -H -o name | grep AABBCC112233")
check("C2", "系统盘与两块数据盘各自克隆", "DATA-1" in clones and "DATA-2" in clones, clones.replace("\n", " | "))
luns = ssh("ls /sys/kernel/config/target/iscsi/*client-aabbcc112233*/tpgt_1/lun/ 2>/dev/null")
check("C3", "导出 lun_0/1/2", all(f"lun_{n}" in luns for n in (0, 1, 2)), luns.replace("\n", " "))
st, letters = call("GET", f"/boot/data-disks?mac={MAC}")
items = letters.get("items") if isinstance(letters, dict) else letters
check("C4", "盘符按 LUN 对齐（D→1, F→2）",
      st == 200 and items == [{"lun": 1, "letter": "D"}, {"lun": 2, "letter": "F"}], str(items))
st, net = call("GET", f"/boot/net-config?mac={MAC}")
check("C5", "启动脚本可取回网关与 DNS", st == 200 and net.get("gateway") == GW, str(net)[:100])

section("S 超管机停机：数据盘存还原点并应用")
# 超管开机后直接往 D: 的持久克隆写标记，关机只把 D: 存为数据配置的还原点，分组下次普通开机从它克隆。
disks = call("GET", f"/api/groups/{gid}/disks")[1].get("items", [])
disk_d = next((d for d in disks if d["MountTarget"] == "D:"), {})
disk_f = next((d for d in disks if d["MountTarget"] == "F:"), {})
st, _ = call("POST", f"/api/terminals/{TERMINAL}/super")
check("S1", "终端设为超管机", st == 200, f"HTTP {st}")
call("GET", f"/boot?mac={MAC}")
sclones = ssh("zfs list -H -o name | grep SCLIENT-AABBCC112233")
check("S2", "超管开机得到持久克隆（系统盘 + DATA-1/2）", "SCLIENT-AABBCC112233-DATA-1" in sclones and "SCLIENT-AABBCC112233-DATA-2" in sclones, sclones.replace("\n", " | "))
MARK = "NDE2E-SUPER-DATA-MARK"
OFFSET = 1500 * 1024 * 1024
# /dev/zvol 链接可能迟到，对不存在的路径 dd 会在 devtmpfs 里建普通文件，读写自洽但真盘没写入，
# udev 之后也不会覆盖它。必须等路径成为符号链接再写。
DEV_D1 = f"/dev/zvol/{POOL}/run/SCLIENT-AABBCC112233-DATA-1"
ssh(f"find /dev/zvol -type f -delete 2>/dev/null; for i in $(seq 1 20); do test -L {DEV_D1} && break; udevadm settle; sleep 1; done")
check("S3a", "D: 超管克隆的设备链接已就位", ssh(f"test -L {DEV_D1} && echo ok") == "ok")
ssh(f"printf '{MARK}' | dd of={DEV_D1} bs=1 seek={OFFSET} conv=notrunc,fsync 2>/dev/null; sync")
seen = ssh(f"dd if={DEV_D1} bs=1 skip={OFFSET} count={len(MARK)} 2>/dev/null")
check("S3", "往超管机的 D: 会话写入标记", seen == MARK, seen)
# 只保存 D:，系统盘和 F: 丢弃。
st, body = call("POST", f"/api/terminals/{TERMINAL}/super/stop",
                {"reduction_name": "", "data_disks": [{"disk_id": disk_d.get("ID", ""), "reduction_name": "e2e-d1"}]})
task = task_of("task-super_stop", timeout=180) if st == 202 else {}
check("S4", "停机任务成功", st == 202 and task.get("Status") == "success", f"HTTP {st} {str(body)[:100]} " + (task.get("Error") or "")[:160])
dcfg = disk_d.get("ConfigID", "")
reds = reductions(dcfg) if dcfg else []
saved = next((r for r in reds if r.get("DisplayName") == "e2e-d1" or r.get("Name") == "@e2e-d1"), None)
cfg_row = next((c for c in configs(DATA) if c["ID"] == dcfg), {})
check("S5", "数据配置多了还原点 e2e-d1 且成为应用还原点", saved is not None and cfg_row.get("DefaultReductionID") == (saved or {}).get("ID"), f"reds={[r.get('Name') for r in reds]} applied={cfg_row.get('DefaultReductionID')}")
# 保存会把超管克隆改名为配置，/dev/zvol 链接由 udev 延迟跟随，所以读盘要重试几次。
def read_mark(dev, attempts=12):
    """rename/clone 之后 /dev/zvol 符号链接由 udev 懒建；双机高 churn 下更慢。"""
    got = ""
    for _ in range(attempts):
        got = ssh(f"udevadm settle 2>/dev/null; dd if={dev} bs=1 skip={OFFSET} count={len(MARK)} 2>/dev/null")
        if got == MARK:
            return got
        time.sleep(3)
    return got


seen = read_mark(f"/dev/zvol/{POOL}/nd/{dcfg}")
check("S6", "标记进了数据配置（超管克隆已顶替配置）", seen == MARK, seen)
left = ssh("zfs list -H -o name | grep SCLIENT-AABBCC112233")
check("S7", "未勾选的系统盘与 F: 会话已丢弃，SCLIENT-* 全部回收", left == "", left.replace("\n", " | "))
fcfg = disk_f.get("ConfigID", "")
check("S8", "F: 的数据配置没有多出还原点", len(reductions(fcfg)) == 1 if fcfg else False, str([r.get("Name") for r in reductions(fcfg)]) if fcfg else "no F:")
term = call("GET", f"/api/terminals/{TERMINAL}")[1]
check("S9", "存还原点后仍是超管机（结束编辑靠取消超管）", term.get("IsSuper") is True, str(term.get("IsSuper")))
# 取消超管后普通开机，D: 应克隆自刚存的还原点。
call("DELETE", f"/api/terminals/{TERMINAL}/super")
call("GET", f"/boot?mac={MAC}")
seen = read_mark(f"/dev/zvol/{POOL}/run/CLIENT-AABBCC112233-DATA-1")
check("S10", "普通开机的 D: 克隆自新还原点（能看到标记）", seen == MARK, seen)
# 同一数据配置不能被两台超管机占用：第二个分组用不同系统配置但同一个 D: 时应拒绝并点名占用者。
call("POST", f"/api/terminals/{TERMINAL}/super")
st, cfg_b = call("POST", f"/api/images/{IMG}/configs", {"name": "e2e-sysb"})
time.sleep(3)
cfg_b_id = next((c["ID"] for c in configs(IMG) if c["Name"] == "e2e-sysb"), "")
# 也要在客户机网段内，网段外的分组会被拒（见 N 段）。
st, group_b = call("POST", "/api/groups", {
    "name": "e2e-乙班", "start_ip": f"{SEG}.140", "client_max": 5,
    "gateway": GW, "netmask": "255.255.255.0", "dns1": GW,
    "system_image_id": IMG, "system_config_id": cfg_b_id})
check("S11a", "建第二个分组（同一数据盘、不同系统配置）", st == 201, f"HTTP {st} {str(group_b)[:160]}")
gid_b = group_b.get("ID", "") if st == 201 else ""
call("POST", f"/api/groups/{gid_b}/disks", {"mount_target": "D:", "image_id": DATA, "config_id": dcfg})
st, t_b = call("POST", "/api/terminals", {"mac": "AA:BB:CC:11:22:44", "name": "乙班机", "group_id": gid_b})
t_b = t_b if isinstance(t_b, dict) else {}
st, body = call("POST", f"/api/terminals/{t_b.get('ID', '')}/super")
check("S11", "另一组共用该数据配置的机器不能再设超管（点名占用者）", st == 409 and "AABBCC112233" in str(body) and dcfg in str(body), f"HTTP {st} {str(body)[:160]}")
call("DELETE", f"/api/terminals/{t_b.get('ID', '')}")
call("DELETE", f"/api/groups/{gid_b}")
call("POST", f"/api/terminals/{TERMINAL}/super/stop", {"reduction_name": ""})
task_of("task-super_stop", timeout=120)

section("N 客户机网络：网段校验与跨网段开关")
# 分组以服务器客户机网段为准：网段外的区间当场拒绝，并点名网卡、给出可用区间；
# 打开中继开关后放行；存在这种分组时开关不能关闭。
st, net = call("GET", "/api/network?client_max=30")
check("N1", "读到客户机网卡与网段", st == 200 and net.get("known") and net.get("client_iface") and net.get("client_networks"), str({k: net.get(k) for k in ("client_iface", "client_networks", "allow_cross_subnet", "known")}))
sug = net.get("suggest") or {}
check("N2", "建议网段在客户机网卡网段内", bool(sug.get("start_ip")) and any(sug.get("start_ip", "").rsplit(".", 1)[0] == p.split("/")[0].rsplit(".", 1)[0] for p in net.get("client_networks", [])), str(sug))
was_cross = bool(net.get("allow_cross_subnet"))
iface_setting = net.get("client_iface_setting", "")
if was_cross:
    call("PUT", "/api/network", {"client_iface": iface_setting, "allow_cross_subnet": False})
off = {"name": "e2e-外网段", "start_ip": "10.99.99.10", "client_max": 5, "gateway": "10.99.99.1", "netmask": "255.255.255.0",
       "system_image_id": IMG, "system_config_id": CFG}
st, body = call("POST", "/api/groups", off)
check("N3", "网段不在客户机网卡时拒绝并点名网卡、给出建议网段", st == 400 and net.get("client_iface", "?") in str(body) and "10.99.99.10" in str(body) and "跨网段" in str(body), f"HTTP {st} {str(body)[:200]}")
st, body = call("PUT", "/api/network", {"client_iface": iface_setting, "allow_cross_subnet": True})
check("N4", "打开跨网段分组", st == 200 and body.get("allow_cross_subnet") is True, f"HTTP {st} {str(body)[:120]}")
base = ssh("cat /etc/dnsmasq.d/00-ndiskless-base.conf 2>/dev/null; systemctl is-active dnsmasq")
check("N4a", "dnsmasq 基础配置改为全网卡监听且服务在跑", "except-interface=lo" in base and "active" in base, base.replace("\n", " | ")[:200])
st, g_off = call("POST", "/api/groups", off)
check("N5", "打开后同一分组可建", st == 201, f"HTTP {st} {str(g_off)[:120]}")
st, body = call("PUT", "/api/network", {"client_iface": iface_setting, "allow_cross_subnet": False})
check("N6", "有跨网段分组时关不掉，提示点名分组", st == 400 and "e2e-外网段" in str(body), f"HTTP {st} {str(body)[:160]}")
server_ip = net.get("client_networks", [f"{GW}/24"])[0].split("/")[0]
st, body = call("POST", "/api/network/probe", {"ip": server_ip})
check("N7", "探测服务器自己的地址：通", st == 200 and body.get("reachable") is True, f"HTTP {st} {str(body)[:120]}")
if isinstance(g_off, dict) and g_off.get("ID"):
    call("DELETE", f"/api/groups/{g_off['ID']}")
st, body = call("PUT", "/api/network", {"client_iface": iface_setting, "allow_cross_subnet": was_cross})
check("N8", "分组删掉后开关可关回", st == 200 and body.get("allow_cross_subnet") is was_cross, f"HTTP {st} {str(body)[:120]}")
base = ssh("cat /etc/dnsmasq.d/00-ndiskless-base.conf 2>/dev/null; systemctl is-active dnsmasq")
check("N8a", "dnsmasq 基础配置回到只监听客户机网卡", (f"interface={net.get('client_iface')}" in base) == (not was_cross) and "active" in base, base.replace("\n", " | ")[:200])
section("D 删除终端立即回收克隆")
call("DELETE", f"/api/terminals/{TERMINAL}")  # 按 ID 删，传 MAC 会静默 404
reclaimed = False
for _ in range(15):
    time.sleep(2)
    if not ssh("zfs list -H -o name | grep AABBCC112233"):
        reclaimed = True
        break
check("D1", "克隆与 target 在 30 秒内回收", reclaimed, ssh("zfs list -H -o name | grep CLIENT- | head -3"))

section("E 合并从中间态续跑")
# 先删分组及其数据盘：合并默认配置会销毁这些盘绑定的兄弟配置，产品会直接拒绝，这里就测不了了。
for g in call("GET", "/api/groups")[1]["items"]:
    if g["Name"].startswith("e2e-"):
        call("DELETE", f"/api/groups/{g['ID']}")
for c in configs(IMG):
    if c["ID"] != CFG:
        call("DELETE", f"/api/configs/{c['ID']}")
        time.sleep(2)
time.sleep(2)


def wait_backup_idle(timeout=300):
    """备份现在是整个 nd/ 容器一条 send 流（分钟级窗口），期间合并的 destroy 会
    被正确拒绝为「存储对象正被占用」。超管保存会自动触发一轮备份（S 段刚做过），
    所以合并用例开跑前先等它落地。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        d = call("GET", "/api/tasks")[1]
        tasks = d if isinstance(d, list) else d.get("items", [])
        busy = any(t.get("Type") == "backup_dataset" and t.get("Status") in ("pending", "running") for t in tasks)
        if not busy:
            return
        time.sleep(3)


wait_backup_idle()
# 只看本池目录，否则其它池上的备份副本和 @ndbackup-* 标记会混入拓扑比较。
topo = lambda: sorted(l for l in ssh(f"zfs list -H -o name -t all | grep '^{POOL}/nd/'").splitlines()
                      if (f"/{IMG}" in l or f"/{CFG}" in l) and "@ndbackup-" not in l and "@rep-" not in l)
baseline = topo()
# 应用 m1：当前点不是最新时合并会被拒（它会悄悄撤掉回滚）。
call("POST", f"/api/configs/{CFG}/reductions", {"name": "m1", "set_current": True})
time.sleep(3)
# 模拟崩溃残留：折叠的快照已删、promote 已完成，旧镜像数据集还在且装着合并后的数据，盲目重放合并会销毁它。
ssh(f"zfs destroy {POOL}/nd/{CFG}@m1; zfs destroy {POOL}/nd/{CFG}@0; zfs promote {POOL}/nd/{CFG}")


def merge_with_retry(attempts=5):
    """双机在位时合并的 destroy 可能撞上每 15s 一轮的复制 send（正被占用）；
    隔轮重试直到成功——这正是产品文案让操作者做的事。"""
    for _ in range(attempts):
        st, body = call("POST", f"/api/configs/{CFG}/merge")
        task = task_of("task-merge_config") if st == 202 else {}
        if task.get("Status") == "success":
            return st, body, task
        time.sleep(18)
    return st, body, task


st, body, task = merge_with_retry()
check("E1", "重试合并可从中间态恢复", task.get("Status") == "success",
      f"HTTP {st} {str(body)[:120]} " + (task.get("Error") or "")[:160])
after = topo()
check("E2", "恢复后拓扑与全新导入一致", after == baseline, " ".join(after))
st, body, task = merge_with_retry()
check("E3", "已完成的合并再跑一次是空操作", task.get("Status") == "success",
      f"HTTP {st} {str(body)[:120]} " + (task.get("Error") or "")[:160])

cleanup()
sys.exit(summary())
