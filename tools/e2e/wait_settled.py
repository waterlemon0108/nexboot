#!/usr/bin/env python3
"""等集群静下来：三台健康、恰好一个写入者、目录追平。只观察，不清理。

套件之间用它。上一套刚把角色折腾完（杀服务、切换、断电），复制还在恢复，下一套
一上来就比对两侧目录，等的是一个还没开始的状态——等多久都不对。

真机实证：ha_matrix 在复位后单独跑，1a 一秒通过、整套 20/20；同一套用例排在
all.sh 第 5 位（前面四套刚做完大量切换），1a 等满 420 秒仍不一致。差别只在起点。

复位（reset.py）是「清干净」，这个是「等安静」，两件事。整轮开头用前者，套件
之间用后者——清理有代价且会动数据，不该每套都来一遍。
"""
import shlex
import sys
import time

sys.path.insert(0, ".")
from ndcluster import NODES, VIP, host, pool_of, ssh_out  # noqa: E402

if not VIP:
    raise SystemExit("请先设置 ND_VIP 为测试集群的虚 IP，再运行本脚本。")

DEADLINE = int(sys.argv[1]) if len(sys.argv) > 1 else 600


def snapshot():
    """(每台健康吗, 谁持有 VIP, 每台的最新标记)"""
    health, vip, marks = {}, [], {}
    born.clear()
    for b in NODES:
        h = host(b)
        health[h] = '"status":"ok"' in ssh_out(
            h, "curl -s -m 5 http://127.0.0.1:8080/healthz", timeout=30)[1]
        if ssh_out(h, f"ip -4 -o addr show | awk -v vip={shlex.quote(VIP)} "
                      "'{split($4, ip, \"/\"); if (ip[1] == vip) found=1} END {print found+0}'",
                   timeout=20)[1] == "1":
            vip.append(h)
        # 根上最近两轮：写入者的「上一轮」用来判备机是否只差一轮
        rows = ssh_out(h, "zfs list -H -p -o name,creation -s creation -t snapshot -d 1 %s/nd 2>/dev/null "
                          "| grep @rep- | tail -2" % pool_of(h), timeout=30)[1].splitlines()
        marks[h] = [r.split()[0].split("@")[-1] for r in rows if r.strip()]
        born[h] = {r.split()[0].split("@")[-1]: int(r.split()[1]) for r in rows if len(r.split()) > 1}
    return health, vip, marks


born = {}  # 节点 -> {标记: 创建时刻}


def in_step(vip, marks):
    """每台都追到了写入者最近两轮之一。

    写入者每分钟打一轮，备机按自己的节奏每分钟拉一次，两者相位由各自的启动时刻决定。
    相位不利时备机一分钟里有五十多秒落后一轮——这是正常状态，不是没追上。真机
    2026-09-29：写入者每分钟第 10 秒打标记、.3 第 7 秒拉取，三台完全相同的窗口只有
    3 秒，「三台同标记」连续两次等满 600 秒。
    """
    recent = set(marks.get(vip[0]) or [])
    # 开始等待之后才打的标记里装着上一套最后的改动（如收尾时的删除），落后一轮的备机不算追平，
    # 否则下一套停写入者时接任者会缺这一轮。
    fresh = {m for m, t in born.get(vip[0], {}).items() if t >= START}
    if fresh:
        recent = fresh
    return bool(recent) and all(m and m[-1] in recent for m in marks.values())


t0 = time.time()
# 先等写入者打一轮标记（每分钟一轮）：上一套刚做的改动要先进标记，才谈得上追平。
START = int(t0)
time.sleep(75)
last = None
while time.time() - t0 < DEADLINE:
    health, vip, marks = snapshot()
    last = (health, vip, marks)
    settled = all(health.values()) and len(vip) == 1 and in_step(vip, marks)
    if settled:
        print("集群已静 %.0fs：写入者 %s，各台最多落后一轮 %s"
              % (time.time() - t0, vip[0], {h: (m[-1] if m else "") for h, m in marks.items()}), flush=True)
        sys.exit(0)
    time.sleep(10)

health, vip, marks = last or snapshot()
print("%ds 内没等到集群静下来：不健康=%s 持VIP=%s 标记=%s"
      % (DEADLINE, [h for h, ok in health.items() if not ok], vip, marks), flush=True)
print("下一套会从一个还在恢复的起点开跑，红出来的多半不是它要测的东西。", flush=True)
sys.exit(1)
