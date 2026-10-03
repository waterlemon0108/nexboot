#!/usr/bin/env python3
"""超管机矩阵：开关超管的关机护栏、取消即丢弃、数据盘在线发布。

对着真实 ZFS/LIO 跑。前置：ND_BASE 指向 VIP（集群）或单机地址，SSH 能连到写入者。

用户自己的超管机占着默认配置（每个配置同时只能有一台超管机），所以本脚本自带
一套临时资产：从默认配置分出 e2e-super 配置、建同名分组和一台 0E2EC1 开头的终端，
结束时按终端 → 分组 → 配置的顺序删掉，并核对池里没有残留。
"""
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import call, check, configs, section, skip, summary  # noqa: E402
from ndcluster import ssh_out  # noqa: E402

MAC = "0E2EC1000B01"
PREFIX = "e2e-super"
# 每轮用独立名字：上一轮崩在交换中途会留下 <配置>_before_super 孤儿，固定名字会误用它导致还原点重名。
STAMP = time.strftime("%H%M%S")
NAME = f"{PREFIX}-{STAMP}"
DATA_NAME = f"{PREFIX}-data-{STAMP}"


def wait(cond, seconds=120, step=3):
    for _ in range(max(1, seconds // step)):
        got = cond()
        if got:
            return got
        time.sleep(step)
    return None


def writer_and_pool():
    """写入者就是此刻接住 VIP 的那台——它自己报 is_self。池名各节点不同，得问它。"""
    st, view = call("GET", "/api/cluster/pools")
    if st == 200 and isinstance(view, dict):
        for node in view.get("nodes", []):
            if node.get("is_self"):
                pools = node.get("pools") or []
                data = next((p for p in pools if p.get("Role") == "data"), pools[0] if pools else None)
                return node.get("ip"), (data or {}).get("Name")
    # 单机装法没有集群视图：退回本机池表
    st, res = call("GET", "/api/pools")
    items = (res or {}).get("items", []) if isinstance(res, dict) else []
    data = next((p for p in items if p.get("Role") == "data"), items[0] if items else None)
    return os.environ.get("ND_SSH_HOST") or "", (data or {}).get("Name")


HOST, POOL = writer_and_pool()
if not HOST or not POOL:
    print("找不到写入者或数据池，停")
    sys.exit(2)
SYS_CLONE = f"{POOL}/run/SCLIENT-{MAC}"
DATA_CLONE = f"{POOL}/run/SCLIENT-{MAC}-DATA-1"
TID = f"terminal-{MAC}"


def zfs(cmd, timeout=90):
    got, out = ssh_out(HOST, cmd, timeout=timeout)
    assert got, f"读不到 {HOST}：{out}"
    return out.strip()


def zfs_try(cmd, timeout=90):
    """建场/清场用：命令失败不把整个套件带走（比如盘正导出着，destroy 必然失败）。"""
    _, out = ssh_out(HOST, cmd, timeout=timeout)
    return out.strip()


def exists(dataset):
    return zfs(f"zfs list -H -o name {dataset} 2>/dev/null || true") == dataset


def fake_clone(dataset, config_id, newest=False):
    """造一块「这台机器开过机」的超管盘：真 ZFS 克隆，和开机时建的是同一种东西。

    真要靠客户机开机才能得到它，而 e2e 环境里没有客户机——但发布这条链路要验的是
    服务端这半边：替换、重新克隆、还原点。

    newest：从配置最新的还原点克隆。配置上已经存过点时要这样——从更早的点克隆，
    保存会因为「配置上有比超管盘更新的还原点」被拒。"""
    pick = "tail -1" if newest else "head -1"
    snap = zfs(f"zfs list -H -t snapshot -s creation -o name -r {POOL}/nd/{config_id} | grep -v '@rep-' | {pick}")
    if not snap:
        return ""
    zfs_try(f"zfs destroy -r {dataset}")
    zfs_try(f"zfs clone {snap} {dataset}")
    # 刚建出来的 zvol 会被 udev 扫一下，等它安静下来再交给产品去动它
    zfs_try("udevadm settle --timeout=10")
    time.sleep(2)
    return snap


def put_terminal(is_super, state, group_id, ip):
    return call("PUT", f"/api/terminals/{TID}",
                {"mac": MAC, "ip": ip, "group_id": group_id, "name": NAME,
                 "is_super": is_super, "state": state})


section("0 前置：临时配置、分组、终端")


def cleanup_leftovers():
    """上一轮崩在半路会留下同名资产，名字冲突会让这一轮从第一步就红。"""
    call("DELETE", f"/api/terminals/{TID}")
    st, gs = call("GET", "/api/groups")
    for g in (gs or {}).get("items", []):
        if g.get("Name", "").startswith(PREFIX):
            st, disks = call("GET", f"/api/groups/{g['ID']}/disks")
            for d in (disks or {}).get("items", []):
                call("DELETE", f"/api/group-disks/{d['ID']}")
            call("DELETE", f"/api/groups/{g['ID']}")
    st, imgs = call("GET", "/api/images")
    for img in (imgs or {}).get("items", []):
        for c in configs(img["ID"]):
            # 孤儿的名字是 <原名>_before_super，所以按"包含"清，不按等于清
            if PREFIX in c["Name"]:
                call("DELETE", f"/api/configs/{c['ID']}")
    time.sleep(3)


cleanup_leftovers()
st, gs = call("GET", "/api/groups")
tmpl = next((g for g in gs["items"] if g.get("SystemConfigID")), None)
if not tmpl:
    print("没有可参照的分组，停")
    sys.exit(2)
IMG, SRC_CFG = tmpl["SystemImageID"], tmpl["SystemConfigID"]
# 分配置必须指明从哪个还原点分，而模板配置不一定设了当前点：取它最新的那个。
st, reds = call("GET", f"/api/configs/{SRC_CFG}/reductions")
base_reds = (reds or {}).get("items", []) if isinstance(reds, dict) else []
if not base_reds:
    print(f"模板配置 {SRC_CFG} 没有还原点，无从分起，停")
    sys.exit(2)
st, _ = call("POST", f"/api/configs/{SRC_CFG}/fork", {"name": NAME, "reduction_id": base_reds[-1]["ID"]})
cfg = wait(lambda: next((c for c in configs(IMG) if c["Name"] == NAME), None))
check("0a", "临时系统配置已就绪", bool(cfg), f"HTTP {st}")
if not cfg:
    sys.exit(summary())

# 数据盘要的是「用途=数据盘」的镜像，系统镜像挂不上（400 数据盘的镜像与配置不匹配）
st, imgs = call("GET", "/api/images")
DATA_IMG = next((i for i in (imgs or {}).get("items", []) if i.get("Purpose") == "data"), None)
dcfg = None
if DATA_IMG:
    dsrc = configs(DATA_IMG["ID"])
    st, dreds = call("GET", f"/api/configs/{dsrc[0]['ID']}/reductions") if dsrc else (404, {})
    dred = (dreds or {}).get("items", []) if isinstance(dreds, dict) else []
    if dred:
        st, _ = call("POST", f"/api/configs/{dsrc[0]['ID']}/fork", {"name": DATA_NAME, "reduction_id": dred[-1]["ID"]})
        dcfg = wait(lambda: next((c for c in configs(DATA_IMG["ID"]) if c["Name"] == DATA_NAME), None))
check("0b", "临时数据盘配置已就绪", bool(dcfg), "" if DATA_IMG else "没有用途为数据盘的镜像")

head = ".".join(tmpl["StartIP"].split(".")[:3])
st, group = call("POST", "/api/groups", {
    "name": NAME, "start_ip": f"{head}.190", "client_max": 2,
    "gateway": tmpl.get("Gateway", ""), "netmask": tmpl.get("Netmask", "255.255.255.0"),
    "dns1": tmpl.get("DNS1", ""), "dns2": tmpl.get("DNS2", ""),
    "system_image_id": IMG, "system_config_id": cfg["ID"]})
check("0c", "临时分组已建", st in (200, 201), f"HTTP {st} {str(group)[:90]}")
GID = (group or {}).get("ID", "")
DISK_ID = ""
if GID and dcfg:
    st, disk = call("POST", f"/api/groups/{GID}/disks",
                    {"image_id": DATA_IMG["ID"], "config_id": dcfg["ID"], "mount_target": "D:"})
    DISK_ID = disk.get("ID", "") if isinstance(disk, dict) else ""
    check("0d", "临时数据盘已挂到分组", bool(DISK_ID), f"HTTP {st} {str(disk)[:90]}")
st, term = call("POST", "/api/terminals", {"mac": MAC, "ip": "", "group_id": GID, "name": NAME})
IP = term.get("IP", "") if isinstance(term, dict) else ""
check("0e", "临时终端已建", st in (200, 201), f"HTTP {st} {str(term)[:90]}")

try:
    section("1 开关超管要先关机")
    put_terminal(False, "online", GID, IP)
    st, r = call("POST", f"/api/terminals/{TID}/super")
    check("1a", "在线时设为超管被拒", st == 409 and "关机" in str(r), f"HTTP {st} {str(r)[:80]}")
    st, r = put_terminal(True, "online", GID, IP)
    check("1b", "在线时经编辑表单设为超管被拒", st == 409, f"HTTP {st} {str(r)[:80]}")

    section("2 设为超管先清遗留盘，取消超管即丢弃")
    put_terminal(False, "offline", GID, IP)
    fake_clone(SYS_CLONE, cfg["ID"])
    check("2a", "造出一块遗留的超管盘", exists(SYS_CLONE))
    st, r = call("POST", f"/api/terminals/{TID}/super")
    check("2b", "离线时设为超管成功", st == 200, f"HTTP {st} {str(r)[:80]}")
    check("2c", "遗留的超管盘已被清掉", not exists(SYS_CLONE))

    fake_clone(SYS_CLONE, cfg["ID"])
    put_terminal(True, "online", GID, IP)
    st, r = call("DELETE", f"/api/terminals/{TID}/super")
    check("2d", "在线时取消超管被拒", st == 409 and "关机" in str(r), f"HTTP {st} {str(r)[:80]}")
    check("2e", "被拒时超管盘原样保留", exists(SYS_CLONE))
    put_terminal(True, "offline", GID, IP)
    st, r = call("DELETE", f"/api/terminals/{TID}/super")
    check("2f", "离线时取消超管成功", st == 200, f"HTTP {st} {str(r)[:80]}")
    check("2g", "取消超管把盘一并丢弃", not exists(SYS_CLONE))

    section("2.5 关机存还原点不结束超管")
    call("POST", f"/api/terminals/{TID}/super")
    fake_clone(SYS_CLONE, cfg["ID"])
    put_terminal(True, "offline", GID, IP)
    st, r = call("POST", f"/api/terminals/{TID}/super/stop", {"reduction_name": "e2e-cun-1"})
    saved = wait(lambda: next((x for x in call("GET", f"/api/configs/{cfg['ID']}/reductions")[1].get("items", [])
                               if (x.get("DisplayName") or x.get("Name", "")).endswith("e2e-cun-1")), None), 120)
    check("2h", "关机存还原点成功", st in (200, 202) and bool(saved), f"HTTP {st} {str(r)[:70]}")
    st, after = call("GET", f"/api/terminals/{TID}")
    check("2i", "存完之后这台还是超管机", isinstance(after, dict) and after.get("IsSuper") is True,
          f"IsSuper={(after or {}).get('IsSuper')}")

    section("2.6 连点两次「关机存还原点」：第二次当场拒绝，配置不丢")
    # 两次保存共用同一个暂存名，交错会把配置改丢（库里有、池里没有，客户机开不了机）。
    # 机器在线时第一次保存会等它关机并一直占着镜像，第二次正好撞上。
    fake_clone(SYS_CLONE, cfg["ID"], newest=True)
    put_terminal(True, "online", GID, IP)
    st1, r1 = call("POST", f"/api/terminals/{TID}/super/stop", {"reduction_name": "e2e-cun-2"})
    st2, r2 = call("POST", f"/api/terminals/{TID}/super/stop", {"reduction_name": "e2e-cun-3"})
    check("2j", "第一次保存已受理", st1 in (200, 202), f"HTTP {st1} {str(r1)[:80]}")
    if st2 == 409:
        check("2k", "第二次当场拒绝，并说出在忙什么", "关机存还原点" in str(r2) and "请等该任务完成" in str(r2), str(r2)[:120])
    else:
        skip("2k", "第二次当场拒绝", f"第一次在第二次到达前就做完了（第二次 HTTP {st2}）")
    put_terminal(True, "offline", GID, IP)
    saved2 = wait(lambda: next((x for x in call("GET", f"/api/configs/{cfg['ID']}/reductions")[1].get("items", [])
                                if (x.get("DisplayName") or x.get("Name", "")).endswith("e2e-cun-2")), None), 180)
    check("2l", "关机后第一次保存完成", bool(saved2))
    names = [x.get("DisplayName") or x.get("Name", "") for x in call("GET", f"/api/configs/{cfg['ID']}/reductions")[1].get("items", [])]
    check("2m", "被拒的那次没有留下还原点", st2 != 409 or not any(n.endswith("e2e-cun-3") for n in names), str(names)[-120:])
    aside = f"{POOL}/nd/{cfg['ID']}_before_super"
    check("2n", "配置还在它的名字上，没有留下暂存副本",
          exists(f"{POOL}/nd/{cfg['ID']}") and not exists(aside),
          zfs(f"zfs list -H -o name -r {POOL}/nd | grep -- '{cfg['ID']}' || true").replace("\n", " | ")[:160])

    section("3 超管编辑期间不许直接给同一配置建还原点")
    call("POST", f"/api/terminals/{TID}/super")
    st, r = call("POST", f"/api/configs/{cfg['ID']}/reductions", {"name": "e2e-manual"})
    check("3a", "系统盘配置被挡住", st == 409 and "超管" in str(r), f"HTTP {st} {str(r)[:90]}")
    st, r = call("POST", f"/api/configs/{dcfg['ID']}/reductions", {"name": "e2e-manual"})
    check("3b", "同组数据盘配置也被挡住", st == 409, f"HTTP {st} {str(r)[:90]}")

    section("4 数据盘在线发布")
    base_snap = fake_clone(DATA_CLONE, dcfg["ID"])
    check("4a", "造出一块数据盘超管盘", exists(DATA_CLONE), base_snap)
    put_terminal(True, "online", GID, IP)  # 发布就是在机器开着的时候做的

    st, res = call("GET", f"/api/terminals/{TID}/super/disks")
    items = (res or {}).get("items", []) if isinstance(res, dict) else []
    mine = next((i for i in items if i.get("disk_id") == DISK_ID), None)
    check("4b", "数据盘列表报得出这块盘可发布", bool(mine) and mine.get("ready") is True, str(mine)[:110])

    st, r = call("POST", f"/api/terminals/{TID}/super/publish", {"disk_id": DISK_ID, "name": "e2e-fabu-1"})
    check("4c", "发布已受理", st in (200, 202), f"HTTP {st} {str(r)[:90]}")
    # 首次发布要做完整保存（快照—提升—改名—重新克隆），可能超过两分钟，不能用默认超时
    red = wait(lambda: next((x for x in call("GET", f"/api/configs/{dcfg['ID']}/reductions")[1].get("items", [])
                             if x.get("DisplayName") == "e2e-fabu-1" or x.get("Name") == "@e2e-fabu-1"), None), 300)
    check("4d", "发布点已生成", bool(red), str(red)[:90])
    after = next((c for c in configs(DATA_IMG["ID"]) if c["ID"] == dcfg["ID"]), {})
    check("4e", "发布点成为当前点", bool(red) and after.get("DefaultReductionID") == red["ID"],
          str(after.get("DefaultReductionID"))[:70])
    check("4f", "超管盘重新基于刚发布的点", exists(DATA_CLONE) and
          zfs(f"zfs get -H -o value origin {DATA_CLONE}").endswith(red["Name"].lstrip("@")) if red else False,
          zfs(f"zfs get -H -o value origin {DATA_CLONE}") if exists(DATA_CLONE) else "盘没了")
    check("4g", "LUN 已装回机器", "lun_1" in zfs(
        f"ls /sys/kernel/config/target/iscsi/*client-{MAC.lower()}*/tpgt_1/lun 2>/dev/null || true"))

    st, r = call("POST", f"/api/terminals/{TID}/super/publish", {"disk_id": DISK_ID, "name": "e2e-fabu-2"})
    red2 = wait(lambda: next((x for x in call("GET", f"/api/configs/{dcfg['ID']}/reductions")[1].get("items", [])
                              if x.get("DisplayName") == "e2e-fabu-2"), None))
    check("4h", "可以接着发第二次", st in (200, 202) and bool(red2), f"HTTP {st}")

    section("5 发布的两道护栏")

    def last_publish_task():
        """进行中的也要看：任务没跑完时不在历史里，只看历史会把「还在跑」当成
        「没发生」。"""
        for path in ("/api/tasks", "/api/tasks/history?limit=5"):
            st, h = call("GET", path)
            items = h if isinstance(h, list) else (h or {}).get("items", [])
            for t in items:
                if t.get("Type") == "publish_data_disk":
                    return t
        return None

    # 一：空闲检查只在客户机在线时做，这里没有真 iSCSI 会话测不到，由单元测试
    # TestPublishDataDiskRefusesWhileTheDiskIsStillBeingWritten 覆盖。这里测发布失败时盘要完好回到机器上。
    skip("5a", "盘还在写时拒绝发布", "本环境的客户机不会真在线，空闲检查按设计跳过")
    zfs_try(f"(dd if=/dev/urandom of=/dev/zvol/{DATA_CLONE} bs=1M count=32 seek=64 oflag=direct status=none &) ; sleep 1", timeout=30)
    st, r = call("POST", f"/api/terminals/{TID}/super/publish", {"disk_id": DISK_ID, "name": "e2e-fabu-busy"})
    done = wait(lambda: (lambda t: t if t and t.get("Status") in ("success", "failed") else None)(last_publish_task()), 90)
    check("5b", "写入中发布：要么成、要么干净地失败，盘都得在机器上",
          bool(done) and exists(DATA_CLONE), f"任务={(done or {}).get('Status')} " + ((done or {}).get("Error") or "")[:80])
    time.sleep(8)

    # 二：配置上有比超管盘更新的还原点——保存会把它删掉，必须拒绝并指名
    origin = zfs_try(f"zfs get -H -o value origin {DATA_CLONE}")
    made = zfs_try(f"zfs snapshot {POOL}/nd/{dcfg['ID']}@e2e-newer && echo made")
    snaps = zfs_try(f"zfs list -H -t snapshot -o name -r {POOL}/nd/{dcfg['ID']} | tr '\n' ' '")
    check("5c", "造出一个比超管盘更新的还原点", "made" in made and "e2e-newer" in snaps,
          f"超管盘基线={origin.split('@')[-1]} 配置快照={snaps[-120:]}")
    st, r = call("POST", f"/api/terminals/{TID}/super/publish", {"disk_id": DISK_ID, "name": "e2e-fabu-3"})
    blocked = wait(lambda: (lambda t: t if t and t.get("Status") == "failed" and "e2e-newer" in (t.get("Error") or "") else None)(last_publish_task()), 90)
    check("5d", "配置上有更新的还原点时拒绝，并指名是哪个", st == 409 or bool(blocked),
          f"HTTP {st} 任务={(last_publish_task() or {}).get('Status')} " + ((last_publish_task() or {}).get("Error") or "")[:100])
    check("5e", "被拒之后数据盘仍在机器上", exists(DATA_CLONE))
    zfs_try(f"zfs destroy {POOL}/nd/{dcfg['ID']}@e2e-newer")
finally:
    section("6 清理")
    put_terminal(True, "offline", GID, IP)
    call("DELETE", f"/api/terminals/{TID}/super")
    s1, _ = call("DELETE", f"/api/terminals/{TID}")
    if DISK_ID:
        call("DELETE", f"/api/group-disks/{DISK_ID}")
    s2, _ = call("DELETE", f"/api/groups/{GID}")
    # 复制每分钟 send 一遍目录，删配置可能撞上正在发送的快照；产品只重试十秒，这里多试几次。
    for _ in range(4):
        st, imgs = call("GET", "/api/images")
        left = [c for img in (imgs or {}).get("items", []) for c in configs(img["ID"]) if PREFIX in c["Name"]]
        if not left:
            break
        for c in left:  # 半途失败留下的 <配置>_before_super 也在内
            call("DELETE", f"/api/configs/{c['ID']}")
        time.sleep(20)
    gone = wait(lambda: not any(PREFIX in x["Name"] for img in ((imgs or {}).get("items", []) or [{"ID": IMG}])
                                for x in configs(img["ID"])), 180)
    left = zfs(f"zfs list -H -o name | grep -c '{MAC}' || true")
    check("6a", "临时资产已清理", s1 in (200, 204) and s2 in (200, 204) and bool(gone), f"终端 {s1} 分组 {s2}")
    check("6b", "池里没有残留", left == "0", f"残留 {left} 个")

sys.exit(summary())
