#!/usr/bin/env python3
"""高可用的自愈与人工救场专项。

`ha_matrix` 测的是「故障发生后集群自己切过去」，`failover_matrix` 测的是「用各种
方式弄坏都能切」。这里补的是第三类：**集群已经处在一个不该出现的状态里，它能不能
自己爬出来**，以及爬不出来时运维手里那根杠杆好不好使。

    1 持 VIP 却是备机 → 自行激活
      keepalived 的 notify 只在边沿触发一次。那一次激活若失败（DB 副本正被复制流
      覆盖、探针超时、磁盘打嗝），就再没人重试，节点会顶着 VIP 当只读备机：没有
      写者、客户机开不了机，而且一切看起来都"活着"。真机上撞见过这个死锁。

    2 不持 VIP 却是主机 → 退位
      一台没有 VIP 的主机是隐形的双写源：它照样接受写、照样渲染 dnsmasq，只是没人
      经 VIP 找得到它。但它也不能一看到自己没 VIP 就退——VRRP 会抖动。

    3 强制激活
      曾经的手动杠杆。多数派拿掉之后 force 什么也跳不过，参数已废弃——这里保留
      带 force 的调用，是为了钉住「带上它也没有特权」。

    7 写入者活着但不健康 → 备机追平后接任
    8 写入者的 keepalived 停了 → 收敛到一台主机
      拒绝接任要有上限：让出一次，虚 IP 又回来就直接从写入者的节点地址追平、
      请它交出主机身份；追不平也接任，并留下记录（issue 062）。

    ND_NODES / ND_VIP / ND_PASSWORD / ND_SSH_PASSWORD / ND_CLUSTER_TOKEN / ND_SSH_USER

破坏性：会临时挪动 VIP、隔离节点。结束时恢复。只在演练环境跑。
"""
import calendar
import json
import sys
import time

from ndcluster import (NODES, VIP, anon_get, call, check, host, node_call, node_id_of,
                       section, skip, ssh, ssh_out, summary, wait)


def role_of(base):
    st, body = node_call(base, "GET", "/internal/ha/status")
    return body if st == 200 and isinstance(body, dict) else {}


def holds_vip(node_host):
    ok, out = ssh_out(node_host, f"ip -4 -br a | grep -c '{VIP}/' || true")
    return ok and out.strip().isdigit() and int(out.strip()) > 0


def iface_of(node_host):
    ok, out = ssh_out(node_host, "ip -4 -o route get 1.1.1.1 2>/dev/null | awk '{print $5}'")
    return out.strip() if ok else "ens33"


def pick_standby(exclude=()):
    """挑一台此刻真的在线、且自称备机的节点。

    原来是 next(b for b in NODES if b != act)——两台时它只有一个候选所以总是对的；
    三台之后它可能挑中一台刚被前一段停掉的机器，于是后面每一步都是连接被拒，
    而红出来的信息与产品无关。
    """
    for b in NODES:
        if b in exclude:
            continue
        if role_of(b).get("role") == "standby":
            return b
    return None


def settled(seconds=180):
    """唯一主机 + VIP 有人持 + 经 VIP 可达。"""
    def ok():
        actives = [b for b in NODES if role_of(b).get("role") == "active"]
        if len(actives) != 1:
            return None
        if not any(holds_vip(host(b)) for b in NODES):
            return None
        st, _ = anon_get(f"http://{VIP}:8080", "/healthz", timeout=5)
        return actives[0] if st == 200 else None
    return wait(ok, seconds, step=5)


def main():
    if len(NODES) < 2 or not VIP:
        skip("0", "自愈专项", "需要至少两个 keepalived 节点与 ND_VIP")
        return summary()

    section("0 前置：集群处于稳态")
    act = settled(240)
    check("0a", "开始前唯一主机 + VIP 可达", bool(act), act or "未收敛")
    if not act:
        return summary()
    standby = pick_standby(exclude=(act,))
    check("0b", "有一台备机", bool(standby), host(standby) if standby else "")
    if not standby:
        return summary()
    print(f"    主机={host(act)} 备机={host(standby)}")

    section("1 主机挂了、VIP 到了备机却没收到通知 → 自行激活")
    # 复现「keepalived 把 VIP 交给备机，但 notify 丢了」。
    # 前提是主机已不能服务，否则备机拒绝接任才是对的，所以先停原主机服务。
    # VIP 必须只在备机上：两台同时持有是分区，栅栏会正确拦住，测的就成了另一回事。
    # 所以先停全部节点的 keepalived 再手工搬：漏停的节点会自认 MASTER 抢走 VIP，
    # 重启后启动核对又把自愈的那台降回备机，造成假红。
    sb_host, sb_if = host(standby), iface_of(host(standby))
    ac_host, ac_if = host(act), iface_of(host(act))
    for b in NODES:
        ssh(host(b), "systemctl stop keepalived 2>/dev/null || true")
    ssh(ac_host, "systemctl stop ndiskless")
    ssh(ac_host, f"ip addr del {VIP}/24 dev {ac_if} 2>/dev/null || true")
    ssh(sb_host, f"ip addr add {VIP}/24 dev {sb_if} 2>/dev/null || true")
    try:
        got = wait(lambda: role_of(standby).get("role") == "active" or None, 150, step=5)
        check("1a", "主机挂了、备机发现自己持有 VIP 后自行激活", bool(got), str(role_of(standby)))
    finally:
        # 交还给 keepalived；原主机回来时启动核对会以备机身份起来。
        ssh(sb_host, f"ip addr del {VIP}/24 dev {sb_if} 2>/dev/null || true")
        for b in NODES:
            ssh(host(b), "systemctl start keepalived 2>/dev/null || true")
        ssh(ac_host, "systemctl start ndiskless")
    back = settled(240)
    check("1b", "清理后集群回到唯一主机", bool(back), back or "未收敛")

    section("1.5 两台同时持有 VIP（分区）→ 备机不得抢")
    # 两台同时持有 VIP 说明 VRRP 断了而不是主机死了，此时备机激活就是双写。
    # 栅栏不能只看 epoch：两任主机共用 epoch 时判据恒假。
    act15 = settled(180)
    if not act15:
        skip("1.5a", "分区下不得抢主", "集群未回稳，跳过")
    else:
        peer15 = pick_standby(exclude=(act15,))
        p_host, p_if = host(peer15), iface_of(host(peer15))
        ssh(p_host, f"ip addr add {VIP}/24 dev {p_if} 2>/dev/null || true")
        time.sleep(30)
        st, body = node_call(peer15, "POST", "/internal/ha/activate?force=1")
        check("1.5a", "对端仍持有 VIP 时激活被拒（旧的 force 参数不给任何特权）", st == 409,
              f"HTTP {st} {str(body)[:100]}")
        check("1.5b", "被拒后仍是备机", role_of(peer15).get("role") == "standby",
              str(role_of(peer15)))
        ssh(p_host, f"ip addr del {VIP}/24 dev {p_if} 2>/dev/null || true")

    section("2 不持 VIP 却是主机 → 退位")
    act2 = back or settled(120)
    if not act2:
        skip("2a", "退位自愈", "集群未回稳，跳过")
    else:
        a_host, a_if = host(act2), iface_of(host(act2))
        other = pick_standby(exclude=(act2,))
        # keepalived 多半很快补回 VIP，这里只验证补回前不会退位、最终仍是唯一主机。
        ssh(a_host, f"ip addr del {VIP}/24 dev {a_if} 2>/dev/null || true")
        time.sleep(20)
        still = role_of(act2).get("role")
        check("2a", "短暂失去 VIP 不会立刻退位（VRRP 会抖动）",
              still in ("active", "standby"), f"role={still}")
        end = settled(240)
        check("2b", "最终收敛回唯一主机", bool(end), end or "未收敛")

    section("3 对端仍在服务时不得激活（force 参数已废弃，带上也没有特权）")
    act3 = settled(120)
    if not act3:
        skip("3a", "强制激活", "集群未回稳，跳过")
    else:
        peer = pick_standby(exclude=(act3,))
        # 对端在服务时必须被拒，否则一个手动操作就能造出双写；带上 ?force=1 是为钉住它没有特权。
        st, body = node_call(peer, "POST", "/internal/ha/activate?force=1")
        check("3a", "对端仍在服务时激活被拒绝（force 无特权）", st == 409,
              f"HTTP {st} {str(body)[:90]}")
        check("3b", "被拒之后角色没变", role_of(peer).get("role") == "standby",
              str(role_of(peer)))

    section("4 旧主崩溃后回归：不得带着旧角色开闸")
    # 角色标记只记得「崩溃前我是主机」。旧主重启若照标记直接开写闸门，会和接管方
    # 出现几十秒双写，并各起一个 dnsmasq。启动时必须先和现实核对。
    act4 = settled(180)
    if not act4:
        skip("4a", "旧主回归", "集群未回稳，跳过")
    else:
        a4_host = host(act4)
        other4 = pick_standby(exclude=(act4,))
        ssh(a4_host, "systemctl stop ndiskless")

        def took_over():
            """接管的是「另外某一台」，不是某一台指定的机器。
            三台都参选 VRRP 之后，VIP 落到哪台由优先级决定；写死 other4 时，
            接管落到第三台就红——红的是用例的假设，不是产品。"""
            for b in NODES:
                if b != act4 and role_of(b).get("role") == "active":
                    return b
            return None

        took = wait(took_over, 180, step=5)
        check("4a", "对端接管", bool(took), f"接管方={host(took) if took else '无'}")
        ssh(a4_host, "systemctl start ndiskless")
        # 进程还没监听时 role_of 返回空，不能当成「不是备机」。
        role = None
        for _ in range(24):
            time.sleep(5)
            r = role_of(act4)
            if r.get("role"):
                role = r
                break
        check("4b", "旧主回归后立刻就是备机（不留双写窗口）",
              bool(role) and role.get("role") == "standby", str(role))
        check("4c", "集群仍是唯一主机", bool(settled(240)))

    section("5 把切换发给备机时，要告诉运维该去哪台")
    # 「指错机器」和「同步未完成」处置相反，不能都回 409。指错机器时写闸门先回 503
    # 并给出该去的地址，钉住的是这个行为。
    act5 = settled(180)
    if not act5:
        skip("5a", "计划切换", "集群未回稳，跳过")
    else:
        sb5 = pick_standby(exclude=(act5,))
        st, body = call(sb5, "POST", "/api/ha/planned-switch")
        check("5a", "备机拒绝并给出该去的地址", st == 503 and "http" in str(body),
              f"HTTP {st} {str(body)[:110]}")
        check("5b", "拒绝之后备机没有变成主机", role_of(sb5).get("role") == "standby",
              str(role_of(sb5)))

    section("7 写入者活着但不健康：备机从它的节点地址追平后接任")
    # 维护标记让 /healthz 回 503，keepalived 交出 VIP，但进程仍在打新标记。
    # 备机只能从 VIP 拉复制，若落后就拒绝接任，VIP 会在备机间来回漂，必须改为从节点地址追平。
    act7 = settled(180)
    if not act7:
        skip("7a", "不健康写入者的交接", "集群未回稳，跳过")
    else:
        a7 = host(act7)
        t7 = time.time()
        ssh(a7, "touch /run/ndiskless.maintenance")
        try:
            def other_took_over():
                for b in NODES:
                    if b != act7 and role_of(b).get("role") == "active" and holds_vip(host(b)):
                        return b
                return None
            took = wait(other_took_over, 360, step=5)
            check("7a", "另一台在几分钟内接任并持有虚 IP", bool(took), f"接管方={host(took) if took else '无'}")
            old = wait(lambda: role_of(act7).get("role") == "standby" or None, 120, step=5)
            check("7b", "原写入者已退位为备机（没有两个主机）", bool(old), str(role_of(act7)))
            if took:
                _, log = ssh_out(host(took), "journalctl -u ndiskless --since '-8min' -o cat | "
                                 "grep -c 'caught up before taking over' || true")
                _, report = ssh_out(host(took), "test -f /var/lib/ndiskless/ha-takeover.json && "
                                    "cat /var/lib/ndiskless/ha-takeover.json || echo none")
                # 记录会一直保留，只认本段开始之后写下的。
                fresh = False
                try:
                    at = json.loads(report)["at"][:19]
                    fresh = calendar.timegm(time.strptime(at, "%Y-%m-%dT%H:%M:%S")) >= int(t7)
                except (ValueError, KeyError, TypeError):
                    pass
                check("7c", "接任前追平了：没有留下「未追平」的接任记录",
                      not fresh, f"追平日志 {log.strip()} 条；记录 {report.strip()[:120]}")
        finally:
            ssh(a7, "rm -f /run/ndiskless.maintenance")
        check("7d", "撤掉维护标记后集群是唯一主机", bool(settled(240)))

    section("8 写入者的 keepalived 停了：虚 IP 不再来回漂，收敛到一台主机")
    # 写入者健康但拿不回 VIP。备机第一次会让出；十分钟内 VIP 再回到手上时，改为请写入者交出主机身份。
    act8 = settled(240)
    if not act8:
        skip("8a", "keepalived 停止后的收敛", "集群未回稳，跳过")
    else:
        a8 = host(act8)
        ssh(a8, "systemctl stop keepalived")
        try:
            def converged_elsewhere():
                actives = [b for b in NODES if role_of(b).get("role") == "active"]
                if len(actives) == 1 and actives[0] != act8 and holds_vip(host(actives[0])):
                    return actives[0]
                return None
            took = wait(converged_elsewhere, 480, step=5)
            check("8a", "收敛到另一台主机，且它持有虚 IP", bool(took),
                  f"接管方={host(took) if took else '无'}；各台角色 "
                  + str({host(b): role_of(b).get('role') for b in NODES}))
            if took:
                _, prepared = ssh_out(a8, "journalctl -u ndiskless --since '-10min' -o cat | "
                                      "grep -c 'handover prepared' || true")
                check("8b", "原写入者是交接出去的（关了写入、打了最后一轮）",
                      prepared.strip().isdigit() and int(prepared.strip()) >= 1, f"{prepared.strip()} 次")
                st, _ = anon_get(f"http://{VIP}:8080", "/healthz", timeout=5)
                check("8c", "经虚 IP 可达", st == 200, f"HTTP {st}")
        finally:
            ssh(a8, "systemctl start keepalived")
        check("8d", "恢复 keepalived 后集群是唯一主机", bool(settled(240)))

    section("6 收尾")
    fin = settled(300)
    check("6a", "结束时集群处于稳态", bool(fin), fin or "未收敛——需要人工检查")
    return summary()


if __name__ == "__main__":
    sys.exit(main())
