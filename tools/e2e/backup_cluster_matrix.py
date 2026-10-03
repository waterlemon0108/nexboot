#!/usr/bin/env python3
"""集群里的本地备份：每台节点各自把目录备份进自己的备份池，写入者换了也接着增量。

`backup_matrix` 是单机形态写的（一台主机、一个数据池一个备份池），测的是一条备份链
本身：整库一条流、promote 之后自愈、基准丢失退回全量。这里测的是集群才有的那一半：

    - 写入者打标记，标记随复制到达每台节点，有备份池的节点各自把自己那份送进去；
    - 第二轮是增量，不是又一次全量；
    - 计划切换之后由新写入者打标记，每台的备份链接着往下走，不从头来。

真机 2026-09-29 才发现：这个集群建好了两个备份池，备份却从来没开过、也从没有人跑过
这类用例。

会临时改备份配置（结束时还原），会做一次计划切换，结束时删掉本套件在备份池里留下的
副本和数据池上的备份标记。

    ND_NODES / ND_VIP / ND_PASSWORD / ND_CLUSTER_TOKEN / ND_SSH_USER
"""
import sys
import time

from ndcluster import (NODES, VIP, VIP_BASE, call, check, host, pool_of, section, skip,
                       ssh, ssh_out, summary, wait)

# 备份经复制到达备机要一轮（最多两分钟），备机每两分钟送一次，超时要给足。
SHIP_WINDOW = 480


def holds_vip(b):
    _, out = ssh_out(host(b), f"ip -4 -o addr show | grep -c ' {VIP}/' || true")
    return out.strip() == "1"


def writer():
    return next((b for b in NODES if holds_vip(b)), None)


def backup_pool(b):
    """本机的备份池：不是数据池的那个产品池（挂在 /ndiskless/<名> 下）。"""
    data = pool_of(host(b))
    _, out = ssh_out(host(b), "for p in $(zpool list -H -o name); do "
                              "[ \"$(zfs get -H -o value mountpoint $p)\" = \"/ndiskless/$p\" ] && echo $p; done")
    return next((p for p in out.split() if p and p != data), "")


def markers(dataset):
    """数据集上的备份标记，按创建先后。"""
    _, out = ssh_out(host(dataset[0]), f"zfs list -H -o name -t snapshot -s creation -d 1 {dataset[1]} 2>/dev/null "
                                       "| grep '@ndbackup-' | sed 's/.*@//'")
    return [m for m in out.split() if m]


def copy_markers(b):
    bp = BACKUP[b]
    return markers((b, f"{bp}/ndiskless/nd")) if bp else []


def source_markers(b):
    return markers((b, f"{pool_of(host(b))}/nd"))


def task(task_id, seconds=600):
    def done():
        st, t = call(VIP_BASE, "GET", f"/api/tasks/{task_id}")
        return t if st == 200 and isinstance(t, dict) and t.get("Status") in ("success", "failed") else None
    return wait(done, seconds, step=5) or {}


def run_backup(label):
    st, body = call(VIP_BASE, "POST", "/api/backup/run", timeout=60)
    tid = (body or {}).get("task_id", "") if isinstance(body, dict) else ""
    t = task(tid) if tid else {}
    check(f"{label}a", "备份任务成功", st == 202 and t.get("Status") == "success",
          f"HTTP {st} {t.get('Status')} {(t.get('Error') or t.get('Result') or str(body))[:120]}")
    return t


def full_fallbacks(b, since):
    """这台从 since 起有没有退回全量：增量收不下时 Agent.Backup 会记这一句。"""
    _, out = ssh_out(host(b), f"journalctl -u ndiskless --since '{since}' -o cat | "
                              "grep -c 'incremental backup not receivable' || true")
    return int(out.strip() or 0) if out.strip().isdigit() else -1


def copy_complete(b, marker):
    """副本里的数据集都收到了这一轮，且没有收到一半的续传。副本的根先于子数据集落地，只看根会早判。"""
    src, bp = pool_of(host(b)), BACKUP[b]
    rel = lambda root: (f"zfs list -H -o name -t snapshot -r {root} 2>/dev/null | grep '@{marker}$' "
                        f"| sed 's#^{root}##' | sort")
    _, have = ssh_out(host(b), rel(f"{bp}/ndiskless/nd"))
    _, want = ssh_out(host(b), rel(f"{src}/nd"))
    _, tokens = ssh_out(host(b), f"zfs get -H -o value -r receive_resume_token {bp}/ndiskless/nd 2>/dev/null | grep -vc '^-$' || true")
    return bool(want) and have == want and tokens.strip() == "0"


def all_shipped(marker):
    """每台有备份池的节点，副本都完整收到了这一轮。"""
    return all(copy_complete(b, marker) for b in NODES if BACKUP[b])


def main():
    global BACKUP
    if len(NODES) < 2 or not VIP:
        skip("0", "集群备份", "需要至少两个节点与 ND_VIP")
        return summary()

    section("0 前置：谁有备份池、原来的备份配置")
    BACKUP = {b: backup_pool(b) for b in NODES}
    with_pool = [b for b in NODES if BACKUP[b]]
    check("0a", "至少一台节点有备份池", bool(with_pool), str({host(b): BACKUP[b] for b in NODES}))
    if not with_pool:
        return summary()
    st, original = call(VIP_BASE, "GET", "/api/backup/config")
    check("0b", "读得到原来的备份配置", st == 200 and isinstance(original, dict), f"HTTP {st}")
    if st != 200:
        return summary()
    started = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime())
    # 本套件开始前备份池里已有的副本不归它管，结束时不碰
    preexisting = {b: bool(copy_markers(b)) for b in with_pool}
    before = {b: set(source_markers(b)) for b in NODES}

    try:
        section("1 定时配置：存得下、读得回、说得清下次什么时候跑")
        st, body = call(VIP_BASE, "PUT", "/api/backup/config", {"enabled": True, "schedule": ""})
        check("1a", "开启却没选时间被拒，并说明原因", st == 400 and "什么时候" in str(body), f"HTTP {st} {str(body)[:80]}")
        # 离服务器当前时刻 12 小时，免得定时备份在测试途中自己跑起来
        _, hour = ssh_out(host(writer()), "date +%H")
        at = f"{(int(hour.strip() or 0) + 12) % 24:02d}:00"
        st, _ = call(VIP_BASE, "PUT", "/api/backup/config", {"enabled": True, "schedule": f"daily@{at}"})
        _, status = call(VIP_BASE, "GET", "/api/backup/status")
        cfg = (status or {}).get("config", {}) if isinstance(status, dict) else {}
        check("1b", f"每天 {at} 存下了，下次运行时间落在 {at}",
              st == 200 and cfg.get("enabled") is True and cfg.get("schedule_text") == f"每天 {at}"
              and at in (cfg.get("next_run_text") or ""), str(cfg)[:160])

        section("2 第一轮：写入者打标记，每台有备份池的节点各自送进自己的备份池")
        t = run_backup("2")
        w = writer()
        first = (source_markers(w) or [""])[-1] if w else ""
        check("2b", "写入者的目录上有这一轮的标记", bool(first), first)
        if BACKUP.get(w):
            check("2c", "写入者自己的副本已更新", "本机副本已更新" in (t.get("Result") or ""), (t.get("Result") or "")[:100])
        else:
            check("2c", "写入者没有备份池时如实说明", "没有备份池" in (t.get("Result") or ""), (t.get("Result") or "")[:100])
        got = wait(lambda: all_shipped(first) or None, SHIP_WINDOW, step=15)
        check("2d", "每台有备份池的节点都收到了这一轮", bool(got),
              str({host(b): (copy_markers(b) or ["无"])[-1] for b in with_pool}))
        for b in with_pool:
            bp = BACKUP[b]
            _, ds = ssh_out(host(b), f"zfs list -H -o name -r {pool_of(host(b))}/nd -t filesystem,volume | "
                                     f"sed 's#^{pool_of(host(b))}/nd##' | sort")
            _, cp = ssh_out(host(b), f"zfs list -H -o name -r {bp}/ndiskless/nd -t filesystem,volume 2>/dev/null | "
                                     f"sed 's#^{bp}/ndiskless/nd##' | sort")
            check(f"2e-{host(b)}", "副本里的数据集与本机目录一致（含数据库副本）",
                  bool(ds) and ds == cp and "/db" in cp.split(), f"本机 {ds.split()} 副本 {cp.split()}")

        section("3 第二轮：增量，不是又一次全量")
        t = run_backup("3")
        second = (source_markers(writer()) or [""])[-1]
        got = wait(lambda: all_shipped(second) or None, SHIP_WINDOW, step=15)
        check("3b", "每台都收到了第二轮", bool(got) and second != first,
              str({host(b): (copy_markers(b) or ["无"])[-1] for b in with_pool}))
        check("3c", "副本上两轮都在（增量接着往下长，没有推倒重来）",
              all(first in copy_markers(b) and second in copy_markers(b) for b in with_pool),
              str({host(b): copy_markers(b) for b in with_pool}))
        if BACKUP.get(writer()):
            check("3d", "写入者这一轮走的是增量", "增量" in (t.get("Result") or ""), (t.get("Result") or "")[:100])
        _, view = call(VIP_BASE, "GET", "/api/cluster/backups")
        nodes = (view or {}).get("nodes", []) if isinstance(view, dict) else []
        protected = [n for n in nodes if n.get("backup_pool") and n.get("last_backup_at") and not n.get("pending")]
        check("3e", "集群备份视图：有备份池的节点都显示已备份、无待送", len(protected) == len(with_pool),
              str([(n.get("ip"), n.get("backup_pool"), n.get("last_backup_at"), n.get("pending")) for n in nodes])[:200])

        section("4 换了写入者：备份链接着往下走")
        old = writer()
        st, body = call(VIP_BASE, "POST", "/api/ha/planned-switch", timeout=240)
        check("4a", "计划切换已受理", st == 202, f"HTTP {st} {str(body)[:100]}")
        new = wait(lambda: (lambda w: w if w and w != old else None)(writer()), 300, step=5)
        # 读接口在备机上也答，要看经 VIP 应答的那台已经是主机
        ready = wait(lambda: (call(VIP_BASE, "GET", "/api/ha")[1] or {}).get("role") == "active" or None, 180, step=5)
        check("4b", "新写入者接管且可用", bool(new) and bool(ready), f"{host(old)} → {host(new) if new else '无'}")
        if new and ready:
            run_backup("4c")
            third = (source_markers(new) or [""])[-1]
            got = wait(lambda: all_shipped(third) or None, SHIP_WINDOW, step=15)
            check("4d", "新写入者打的这一轮，每台都收到了", bool(got) and third not in (first, second),
                  str({host(b): (copy_markers(b) or ["无"])[-1] for b in with_pool}))
            check("4e", "副本上仍连着切换前那一轮（增量，没有从头来）",
                  all(second in copy_markers(b) and third in copy_markers(b) for b in with_pool),
                  str({host(b): copy_markers(b) for b in with_pool}))
        falls = {host(b): full_fallbacks(b, started) for b in with_pool}
        check("4f", "全程没有一台退回全量", all(v == 0 for v in falls.values()), str(falls))
    finally:
        section("9 还原")
        st, _ = call(VIP_BASE, "PUT", "/api/backup/config",
                     {"enabled": bool(original.get("enabled")), "schedule": original.get("schedule") or ""})
        _, cfg = call(VIP_BASE, "GET", "/api/backup/config")
        check("9a", "备份配置还原为原样", st == 200 and isinstance(cfg, dict)
              and cfg.get("enabled") == original.get("enabled") and (cfg.get("schedule") or "") == (original.get("schedule") or ""),
              str(cfg)[:120])
        # 先删每台本轮新打的标记：后台循环不看备份开关，有标记就会再送一整份
        for b in NODES:
            for m in set(source_markers(b)) - before[b]:
                ssh(host(b), f"zfs destroy -r {pool_of(host(b))}/nd@{m} 2>/dev/null || true")
        for b in with_pool:
            if not preexisting[b]:
                ssh(host(b), f"zfs destroy -r {BACKUP[b]}/ndiskless 2>/dev/null || true")
        left = {host(b): copy_markers(b) for b in with_pool if not preexisting[b]}
        check("9b", "本套件留下的备份副本已删除", all(not v for v in left.values()), str(left))
    return summary()


BACKUP = {}

if __name__ == "__main__":
    sys.exit(main())
