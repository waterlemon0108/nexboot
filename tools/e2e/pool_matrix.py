#!/usr/bin/env python3
"""存储池：建池、加盘、换盘、读写缓存、销毁——全部对着真实块设备跑。

这是产品里唯一直接管理数据载体的功能面，此前只有手测。产品只把 lsblk 的
TYPE=disk 当候选盘，所以需要真的块设备；没有整块空闲物理盘时用内核自带的
scsi_debug 造几块（见 README 的「造测试盘」）。

    modprobe scsi_debug dev_size_mb=128 add_host=4 num_tgts=1 per_host_store=1

用完 `rmmod scsi_debug` 收走。脚本只碰自己建的池，绝不碰已有池。
"""
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import call, check, section, skip, ssh, summary  # noqa: E402

POOL = os.environ.get("ND_E2E_POOL", "e2epool")


def pools():
    return call("GET", "/api/pools")[1]["items"]


def find_pool(name):
    return next((p for p in pools() if p["Name"] == name), None)


def wait_task(prefix, timeout=180):
    """Pool operations answer 202 and finish in the background."""
    for _ in range(timeout):
        _, d = call("GET", "/api/tasks/history?limit=10")
        for item in (d if isinstance(d, list) else d.get("items", [])):
            if item["ID"].startswith(prefix):
                if item["Status"] in ("success", "failed"):
                    return item
                break
        time.sleep(1)
    return {}


def free_disks():
    return [d for d in call("GET", "/api/storage/disks")[1]["items"] if not d["InUse"]]


def cleanup():
    for name in (POOL, "e2emirror"):
        p = find_pool(name)
        if p:
            call("DELETE", f"/api/pools/{p['ID']}")
            wait_task("task-destroy_pool")
            time.sleep(2)
        ssh(f"zpool destroy {name} 2>/dev/null")


cleanup()

section("0 数据池与候选盘")
# 安装产品前就建好的数据池也必须列出，否则总览显示无存储，该池也无法管理。
data_pool = [p for p in pools() if p["Capacity"] > 0]
check("0z", "配置的数据池已登记且能读出容量", bool(data_pool),
      str([(p["Name"], p["Health"], p["Capacity"]) for p in pools()]))

disks = free_disks()
# 镜像 / raidz 要同尺寸的盘：混进一块大盘，zpool 会拒成 invalid vdev specification。
by_size = {}
for d in disks:
    by_size.setdefault(d.get("Size"), []).append(d)
same = max(by_size.values(), key=len, default=[])
if len(same) >= 4:
    disks = same + [d for d in disks if d not in same]
# 池自己的卷也是块设备，列为可用盘会导致覆盖其中的镜像或运行中的克隆。
zvols = [d for d in disks if d["Name"].startswith("zd")]
check("0a", "候选盘里没有池自己的卷（zvol）", not zvols, str([d["Name"] for d in zvols]))
if len(disks) < 4:
    skip("0b", "存储池全套用例", f"只有 {len(disks)} 块空闲盘，需要 4 块（modprobe scsi_debug dev_size_mb=128 add_host=4 num_tgts=1 per_host_store=1）")
    sys.exit(summary())
if len(same) < 4:
    skip("0b", "存储池全套用例", f"没有四块同尺寸的空闲盘（{[(d['Name'], d.get('Size')) for d in disks]}）")
    sys.exit(summary())
check("0b", "至少四块同尺寸空闲盘可用", True, " ".join(d["Name"] for d in disks[:4]))
# 每台最多一个数据池加一个备份池：用例自己建的池占的是备份池那个位子。
if len([p for p in pools() if p["Capacity"] > 0]) >= 2:
    skip("0c", "存储池全套用例", "这台数据池和备份池都已建，没有空位给测试池；先销毁备份池再跑")
    sys.exit(summary())
d1, d2, d3, d4 = (d["Path"] for d in disks[:4])

section("A 建池")
st, body = call("POST", "/api/pools", {"name": POOL, "disks": [d1]})
check("A1", "提交建池", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-create_pool")
check("A2", "建池任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
pool = find_pool(POOL)
check("A3", "池出现在列表里且健康", bool(pool) and str(pool.get("Health", "")).upper() == "ONLINE",
      str(pool)[:160] if pool else "not listed")
pid = pool["ID"] if pool else ""
check("A4", "zpool 里确实有这个池", POOL in ssh("zpool list -H -o name"), ssh("zpool list -H -o name").replace("\n", " "))
check("A5", "重名建池被拒", call("POST", "/api/pools", {"name": POOL, "disks": [d2]})[0] >= 400)
check("A6", "没选盘建池 400", call("POST", "/api/pools", {"name": "e2epool2", "disks": []})[0] == 400)
# 这块盘已经在池里，再次列出会破坏两个池。
check("A7", "已入池的盘不再是候选盘", d1 not in [d["Path"] for d in free_disks()],
      " ".join(d["Path"] for d in free_disks()))

section("B 加盘")
st, _ = call("POST", f"/api/pools/{pid}/disks", {"disks": [d2]})
check("B1", "提交加盘", st == 202, f"HTTP {st}")
task = wait_task("task-add_disk")
check("B2", "加盘任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
check("B3", "zpool 状态里出现第二块盘", d2.split("/")[-1] in ssh(f"zpool status {POOL}"),
      ssh(f"zpool status {POOL} | tail -8").replace("\n", " | ")[:200])

section("C 读缓存 L2ARC")
st, _ = call("POST", f"/api/pools/{pid}/read-cache", {"disks": [d3]})
check("C1", "提交加读缓存", st == 202, f"HTTP {st}")
task = wait_task("task-add_read_cache")
check("C2", "加读缓存成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
check("C3", "zpool 里出现 cache 段", "cache" in ssh(f"zpool status {POOL}"),
      ssh(f"zpool status {POOL} | tail -6").replace("\n", " | ")[:200])
st, _ = call("POST", f"/api/pools/{pid}/read-cache/remove", {"disk": d3})
check("C4", "提交删读缓存", st == 202, f"HTTP {st}")
task = wait_task("task-remove_read_cache")
check("C5", "删读缓存成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
check("C6", "cache 段已消失", "cache" not in ssh(f"zpool status {POOL}"))

section("D 写缓存 SLOG")
st, _ = call("POST", f"/api/pools/{pid}/write-cache", {"disks": [d3]})
check("D1", "提交加写缓存", st == 202, f"HTTP {st}")
task = wait_task("task-add_write_cache")
check("D2", "加写缓存成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
check("D3", "zpool 里出现 logs 段", "logs" in ssh(f"zpool status {POOL}"),
      ssh(f"zpool status {POOL} | tail -6").replace("\n", " | ")[:200])
st, _ = call("POST", f"/api/pools/{pid}/write-cache/flush")
task = wait_task("task-flush_cache")
check("D4", "清空写缓存", task.get("Status") == "success", (task.get("Error") or "")[:160])
st, _ = call("POST", f"/api/pools/{pid}/write-cache/remove", {"disk": d3})
task = wait_task("task-remove_write_cache")
check("D5", "删写缓存成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
check("D6", "logs 段已消失", "logs" not in ssh(f"zpool status {POOL}"))

section("E 换盘")
st, _ = call("POST", f"/api/pools/{pid}/disks/replace", {"old_disk": d2, "new_disk": d4})
check("E1", "提交换盘", st == 202, f"HTTP {st}")
task = wait_task("task-replace_disk", timeout=300)
check("E2", "换盘任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
# 任务返回后 resilver 还要片刻才完成，旧盘在此之前仍列在 replacing-N vdev 下，等几秒。
for _ in range(10):
    status = ssh(f"zpool status {POOL}")
    if d4.split("/")[-1] in status and d2.split("/")[-1] not in status:
        break
    time.sleep(1)
check("E3", "新盘顶替了旧盘", d4.split("/")[-1] in status and d2.split("/")[-1] not in status,
      status.replace("\n", " | ")[:220])

section("F 销毁")
st, _ = call("DELETE", f"/api/pools/{pid}")
check("F1", "提交销毁", st == 202, f"HTTP {st}")
task = wait_task("task-destroy_pool")
check("F2", "销毁任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
check("F3", "池不在列表里了", find_pool(POOL) is None)
check("F4", "zpool 里也没有了", POOL not in ssh("zpool list -H -o name"))
check("F5", "盘回到候选池", d1 in [d["Path"] for d in free_disks()],
      " ".join(d["Path"] for d in free_disks()))
check("F6", "销毁不存在的池 404", call("DELETE", "/api/pools/pool-ghost")[0] == 404)

# 布局：mirror 池按对加盘，attach 加一路、detach 减一路，布局从池本身读出；raidz 拒绝减盘。
# 使用 F 段释放的同四块 scsi_debug 盘。
section("G 布局：镜像与 raidz")
LPOOL = "e2emirror"
ssh(f"zpool destroy {LPOOL} 2>/dev/null")
st, body = call("POST", "/api/pools", {"name": LPOOL, "disks": [d1, d2], "layout": "mirror"})
check("G1", "建镜像池", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-create_pool")
check("G2", "建池任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
lp = find_pool(LPOOL)
lpid = lp["ID"] if lp else ""
check("G3", "布局从池上读出为 mirror 且有 mirror-0", bool(lp) and lp.get("Layout") == "mirror" and lp.get("GroupWidth") == 2
      and any(g.get("Name") == "mirror-0" for g in (lp.get("Groups") or [])), str(lp)[:200] if lp else "not listed")
check("G4", "zpool 里是 mirror-0", "mirror-0" in ssh(f"zpool status {LPOOL}"))
st, body = call("POST", f"/api/pools/{lpid}/disks", {"disks": [d3]})
check("G5", "镜像池单块加盘被拒并说清再选几块", st == 400 and "再选" in str(body), f"HTTP {st} {str(body)[:120]}")
st, body = call("POST", f"/api/pools/{lpid}/disks", {"disks": [d3], "mode": "attach", "target": d1})
check("G6", "给 d1 加镜像盘（两路变三路）", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-add_disk", timeout=300)
check("G7", "attach 任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
lp = find_pool(LPOOL)
grp = next((g for g in (lp.get("Groups") or []) if g.get("Name") == "mirror-0"), {}) if lp else {}
check("G8", "mirror-0 现在有三块盘", len(grp.get("Disks") or []) == 3, str(grp)[:200])
st, body = call("POST", f"/api/pools/{lpid}/disks/remove", {"disk": d3})
check("G9", "摘掉一路（detach）", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-detach_disk")
check("G10", "detach 任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
st, body = call("POST", f"/api/pools/{lpid}/disks/remove", {"disk": d2})
task = wait_task("task-detach_disk")
lp = find_pool(LPOOL)
check("G11", "再摘一路后只剩裸盘，布局回读为 stripe", bool(lp) and lp.get("Layout") == "stripe", str(lp)[:160] if lp else "not listed")
st, body = call("POST", f"/api/pools/{lpid}/mirror-upgrade", {"pairs": [{"target": d1, "disk": d2}]})
check("G12", "一键升级为镜像", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-mirror_upgrade", timeout=300)
check("G13", "升级任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
lp = find_pool(LPOOL)
check("G14", "布局回读为 mirror", bool(lp) and lp.get("Layout") == "mirror", str(lp)[:160] if lp else "not listed")

# special（元数据）vdev 只能以 mirror 方式添加；加 spare 会打开 autoreplace。两者都按各自角色读回。
st, body = call("POST", f"/api/pools/{lpid}/special", {"disks": [d3]})
check("G14a", "单块元数据盘被拒", st == 400 and "镜像" in str(body), f"HTTP {st} {str(body)[:120]}")
st, body = call("POST", f"/api/pools/{lpid}/special", {"disks": [d3, d4]})
check("G14b", "加镜像元数据盘", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-add_special")
check("G14c", "加元数据盘任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
lp = find_pool(LPOOL)
sp = [g for g in (lp.get("Groups") or []) if g.get("Role") == "special"] if lp else []
check("G14d", "special 组回读且为镜像", len(sp) == 1 and sp[0].get("Kind") == "mirror" and "special" in ssh(f"zpool status {LPOOL}"), str(sp)[:200])
st, body = call("POST", f"/api/pools/{lpid}/special/remove", {"disk": sp[0]["Name"] if sp else "mirror-x"})
check("G14e", "整组移除元数据盘", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-remove_special", timeout=300)
check("G14f", "移除元数据盘任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
st, body = call("POST", f"/api/pools/{lpid}/spares", {"disks": [d3]})
check("G14g", "加热备盘", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-add_spare")
check("G14h", "热备盘任务成功且 autoreplace=on", task.get("Status") == "success" and "on" in ssh(f"zpool get -H -o value autoreplace {LPOOL}"),
      (task.get("Error") or "")[:160] + " autoreplace=" + ssh(f"zpool get -H -o value autoreplace {LPOOL}").strip())
lp = find_pool(LPOOL)
check("G14i", "热备盘按 spare 角色回读", any(g.get("Role") == "spare" for g in (lp.get("Groups") or [])) if lp else False, str(lp.get("Groups"))[:200] if lp else "")
st, body = call("POST", f"/api/pools/{lpid}/spares/remove", {"disk": d3})
task = wait_task("task-remove_spare")
check("G14j", "移除热备盘", st == 202 and task.get("Status") == "success", f"HTTP {st} {(task.get('Error') or '')[:120]}")
call("DELETE", f"/api/pools/{lpid}")
wait_task("task-destroy_pool")

st, body = call("POST", "/api/pools", {"name": LPOOL, "disks": [d1, d2, d3], "layout": "raidz2"})
check("G15", "raidz2 三块盘被拒并说清再选几块", st == 400 and "再选" in str(body), f"HTTP {st} {str(body)[:120]}")
st, body = call("POST", "/api/pools", {"name": LPOOL, "disks": [d1, d2, d3, d4], "layout": "raidz2"})
check("G16", "建 raidz2 池", st == 202, f"HTTP {st} {str(body)[:120]}")
task = wait_task("task-create_pool")
check("G17", "建池任务成功", task.get("Status") == "success", (task.get("Error") or "")[:160])
lp = find_pool(LPOOL)
lpid = lp["ID"] if lp else ""
check("G18", "布局回读为 raidz2", bool(lp) and lp.get("Layout") == "raidz2" and lp.get("GroupWidth") == 4, str(lp)[:160] if lp else "not listed")
st, body = call("POST", f"/api/pools/{lpid}/disks/remove", {"disk": d1})
check("G19", "raidz 移盘被拒并指向换盘", st == 409 and "换盘" in str(body), f"HTTP {st} {str(body)[:120]}")
st, body = call("POST", f"/api/pools/{lpid}/disks", {"disks": [d1], "mode": "attach", "target": d2})
check("G20", "raidz 加镜像盘被拒", st == 409, f"HTTP {st} {str(body)[:120]}")
# 只在节点能扩容时提供扩容，并由条目说明是哪种。
if lp and lp.get("RaidzExpandable"):
    check("G20a", "本机支持 raidz 扩容（仅记录）", True, "RaidzExpandable=true")
else:
    check("G20a", "本机不支持 raidz 扩容时说明原因", bool(lp and lp.get("RaidzExpandNote")), str(lp.get("RaidzExpandNote") if lp else ""))
call("DELETE", f"/api/pools/{lpid}")
wait_task("task-destroy_pool")
check("G21", "布局用例结束后池已清理", find_pool(LPOOL) is None and LPOOL not in ssh("zpool list -H -o name"))

sys.exit(summary())
