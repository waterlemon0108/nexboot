#!/usr/bin/env python3
"""存储池详情：页面说的和池里实际的要对得上账。只读，不建也不删任何东西。

单元测试里的 zfs 输出是录下来的，这里对着真机核对：列表的字段、合计能不能闭合、
读一次会不会改动池。集群上跑时，别的节点的池只查合计和读得到与否（SSH 只连 ND_SSH_HOST）。
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import BASE, POOL, call, check, configs, section, ssh, summary  # noqa: E402

# zfs 的数字在客户机运行时会变，核对留一点余量。
DRIFT = 64 << 20
# 和 main.go 里 PoolFactsCache 的 TTL 一致。
CACHE_TTL = 10


def volumes(pool, node=""):
    q = {"pool": pool}
    if node:
        q["node"] = node
    return call("GET", "/api/volumes?" + urllib.parse.urlencode(q))


def zfs_rows(pool):
    out = ssh(f"zfs list -Hp -r -t filesystem,volume -o name,used,volsize,available {pool}")
    rows = {}
    for line in out.splitlines():
        name, used, volsize, avail = line.split("\t")
        rel = "" if name == pool else name[len(pool) + 1:]
        rows[rel] = {"used": int(used), "volsize": 0 if volsize == "-" else int(volsize), "avail": int(avail)}
    return rows


def near(a, b):
    return abs(a - b) <= DRIFT


def unauthenticated_status(path):
    try:
        with urllib.request.urlopen(BASE + path, timeout=15) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def check_data_pool(pool):
    section(f"数据池 {pool}：页面和 zfs 对账")
    status, res = volumes(pool)
    if not check("A1", "读得到这个池", status == 200 and res.get("reachable") is True, f"{status} {str(res)[:200]}"):
        return
    rows = zfs_rows(pool)
    check("A2", "角色是数据池", res["role"] == "data", res["role"])
    check("A3", "已用、可用和 zfs 一致", near(res["used"], rows[""]["used"]) and near(res["available"], rows[""]["avail"]),
          f"页面 {res['used']}/{res['available']} zfs {rows['']['used']}/{rows['']['avail']}")
    total = sum(c["bytes"] for c in res["categories"])
    check("A4", "各类别合计等于已用", total == res["used"], f"合计 {total} 已用 {res['used']}")

    claimed = set()
    bad_used, bad_size = [], []
    for img in res["images"]:
        names = [img["id"]]
        if img.get("state") not in ("pending_delete", "importing"):
            names += [c["ID"] for c in configs(img["id"])]
        expect = sum(rows.get(f"nd/{n}", {}).get("used", 0) for n in names)
        claimed.update(f"nd/{n}" for n in names)
        if not near(img["used"], expect):
            bad_used.append(f"{img['id']}: 页面 {img['used']} zfs {expect}")
        vol = rows.get(img["dataset"], {}).get("volsize", 0)
        if vol and img["size"] != vol:
            bad_size.append(f"{img['id']}: 页面 {img['size']} zfs {vol}")
    check("A5", "镜像实占含配置，和 zfs 一致", not bad_used, "；".join(bad_used))
    check("A6", "镜像大小取 volsize", not bad_size, "；".join(bad_size))

    residues = {r["dataset"] for r in res["residues"]}
    orphan = [n for n in rows if n.startswith("nd/") and n.count("/") == 1 and n not in ("nd/db", "nd/files")
              and n not in claimed and n not in residues]
    check("A7", "nd 下每个卷都有归属，或被列为残留", not orphan, "、".join(orphan))

    disks = {d["dataset"] for c in res["clients"] for d in c["disks"]}
    missing = [n for n in rows if n.startswith("run/") and n.count("/") == 1 and n not in disks]
    check("A8", "run 下每块盘都列出来了", not missing, "、".join(missing))

    tops = [n for n in rows if n and "/" not in n and n not in ("nd", "run", "imports")]
    unknown = {s["path"] for s in res["system"] if s["kind"] == "unknown"}
    lost = [n for n in tops if n not in residues and n not in unknown]
    check("A9", "池顶层的数据集不漏，认不出的也要列", not lost, "、".join(lost))

    _, groups = call("GET", "/api/groups")
    used = {g["SystemImageID"] for g in groups.get("items", [])}
    wrong = [i["id"] for i in res["images"] if i["id"] in used and not i["users"]]
    check("A10", "分组在用的镜像不会标成未使用", not wrong, "、".join(wrong))

    if res["files_readable"]:
        dir_out = ssh(f"F=$(zfs get -H -o value mountpoint {pool}/nd/files); ls -1 \"$F/imports\" 2>/dev/null")
        on_disk = {n for n in dir_out.splitlines() if n}
        listed = {f["name"] for f in res["imports"]}
        check("A11", "导入文件和目录里一致", on_disk == listed, f"目录 {sorted(on_disk)} 页面 {sorted(listed)}")
    else:
        check("A11", "文件库读不到时要如实说明", res["imports"] == [], str(res["imports"]))


def check_read_only(pool):
    section("读一次不改动池")
    before = ssh(f"zfs list -H -r -t all -o name {pool} | grep -v '@rep-' | sort")
    # 前面的用例刚读过，不等缓存过期，下面量到的就不是真正的 zfs list。
    time.sleep(CACHE_TTL + 1)
    started = time.time()
    status, _ = volumes(pool)
    first = time.time() - started
    started = time.time()
    volumes(pool)
    second = time.time() - started
    after = ssh(f"zfs list -H -r -t all -o name {pool} | grep -v '@rep-' | sort")
    check("B1", "数据集和快照都没变", status == 200 and before == after)
    check("B2", "读一次不到 5 秒", first < 5, f"{first:.2f}s")
    check("B3", "10 秒内重复打开走缓存", second < 1, f"第一次 {first:.2f}s 第二次 {second:.2f}s")


def check_refusals(pool):
    section("拒绝和读不到")
    status, _ = call("GET", "/api/volumes")
    check("C1", "没有池名要拒绝", status == 400, str(status))
    status, res = volumes("no-such-pool")
    check("C2", "没有这个池时说明，不显示成空", status == 200 and res.get("reachable") is False and "no-such-pool" in res.get("error", ""),
          json.dumps(res, ensure_ascii=False)[:200])
    status, _ = volumes(pool, "no-such-node")
    check("C3", "不存在的节点", status == 404, str(status))
    check("C4", "控制台接口要登录", unauthenticated_status(f"/api/volumes?pool={pool}") == 401)
    check("C5", "节点接口要集群令牌", unauthenticated_status(f"/internal/node/volumes?pool={pool}") == 401)


def check_cluster():
    status, cluster = call("GET", "/api/cluster/pools")
    if status != 200 or len(cluster.get("nodes", [])) < 2:
        return
    section("集群：每台的每个池都读得到，账都闭合")
    for node in cluster["nodes"]:
        if not node.get("reachable"):
            continue
        for p in node.get("pools") or []:
            who = f"{node['ip']} {p['Name']}"
            status, res = volumes(p["Name"], node["node_id"])
            ok = status == 200 and res.get("reachable") is True
            check(f"D-{who}", f"{who} 读得到", ok, str(res)[:200] if not ok else "")
            if ok:
                total = sum(c["bytes"] for c in res["categories"])
                check(f"D-{who}-sum", f"{who} 合计等于已用", total == res["used"] or res["role"] == "backup", f"{total}/{res['used']}")


def main():
    check_data_pool(POOL)
    check_read_only(POOL)
    check_refusals(POOL)
    check_cluster()
    return summary()


if __name__ == "__main__":
    sys.exit(main())
