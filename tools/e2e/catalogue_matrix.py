#!/usr/bin/env python3
"""目录保全与可见性：复制回滚会不会吃掉还原点、保住的副本会不会被自动清掉、
运维看不看得见。

这几条只有在真集群上才有意义——单元测试用的是假池，而 2026-09-22 丢还原点那次，
单元测试全绿。

前置：ND_BASE 指向 VIP，SSH 能连到各节点。本脚本在备机上制造一次分叉（会触发
该备机整体重建目录，几分钟），并在结束时清掉自己造的数据集与临时配置。
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import call, check, configs, section, skip, summary  # noqa: E402
from ndcluster import pool_of, ssh_out  # noqa: E402

NAME = "e2e-cat-" + time.strftime("%H%M%S")
STAMP = str(int(time.time() * 1e9))


def wait(cond, seconds=120, step=5):
    for _ in range(max(1, seconds // step)):
        got = cond()
        if got:
            return got
        time.sleep(step)
    return None


def zfs(host, cmd, timeout=120):
    """命令失败不把套件带走——建场/清场里很多命令本来就可能无事可做。"""
    _, out = ssh_out(host, cmd, timeout=timeout)
    return out.strip()


def nodes():
    """(写入者, [备机...])。写入者就是此刻接住 VIP 的那台，它自己报 is_self。"""
    st, view = call("GET", "/api/cluster/pools")
    if st != 200 or not isinstance(view, dict):
        return "", []
    writer, others = "", []
    for node in view.get("nodes", []):
        ip = node.get("ip") or ""
        if node.get("is_self"):
            writer = ip
        elif ip:
            others.append(ip)
    return writer, others


def aside_copies(host, pool, kind="-diverged-"):
    out = zfs(host, f"zfs list -H -o name -d 1 {pool} | grep -- '{kind}' || true")
    return [x for x in out.split() if x]


def any_aside(host, pool):
    """两种挪开副本都算：正常删一个还原点，一种都不该出现。"""
    return sorted(aside_copies(host, pool) + aside_copies(host, pool, "-rebuilding-"))


def marker(host, pool):
    return zfs(host, f"zfs list -H -t snapshot -o name -r {pool}/nd 2>/dev/null | grep @rep- | tail -1")


def history():
    _, h = call("GET", "/api/tasks/history?limit=20")
    return h if isinstance(h, list) else (h or {}).get("items", [])


def caught_up():
    """备机是否真的追平了写入者。

    只比最新标记名不够：整份接收时根数据集的快照最先落地，子数据集还在路上，
    那时名字已经一样了。要求整棵目录每个数据集都有写入者的最新标记，而且没有
    没收完的续传。
    """
    latest = marker(WRITER, W_POOL).split("@")[-1]
    if not latest:
        return False
    count = lambda host, pool: zfs(host, f"zfs list -H -o name -t snapshot -r {pool}/nd 2>/dev/null | grep -c '@{latest}$' || true")
    token = zfs(PEER, f"zfs get -H -o value receive_resume_token {P_POOL}/nd 2>/dev/null")
    want = count(WRITER, W_POOL)
    return want not in ("", "0") and count(PEER, P_POOL) == want and token in ("-", "")


def alarms():
    st, res = call("GET", "/api/alarms?page_size=100")
    items = (res or {}).get("items", []) if isinstance(res, dict) else []
    return [a for a in items if (a.get("status") or a.get("Status")) == "active"]


WRITER, PEERS = nodes()
if not WRITER:
    print("找不到写入者，停")
    sys.exit(2)
W_POOL = pool_of(WRITER)
PEER = PEERS[0] if PEERS else ""
P_POOL = pool_of(PEER) if PEER else ""
print(f"写入者 {WRITER}({W_POOL})  备机 {PEER}({P_POOL})")

section("0 前置：临时配置")
st, gs = call("GET", "/api/groups")
tmpl = next((g for g in (gs or {}).get("items", []) if g.get("SystemConfigID")), None)
if not tmpl:
    print("没有可参照的分组，停")
    sys.exit(2)
IMG, SRC = tmpl["SystemImageID"], tmpl["SystemConfigID"]
st, reds = call("GET", f"/api/configs/{SRC}/reductions")
base_reds = (reds or {}).get("items", []) if isinstance(reds, dict) else []
if not base_reds:
    print("模板配置没有还原点，无从分起，停")
    sys.exit(2)
call("POST", f"/api/configs/{SRC}/fork", {"name": NAME, "reduction_id": base_reds[-1]["ID"]})
cfg = wait(lambda: next((c for c in configs(IMG) if c["Name"] == NAME), None))
check("0a", "临时配置已就绪", bool(cfg))
if not cfg:
    sys.exit(summary())
CFG = cfg["ID"]
# 第 2 段拿它在整体同步期间试删
NAME2 = NAME + "-b"
call("POST", f"/api/configs/{SRC}/fork", {"name": NAME2, "reduction_id": base_reds[-1]["ID"]})
cfg2 = wait(lambda: next((c for c in configs(IMG) if c["Name"] == NAME2), None))
check("0b", "第二个临时配置已就绪", bool(cfg2))
CFG2 = cfg2["ID"] if cfg2 else ""
DUMMY = f"{P_POOL}/nd-diverged-{STAMP}" if PEER else ""

try:
    section("1 写入者正常删还原点，备机不该整体重建")
    if not PEER:
        skip("1a", "删还原点不触发重建", "单节点，无备机可观察")
    else:
        before = any_aside(PEER, P_POOL)
        st, _ = call("POST", f"/api/configs/{CFG}/reductions", {"name": "e2e-del"})
        red = wait(lambda: next((x for x in call("GET", f"/api/configs/{CFG}/reductions")[1].get("items", [])
                                 if (x.get("DisplayName") or x.get("Name", "")).endswith("e2e-del")), None), 120)
        check("1a", "还原点已建", bool(red), f"HTTP {st}")
        # 等它复制到备机，否则备机上没有这个快照，测不到该路径
        got = wait(lambda: "e2e-del" in zfs(PEER, f"zfs list -H -t snapshot -o name -r {P_POOL}/nd/{CFG} 2>/dev/null"), 240)
        check("1b", "还原点已复制到备机", bool(got))
        if red:
            call("DELETE", f"/api/reductions/{red['ID']}")
        gone = wait(lambda: "e2e-del" not in zfs(PEER, f"zfs list -H -t snapshot -o name -r {P_POOL}/nd/{CFG} 2>/dev/null"), 240)
        check("1c", "删除也复制到了备机", bool(gone))
        check("1d", "备机没有因为一次正常删除就整体重建",
              any_aside(PEER, P_POOL) == before, f"删除前 {before} → 现在 {any_aside(PEER, P_POOL)}")

    section("2 备机上出现对端没有的内容：保全而不是回滚")
    if not PEER:
        skip("2a", "分叉时保全", "单节点，无备机可观察")
    else:
        # 先埋一份旧的保留副本，第 4 段要看它会不会被这次重建顺手清掉
        zfs(PEER, f"zfs create {DUMMY}")
        check("2a", "埋下一份旧的保留副本", zfs(PEER, f"zfs list -H -o name {DUMMY} 2>/dev/null") == DUMMY, DUMMY)
        before = set(aside_copies(PEER, P_POOL))
        # 配置得先复制到备机，才谈得上在它上面打快照
        wait(lambda: zfs(PEER, f"zfs list -H -o name {P_POOL}/nd/{CFG} 2>/dev/null").endswith(CFG), 240)
        # 在备机上打一个写入者没有的内容快照，模拟它曾当过写入者
        zfs(PEER, f"zfs snapshot {P_POOL}/nd/{CFG}@e2e-local")
        check("2b", "造出一个对端没有的还原点",
              "e2e-local" in zfs(PEER, f"zfs list -H -t snapshot -o name -r {P_POOL}/nd/{CFG} 2>/dev/null"))
        made = wait(lambda: (set(aside_copies(PEER, P_POOL)) - before) or None, 600)
        check("2c", "备机把自己那份挪到一边保住了", bool(made), str(made))
        if made:
            saved = list(made)[0]
            check("2d", "被保住的那份里还有那个还原点",
                  "e2e-local" in zfs(PEER, f"zfs list -H -t snapshot -o name -r {saved} 2>/dev/null"), saved)
        # 保全后备机整体重建期间目录被占用，删配置必须当场拒绝并点名备机，而不是异步失败。
        if made and CFG2:
            st, r = call("DELETE", f"/api/configs/{CFG2}")
            if st == 409:
                check("2f", "整体同步期间删配置当场拒绝，并点名备机", PEER in str(r), str(r)[:100])
            else:
                task_id = (r or {}).get("task_id", "") if isinstance(r, dict) else ""
                done = wait(lambda: next((t for t in history()
                                          if t.get("ID") == task_id and t.get("Status") in ("success", "failed")), None), 90)
                if done and done.get("Status") == "failed":
                    check("2f", "整体同步期间删配置失败时点名备机、不提客户机",
                          PEER in done.get("Error", "") and "客户机" not in done.get("Error", ""), done.get("Error", "")[:100])
                else:
                    skip("2f", "整体同步期间删配置被拒", f"没赶上整体发送的窗口（HTTP {st}，任务 {(done or {}).get('Status')}）")
                    CFG2 = ""  # 已经删掉了
        # 复制不能因此停摆：备机要能重新追上写入者
        caught = wait(caught_up, 900)
        check("2e", "复制继续走，备机重新追平", bool(caught),
              f"备机 {marker(PEER, P_POOL)[-24:]} 写入者 {marker(WRITER, W_POOL)[-24:]}")
        if CFG2:
            st, r = call("DELETE", f"/api/configs/{CFG2}")
            gone2 = wait(lambda: not any(c["Name"] == NAME2 for c in configs(IMG)), 120)
            check("2g", "同步完成后同一个删除能成功", st in (200, 202) and bool(gone2), f"HTTP {st} {str(r)[:80]}")
            if gone2:
                CFG2 = ""

    section("3 自动清理不许碰保住的那份")
    if not PEER:
        skip("3a", "清理不碰保留副本", "单节点，无备机可观察")
    else:
        # 重建只该回收 -rebuilding- 残留，埋下的旧旁置副本必须原样保留。
        check("3a", "重建之后那份保留副本还在",
              zfs(PEER, f"zfs list -H -o name {DUMMY} 2>/dev/null") == DUMMY, DUMMY)

    section("4 保留的副本要能在告警里看见")
    if not PEER:
        skip("4a", "保留副本告警", "单节点，无备机可观察")
    else:
        hit = wait(lambda: next((a for a in alarms() if DUMMY in str(a.get("Message", ""))), None), 150)
        check("4a", "告警里列出了保留的副本", bool(hit), str(hit.get("Message", ""))[:100] if hit else "没等到")
        if hit:
            check("4b", "告警说清是哪台机器", PEER in str(hit.get("Message", "")), str(hit.get("Message"))[:80])
        zfs(PEER, f"zfs destroy -r {DUMMY}")
        recovered = wait(lambda: not any(DUMMY in str(a.get("Message", "")) for a in alarms()), 180)
        check("4c", "删掉副本后告警自动恢复", bool(recovered))

    section("6 备机停机几分钟回来：增量追上，而不是整份重建")
    # 写入者只留最近 3 个复制标记，但要保住每台备机最后确认的那个；
    # 否则停机超过 3 轮的备机没有共同快照，只能整份重建目录。
    if not PEER:
        skip("6a", "停机后增量追上", "单节点，无备机可观察")
    else:
        stopped_at = ""
        check("6-", "开始前备机已完整追平（否则测的是半截接收）", bool(wait(caught_up, 900)))
        try:
            before_aside = any_aside(PEER, P_POOL)
            held = marker(PEER, P_POOL).split("@")[-1]
            zfs(PEER, "systemctl stop ndiskless")
            stopped_at = zfs(PEER, "date -u '+%Y-%m-%d %H:%M:%S'")
            check("6a", "备机已停，记下它手里最新的标记", bool(held), held)
            rounds = 0
            for i in range(5):
                last = marker(WRITER, W_POOL)
                call("POST", f"/api/configs/{CFG}/reductions", {"name": f"e2e-pin-{i}"})
                if wait(lambda: marker(WRITER, W_POOL) != last, 150, 5):
                    rounds += 1
            check("6b", "写入者在备机停机期间打出了 5 轮以上的新标记", rounds >= 5, f"{rounds} 轮")
            kept = zfs(WRITER, f"zfs list -H -o name {W_POOL}/nd@{held} 2>/dev/null")
            check("6c", "写入者仍留着备机手里那个标记", kept.endswith(held), kept or "已被清掉")
        finally:
            zfs(PEER, "systemctl start ndiskless")
        caught = wait(caught_up, 600)
        check("6d", "备机回来后追平了写入者", bool(caught),
              f"备机 {marker(PEER, P_POOL)[-24:]} 写入者 {marker(WRITER, W_POOL)[-24:]}")
        rebuilt = zfs(PEER, f"journalctl -u ndiskless --since '{stopped_at}' -o cat | grep -c 'no snapshot in common' || true") if stopped_at else "?"
        check("6e", "是增量追上的，没有整份重建", rebuilt.strip() == "0" and any_aside(PEER, P_POOL) == before_aside,
              f"整份重建 {rebuilt.strip()} 次，旁置副本 {before_aside} → {any_aside(PEER, P_POOL)}")

finally:
    section("5 清理")
    if PEER:
        zfs(PEER, f"zfs destroy -r {DUMMY}")
        for name in aside_copies(PEER, P_POOL):
            if STAMP in name or name.endswith(STAMP):
                zfs(PEER, f"zfs destroy -r {name}")
    # 备机还在整体同步时删除会被拒（2f 测的就是这个），等它同步完再删
    gone = None
    for _ in range(6):
        for cid in (CFG, CFG2):
            if cid:
                call("DELETE", f"/api/configs/{cid}")
        gone = wait(lambda: not any(c["Name"] in (NAME, NAME2) for c in configs(IMG)), 120)
        if gone:
            break
    left = [c["Name"] for c in configs(IMG) if c["Name"].startswith("e2e-cat-")]
    check("5a", "临时配置已删除", bool(gone), f"残留 {left}")
    if PEER:
        # 第 2 段那份保留副本是本脚本伪造出来的（里面只有 e2e-local），自己收干净
        for name in aside_copies(PEER, P_POOL):
            if "e2e-local" in zfs(PEER, f"zfs list -H -t snapshot -o name -r {name} 2>/dev/null"):
                zfs(PEER, f"zfs destroy -r {name}")
        check("5b", "本次造出的保留副本已清理",
              not any("e2e-local" in zfs(PEER, f"zfs list -H -t snapshot -o name -r {n} 2>/dev/null")
                      for n in aside_copies(PEER, P_POOL)), str(aside_copies(PEER, P_POOL)))

sys.exit(summary())
