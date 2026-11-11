#!/usr/bin/env python3
"""删除导入文件：删得掉、备机跟着删、空间真的回来、不该删的删不掉。

只动自己上传的 e2e-del-* 文件（64 MiB），不碰现场已有的文件，结束时清理。
单机和集群都能跑：集群上 ND_BASE 走 VIP，SSH 连 VIP 所在的写入者。
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
from ndapi import BASE, POOL, call, check, login, section, ssh, summary  # noqa: E402

NAME = f"e2e-del-{time.strftime('%H%M%S')}.iso"
SIZE = 64 << 20
CHUNK = 8 << 20
# 标记每分钟一轮，强制打 4 轮，备机再晚一轮。
RELEASE_WAIT = 420
FOLLOW_WAIT = 240


def volumes(node=""):
    q = {"pool": pool_of(node)}
    if node:
        q["node"] = node
    return call("GET", "/api/volumes?" + urllib.parse.urlencode(q))


NODES = {}  # node_id -> 这台的池名：集群里各台的池名可以不同


def pool_of(node):
    return NODES.get(node, POOL)


def names_on(node=""):
    status, res = volumes(node)
    if status != 200 or not res.get("reachable"):
        return None
    return {f["name"] for f in res["imports"]}


def gone_from(node=""):
    # 读不到不算删掉了；列表空了才算。
    names = names_on(node)
    return names is not None and NAME not in names


def has_deleted_row(node=""):
    status, res = volumes(node)
    return status == 200 and any(s["kind"] == "deleted_files" for s in res.get("system") or [])


def marks_older_than(when):
    """写入者上比删除时刻早的复制标记。它们占着删掉的内容，文件比它们早时空间就回不来。"""
    out = ssh(f"zfs list -Hp -t snapshot -d 1 -o name,creation {writer_pool()}/nd")
    old = []
    for line in out.splitlines():
        name, created = line.split("\t")
        if "@rep-" in name and int(created) < when:
            old.append(name.split("@", 1)[1])
    return old


def writer_pool():
    return ssh("cat /var/lib/ndiskless/data-pool 2>/dev/null").strip() or POOL


def wait_for(fn, limit, step=10):
    deadline = time.time() + limit
    while time.time() < deadline:
        if fn():
            return True
        time.sleep(step)
    return fn()


def upload():
    data = os.urandom(SIZE)
    status, s = call("POST", "/api/images/uploads", {"file_name": NAME, "size_bytes": SIZE, "last_modified": int(time.time() * 1000)})
    if status not in (200, 201):
        return False, f"{status} {s}"
    token = login()
    offset = int(s.get("received") or 0)
    last = s
    while offset < SIZE:
        piece = data[offset:offset + CHUNK]
        req = urllib.request.Request(f"{BASE}/api/images/uploads/{s['upload_id']}", data=piece, method="PATCH")
        req.add_header("Authorization", "Bearer " + token)
        req.add_header("Content-Type", "application/octet-stream")
        req.add_header("Content-Range", f"bytes {offset}-{offset + len(piece) - 1}/{SIZE}")
        with urllib.request.urlopen(req, timeout=120) as r:
            last = json.loads(r.read().decode())
        offset += len(piece)
    return bool(last.get("complete")), str(last)[:160]


def delete(name):
    return call("DELETE", "/api/volumes/imports/" + name)


def unauthenticated_delete():
    req = urllib.request.Request(f"{BASE}/api/volumes/imports/{NAME}", method="DELETE")
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def peers():
    status, cluster = call("GET", "/api/cluster/pools")
    out = []
    if status != 200 or len(cluster.get("nodes", [])) < 2:
        return out
    for node in cluster["nodes"]:
        pools = [p["Name"] for p in node.get("pools") or [] if p.get("Role") == "data"]
        if pools:
            NODES[node["node_id"]] = pools[0]
        if not node.get("is_self") and node.get("reachable") and pools:
            out.append(node)
    return out


def main():
    others = peers()
    section("准备：上传一个自己的文件")
    # 基线要在上传前取：刚写完的数据还没落盘，已用只算进去一部分。
    used_before = volumes()[1]["used"]
    ok, detail = upload()
    if not check("P1", "上传完成", ok, detail):
        return summary()
    check("P2", "这台的列表里有它", wait_for(lambda: NAME in (names_on() or set()), 30, 3))
    for node in others:
        got = wait_for(lambda: NAME in (names_on(node["node_id"]) or set()), FOLLOW_WAIT)
        check(f"P3-{node['ip']}", f"{node['ip']} 收到了它", got)

    try:
        section("不该删的删不掉")
        for bad in ("..%2Fimports", "..%2F..%2Fetc%2Fpasswd", ".uploads"):
            status, body = delete(bad)
            check(f"R-{bad}", f"{urllib.parse.unquote(bad)} 被拒绝", status == 400, f"{status} {str(body)[:120]}")
        check("R2", "未登录不能删", unauthenticated_delete() == 401)
        check("R3", "上传目录还在", ".uploads" in ssh(f"ls -a \"$(zfs get -H -o value mountpoint {POOL}/nd/files)/imports\""))
        check("R4", "文件还在", NAME in (names_on() or set()))

        section("删除")
        status, body = delete(NAME)
        if not check("D1", "删除成功", status == 204, f"{status} {str(body)[:160]}"):
            return summary()
        deleted_at = int(ssh("date +%s"))
        check("D2", "页面马上看不到（不用等缓存）", gone_from())
        status, body = delete(NAME)
        check("D3", "再删一次不报错", status == 204, f"{status} {str(body)[:120]}")
        status, logs = call("GET", "/api/logs?type=operation&size=20")
        check("D4", "审计记下了删的是哪个文件", status == 200 and NAME in json.dumps(logs, ensure_ascii=False), f"{status} {str(logs)[:160]}")

        for node in others:
            # 还没跟上的备机上，这个文件要标待复制删除，否则操作者会以为还能再删。
            status, res = volumes(node["node_id"])
            row = next((f for f in (res.get("imports") or []) if f["name"] == NAME), None) if status == 200 else None
            check(f"F0-{node['ip']}", f"{node['ip']} 还没跟上时标待复制删除", row is None or row.get("state") == "pending_delete", str(row))
        for node in others:
            gone = wait_for(lambda n=node["node_id"]: gone_from(n), FOLLOW_WAIT)
            check(f"F-{node['ip']}", f"{node['ip']} 跟着删了", gone)

        section("空间回来")
        if others:
            seen = wait_for(lambda: has_deleted_row(), 40, 5)
            # 轮转可能已经做完，所以这条看不到不算失败，只记录。
            print(f"  说明：删除后页面{'列出了' if seen else '没列出'}「已删除的文件」")
            released = wait_for(lambda: not has_deleted_row(), RELEASE_WAIT)
            check("S1", "写入者的旧标记轮转完，不再有等待释放的空间", released)
            # 这个文件是刚传的，只看 S1 查不出「切换前的旧标记一直留着」：真正的 ISO 比它们都早。
            gone = wait_for(lambda: not marks_older_than(deleted_at), RELEASE_WAIT)
            check("S4", "比删除早的标记都没了，再旧的文件也能释放", gone, "、".join(marks_older_than(deleted_at)))
            for node in others:
                got = wait_for(lambda: not has_deleted_row(node["node_id"]), RELEASE_WAIT)
                check(f"S2-{node['ip']}", f"{node['ip']} 也释放了", got)
        used_after = {}

        def dropped():
            used_after["v"] = volumes()[1]["used"]
            return used_after["v"] < used_before + SIZE // 2

        check("S3", "已用回到上传前的水平", wait_for(dropped, RELEASE_WAIT if others else 60), f"上传前 {used_before}，现在 {used_after.get('v')}")
    finally:
        delete(NAME)
    return summary()


if __name__ == "__main__":
    sys.exit(main())
