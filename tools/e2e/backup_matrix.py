#!/usr/bin/env python3
"""本地备份的功能矩阵：整库目录一条流、增量断链自愈、DB 副本随流，对着真实 ZFS 跑。

背景：旧备份逐数据集记增量基准，`合并配置` / `超管保存` 的 promote 会让下一轮
增量必然失败且无退路；配置首轮不带 -i origin，在备份池是全量拷贝。本矩阵验证
重做后的行为（issue 040）：一轮 = 整个 nd/ 容器一条 send -R 流；基准从两侧池实
况读出；promote 打断增量时自动整库重建；SQLite 副本在流里。

前置：见 README.md；另需一个与数据池不同的备份池（ND_BACKUP_POOL，默认 tank2）
和一个能通过体检的真实 Windows 镜像源（ND_WINDOWS_SOURCE——超管保存场景要建组，
64M 假 raw 过不了体检闸门）。本脚本会清空备份池上的 ndiskless/ 子树、删除并重建
名为 probe-bk 的镜像。
"""
import os
import shlex
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import (IMPORT_DIR, POOL, call, cfg_by_name, check, configs,  # noqa: E402
                   reductions, section, skip, ssh, summary, task_of)

BACKUP_POOL = os.environ.get("ND_BACKUP_POOL", "tank2")
SOURCE = os.environ["ND_WINDOWS_SOURCE"]
IMG = "probe-bk"
CFG = f"{IMG}_default"
MAC = "AA:BB:CC:55:66:77"
TERMINAL = "terminal-AABBCC556677"
SRC_ROOT = f"{POOL}/nd"
DST_ROOT = f"{BACKUP_POOL}/ndiskless/nd"


def cleanup():
    call("DELETE", f"/api/terminals/{TERMINAL}")
    for g in call("GET", "/api/groups")[1]["items"]:
        if g["Name"].startswith("e2e-bk"):
            call("DELETE", f"/api/groups/{g['ID']}")
    if call("GET", f"/api/images/{IMG}")[0] == 200:
        for c in configs(IMG):
            call("DELETE", f"/api/configs/{c['ID']}")
            time.sleep(2)
        call("DELETE", f"/api/images/{IMG}")
        time.sleep(3)
    ssh(f"zfs destroy -r {BACKUP_POOL}/ndiskless 2>/dev/null")
    ssh(f"for s in $(zfs list -H -o name -t snapshot -d 1 {SRC_ROOT} 2>/dev/null | grep '@ndbackup-'); do zfs destroy -r $s; done")


def diff_detail(src, dst, must):
    """两侧不一致时，把差在哪说出来。

    只报「src=23 dst=23」等于什么都没说：数目相同、内容不同是最常见的情形，
    而收尾清理会把备份池删掉，事后再想查已经查不到了。
    """
    only_src = [r for r in src if r not in dst][:5]
    only_dst = [r for r in dst if r not in src][:5]
    parts = [f"src={len(src)} dst={len(dst)}"]
    if only_src:
        parts.append("仅源侧: " + ", ".join(only_src))
    if only_dst:
        parts.append("仅备份侧: " + ", ".join(only_dst))
    if must and not any(must in r for r in dst):
        parts.append(f"备份侧找不到 {must}")
    return " | ".join(parts)[:400]


def run_backup():
    """跑一轮备份并等它结束。超管保存会自动触发一轮（既有功能），手动请求
    撞上 409「备份正在进行中」时等在跑的那轮完成再重试。"""
    deadline = time.time() + 300
    while True:
        st, body = call("POST", "/api/backup/run")
        if st == 202:
            return task_of("task-backup_dataset", timeout=300)
        if st == 409 and time.time() < deadline:
            time.sleep(3)
            continue
        return {"Status": "not-started", "Error": f"HTTP {st} {body}"}


def inventories():
    """两侧 guid 清单，归一化到容器相对名。数据集按 name+origin（send/recv 不保留
    文件系统 guid），快照按 name+guid。

    两类标记要排除，都不是备份的内容：

      ndbackup-*  备份自己的基准，prune 在流之后跑，两侧差一轮是设计内的。
      rep-*       HA 复制的轮次标记，每 60 秒打一次。备份池装的是某一刻的流，
                  不可能有那之后新产生的轮次——留着它，比对就变成了在赌
                  「这一刻有没有跨过 60 秒」。单机没有复制、这一项恒空，所以
                  这个矩阵在单机上一直是绿的；三节点才暴露，且症状是失败在
                  B3/C3/D5/E2 之间飘移，看上去像备份时好时坏。
    """
    def side(root):
        out = ssh(f"zfs list -H -p -r -t all -o name,guid,origin {root} 2>/dev/null")
        rows = []
        for line in out.splitlines():
            parts = line.split("\t")
            if len(parts) != 3:
                continue
            name, guid, origin = parts
            rel = name[len(root):]
            if "@ndbackup-" in rel or "@rep-" in rel:
                continue
            origin_rel = origin.replace(root + "/", "§/") if origin != "-" else "-"
            if "@" in rel:
                rows.append(f"{rel}\t{guid}")
            else:
                rows.append(f"{rel}\t{origin_rel}")
        return sorted(rows)
    return side(SRC_ROOT), side(DST_ROOT)


def preflight():
    """前置不成立就整段跳过，别空跑。

    这个矩阵要两样东西：一个与数据池不同的备份池，和一个能过导入体检的镜像源。
    缺任何一样，后面每一条都在报「备份失败」「两侧不一致」——指向备份逻辑，
    而真正的原因在这里。一条明确的跳过比二十条误导的失败有用。
    """
    if not SOURCE:
        skip("0", "本地备份全部检查", "未提供 ND_WINDOWS_SOURCE，没有可导入的镜像源")
        return False
    if "yes" not in ssh(f"test -f {shlex.quote(SOURCE)} && echo yes || echo no"):
        skip("0", "本地备份全部检查", f"节点上找不到镜像源 {SOURCE}")
        return False
    if BACKUP_POOL == POOL:
        skip("0", "本地备份全部检查", f"备份池不能和数据池同名（都是 {POOL}）")
        return False
    if BACKUP_POOL not in ssh(f"zpool list -H -o name {shlex.quote(BACKUP_POOL)} 2>/dev/null"):
        skip("0", "本地备份全部检查", f"备份池 {BACKUP_POOL} 不存在，请先建好再跑")
        return False
    return True


if not preflight():
    sys.exit(summary())

cleanup()

section("0 准备：镜像 + 备份池配置")
call("POST", "/api/images/import", {"name": IMG, "source_path": SOURCE, "os_type": "windows"})
t = task_of("task-import_image", timeout=900)
check("0a", "测试镜像导入完成", t.get("Status") == "success", (t.get("Error") or "")[:120])
time.sleep(12)  # 体检的 INSPECT 克隆要先释放
st, _ = call("PUT", "/api/backup/config", {"backup_pool": BACKUP_POOL, "enabled": True, "schedule": "24h"})
check("0b", "备份池配置写入", st == 200)

section("A 全量：整库一条流 + DB 副本")
t = run_backup()
check("A1", "首轮备份成功", t.get("Status") == "success", (t.get("Error") or t.get("Result") or "")[:160])
check("A2", "任务结果标为全量", "全量" in (t.get("Result") or ""), t.get("Result") or "")
listing = ssh(f"zfs list -H -o name -r {DST_ROOT} 2>/dev/null")
check("A3", "备份池出现整个目录容器（镜像+配置+db）",
      f"{DST_ROOT}/{IMG}" in listing and f"{DST_ROOT}/{CFG}" in listing and f"{DST_ROOT}/db" in listing,
      listing.replace("\n", " | ")[:200])
origin = ssh(f"zfs get -H -o value origin {DST_ROOT}/{CFG}").strip()
check("A4", "备份池上配置仍是镜像的克隆（不再是全量拷贝）", origin == f"{DST_ROOT}/{IMG}@0", origin)
db_snap = ssh(f"zfs list -H -o name -t snapshot -d 1 {DST_ROOT}/db | tail -1").strip()
# 备份侧数据集由 recv -u 收入，默认不挂载，挂快照即可读到内容。
# 不要吞掉挂载错误，否则分不清是没备份进去还是挂载失败。
db_ok = ssh(f"D=$(mktemp -d); mount -t zfs -o ro {db_snap} $D 2>&1 && ls $D; umount $D 2>/dev/null; rmdir $D") if db_snap else ""
check("A5", "DB 副本在备份里（快照可挂载见 ndiskless.db）", "ndiskless.db" in db_ok, f"{db_snap}: {db_ok}".strip()[:160])
check("A6", "副本不抢占源的挂载点（mountpoint 未随流）",
      ssh(f"zfs get -H -o value mountpoint {DST_ROOT}/db").strip() != ssh(f"zfs get -H -o value mountpoint {SRC_ROOT}/db").strip(),
      ssh(f"zfs get -H -o value mountpoint {DST_ROOT}/db").strip())
src, dst = inventories()
check("A7", "两侧 guid 清单一致", src == dst, f"src={len(src)} dst={len(dst)}")

section("B 还原点与派生：普通增量")
# 应用 r1：当前点不是最新时 C 段的合并会被拒。
call("POST", f"/api/configs/{CFG}/reductions", {"name": "r1", "set_current": True}); time.sleep(3)
call("POST", f"/api/configs/{CFG}/fork", {"name": "bk-fork", "reduction_id": f"{IMG}_0"}); time.sleep(3)
t = run_backup()
check("B1", "还原点+派生后备份成功", t.get("Status") == "success", (t.get("Error") or "")[:160])
check("B2", "走的是增量", "增量" in (t.get("Result") or ""), t.get("Result") or "")
src, dst = inventories()
check("B3", "两侧一致（含新还原点与 fork）", src == dst and any("@r1" in r for r in dst),
      diff_detail(src, dst, "@r1"))

section("C 合并配置（promote）：旧实现从此断链，新实现自愈")
fork_id = cfg_by_name(IMG, "bk-fork")["ID"]
st, _ = call("POST", f"/api/configs/{CFG}/merge", {"delete_config_ids": [fork_id]})
t = task_of("task-merge_config") if st == 202 else {}
check("C1", "合并配置成功", st == 202 and t.get("Status") == "success", f"HTTP {st} " + (t.get("Error") or "")[:120])
t = run_backup()
check("C2", "合并后的备份成功（增量或整库重建都算，不许失败）", t.get("Status") == "success",
      (t.get("Error") or "")[:200])
check("C2r", "任务结果说明了走的路径", bool(t.get("Result")), t.get("Result") or "")
src, dst = inventories()
check("C3", "两侧一致（合并后的目录）", src == dst, f"src={len(src)} dst={len(dst)}")

section("D 超管保存（promote+rename）：会话中备份 + 保存后备份")
# 网段按 ND_SSH_HOST 所在网段推：产品会硬拒不在客户机网卡网段内的分组，写死会让后续超管用例全部 404。
_seg = ".".join((os.environ.get("ND_SSH_HOST") or "192.168.10.3").split(".")[:3])
st, group = call("POST", "/api/groups", {
    "name": "e2e-bk班", "start_ip": f"{_seg}.140", "client_max": 5,
    "gateway": "", "netmask": "255.255.255.0",
    "system_image_id": IMG, "system_config_id": CFG,
    "system_reduction_id": reductions(CFG)[0]["ID"]})
gid = group.get("ID", "") if st == 201 else ""
call("POST", "/api/terminals", {"mac": MAC, "name": "备份验证机", "group_id": gid, "state": "offline"})
call("POST", f"/api/terminals/{TERMINAL}/super")
st, script = call("GET", f"/boot?mac={MAC}")
check("D1", "超管开机（SCLIENT 在 run/ 下，不进备份流）", st == 200 and "sanboot" in str(script),
      ssh("zfs list -H -o name | grep SCLIENT-AABBCC556677 || true").replace("\n", " | "))
t = run_backup()
check("D2", "超管会话中的备份成功", t.get("Status") == "success", (t.get("Error") or "")[:160])
st, _ = call("POST", f"/api/terminals/{TERMINAL}/super/stop", {"reduction_name": "bk-super"})
t = task_of("task-super_stop", timeout=180) if st == 202 else {}
check("D3", "超管保存成功", st == 202 and t.get("Status") == "success", f"HTTP {st} " + (t.get("Error") or "")[:120])
t = run_backup()
check("D4", "保存后的备份成功（旧实现在此必然断链）", t.get("Status") == "success", (t.get("Error") or "")[:200])
src, dst = inventories()
check("D5", "两侧一致（含超管固化的新还原点）", src == dst and any("bk-super" in r for r in dst),
      diff_detail(src, dst, "bk-super"))

section("E 基准丢失：自动退回全量")
ssh(f"for s in $(zfs list -H -o name -t snapshot -d 1 {SRC_ROOT} | grep '@ndbackup-'); do zfs destroy -r $s; done")
t = run_backup()
check("E1", "发送端基准全删后备份仍成功", t.get("Status") == "success", (t.get("Error") or "")[:160])
src, dst = inventories()
# 和 D5 一样用 diff_detail 列出缺了哪些：收尾会删掉备份池，事后无法再查。
check("E2", "两侧一致", src == dst, diff_detail(src, dst, "@0"))

section("F 收尾")
markers = ssh(f"zfs list -H -o name -t snapshot -d 1 {SRC_ROOT} | grep -c '@ndbackup-' || true").strip()
check("F1", "发送端 ndbackup-* 有上限（≤3）", markers.isdigit() and int(markers) <= 3, markers)
# 状态接口分 {config, node, task}：config 是计划，node 是本机这一份，task 是最近一轮。
# 验证本机确实备到了配置指定的池，且刚跑过一轮。
status = call("GET", "/api/backup/status")[1]
node = status.get("node") or {}
cfg_ = status.get("config") or {}
check("F2", "备份状态指向本机的备份池且刚跑过一轮",
      node.get("backup_pool") == BACKUP_POOL and bool(node.get("last_backup_at")) and bool(cfg_.get("last_run_at")),
      f"node={node} config.last_run_at={cfg_.get('last_run_at')}"[:200])

cleanup()
sys.exit(summary())
