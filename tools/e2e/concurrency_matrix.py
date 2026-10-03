#!/usr/bin/env python3
"""同一镜像上的并发操作：一个做完之前，另一个要么等不到提交（当场 409、说清在忙什么），
要么排在它之后——不许两个交错着改同一棵克隆树。

单元测试用假池证明过两处真损坏（应用与删除交错留下悬空的当前点；同一台超管机两次
保存交错把配置改名改丢）。这里在真池、真 HTTP 并发上验一遍契约。

只动自己新建的一个空白数据盘镜像：锁是按镜像的，拿现场镜像做实验会在这几十秒里
挡住操作员在同一镜像上的操作。

前置：ND_BASE 指向 VIP（集群）或单机地址。不需要 SSH。
"""
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import call, check, configs, section, skip, summary  # noqa: E402

PREFIX = "e2e-conc"
NAME = f"{PREFIX}-{time.strftime('%H%M%S')}"


def wait(cond, seconds=120, step=2):
    for _ in range(max(1, seconds // step)):
        got = cond()
        if got:
            return got
        time.sleep(step)
    return None


def at_once(*requests):
    """同一瞬间发出几个请求，返回各自的 (状态码, 响应)，顺序与参数一致。"""
    out = [None] * len(requests)
    gate = threading.Barrier(len(requests))

    def fire(i, req):
        gate.wait()
        out[i] = call(*req)

    threads = [threading.Thread(target=fire, args=(i, r)) for i, r in enumerate(requests)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    return out


def task(task_id):
    st, t = call("GET", f"/api/tasks/{task_id}")
    return t if st == 200 and isinstance(t, dict) else None


def settled(task_ids, seconds=180):
    """全部结束才算；返回 {任务 ID: 任务}。"""
    def done():
        got = {i: task(i) for i in task_ids}
        if all(t and t.get("Status") in ("success", "failed") for t in got.values()):
            return got
        return None
    return wait(done, seconds) or {}


def reductions(cfg_id):
    st, res = call("GET", f"/api/configs/{cfg_id}/reductions")
    return (res or {}).get("items", []) if isinstance(res, dict) else []


def config(cfg_id):
    return next((c for c in configs(IMG) if c["ID"] == cfg_id), {})


def image_by_name(name):
    st, imgs = call("GET", "/api/images")
    return next((i for i in (imgs or {}).get("items", []) if i.get("Name") == name), None)


def refused_well(resp):
    """被拒的样子：409，并且说出是哪个镜像、在忙什么。"""
    st, body = resp
    text = str(body)
    return st == 409 and NAME in text and "请等该任务完成" in text


def cleanup_leftovers():
    st, imgs = call("GET", "/api/images")
    for img in (imgs or {}).get("items", []):
        if img.get("Name", "").startswith(PREFIX):
            call("DELETE", f"/api/images/{img['ID']}")


section("0 前置：一个自己的空白数据盘镜像")
cleanup_leftovers()
st, body = call("POST", "/api/images/blank", {"name": NAME, "size_bytes": 1024 ** 3, "filesystem": "ntfs"})
made = wait(lambda: image_by_name(NAME), 180)
IMG = (made or {}).get("ID", "")
cfgs = configs(IMG) if IMG else []
CFG = cfgs[0]["ID"] if cfgs else ""
check("0a", "空白数据盘镜像已建好", bool(IMG) and bool(CFG), f"HTTP {st} {str(body)[:100]}")
if not CFG:
    sys.exit(summary())

try:
    section("1 同一镜像同时提交多个操作：只受理一个，其余当场拒绝并说明原因")
    resps = at_once(*[("POST", f"/api/configs/{CFG}/reductions", {"name": f"e2e-c1-{i}"}) for i in range(5)])
    accepted = [r[1].get("task_id") for r in resps if r[0] == 202 and isinstance(r[1], dict)]
    refused = [r for r in resps if r[0] != 202]
    check("1a", "至少受理了一个", len(accepted) >= 1, str([r[0] for r in resps]))
    if not refused:
        skip("1b", "被拒的点名镜像和正在进行的操作", "五个请求没撞上（前一个在后一个到达前就做完了）")
    else:
        check("1b", "被拒的都是 409，并点名镜像和正在进行的操作",
              all(refused_well(r) and "创建还原点" in str(r[1]) for r in refused), str(refused[0])[:140])
    tasks = settled(accepted)
    check("1c", "受理的任务全部成功", len(tasks) == len(accepted) and all(t["Status"] == "success" for t in tasks.values()),
          str([(t.get("Status"), (t.get("Error") or "")[:60]) for t in tasks.values()]))
    names = [r.get("DisplayName") or r.get("Name", "") for r in reductions(CFG)]
    made_now = [n for n in names if "e2e-c1-" in n]
    check("1d", "建出来的还原点数与受理数一致", len(made_now) == len(accepted), f"受理 {len(accepted)}，实有 {made_now}")

    section("2 应用与删除同一个还原点同时发生：当前点不许悬空")
    for rnd in range(3):
        call("POST", f"/api/configs/{CFG}/reductions", {"name": f"e2e-c2-{rnd}"})
        red = wait(lambda: next((r for r in reductions(CFG)
                                 if (r.get("DisplayName") or r.get("Name", "")).endswith(f"e2e-c2-{rnd}")), None), 120)
        if not red:
            check(f"2{rnd}-", f"第 {rnd + 1} 轮的还原点已建", False)
            continue
        delete, apply = at_once(("DELETE", f"/api/reductions/{red['ID']}", None),
                                ("POST", f"/api/reductions/{red['ID']}/apply", None))
        if delete[0] == 202 and isinstance(delete[1], dict):
            settled([delete[1].get("task_id")])
        alive = {r["ID"] for r in reductions(CFG)}
        current = config(CFG).get("DefaultReductionID")
        check(f"2{rnd}a", f"第 {rnd + 1} 轮：当前点指向一个存在的还原点",
              current in alive, f"当前点 {current}；删除 HTTP {delete[0]}，应用 HTTP {apply[0]}")
        ok = lambda r: r[0] in (200, 202, 204, 404) or (r[0] == 409 and bool(str(r[1]).strip()))
        check(f"2{rnd}b", f"第 {rnd + 1} 轮：两个请求要么成、要么说明原因地被拒",
              ok(delete) and ok(apply), f"删除 {delete[0]} {str(delete[1])[:60]} / 应用 {apply[0]} {str(apply[1])[:60]}")
        if apply[0] == 200 and red["ID"] not in alive:
            # 应用先成功、删除随后执行，是先后而非交错（当前点已在 2a 核过），这里只记录顺序。
            print(f"      第 {rnd + 1} 轮：先应用后删除，当前点已移到 {current}")

    section("3 做完之后，库和池对得上")
    st, report = call("GET", "/api/system/consistency")
    issues = ((report or {}).get("issues") or []) if isinstance(report, dict) else []
    mine = [i for i in issues if i.get("kind", "").startswith("missing") and (IMG in str(i) or CFG in str(i))]
    check("3a", "本镜像没有「库里有、池里没有」的条目", st == 200 and not mine, str(mine)[:160] if mine else f"HTTP {st}")
finally:
    section("9 清理")
    gone = None
    for _ in range(4):
        call("DELETE", f"/api/images/{IMG}")
        gone = wait(lambda: image_by_name(NAME) is None, 60)
        if gone:
            break
    check("9a", "临时镜像已删除", bool(gone))

sys.exit(summary())
