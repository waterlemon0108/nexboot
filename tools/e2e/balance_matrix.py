#!/usr/bin/env python3
"""客户机负载均衡矩阵：自动均衡、显式绑定、粘性、节点故障时的重放置。

放置是分层的，越靠前越硬：
    超管粘性 > 分组显式绑定 > 上次落点 > 最闲的节点（自动均衡）
每一层都得单独验，因为它们互相压制：一条绑定会让均衡看起来「失效」，而一次
粘性会让绑定看起来「没生效」——现场分不清是策略在起作用还是出了缺陷。

    ND_NODES / ND_VIP / ND_PASSWORD / ND_CLUSTER_TOKEN / ND_POOL

自建 bal-e2e-* 分组与终端，结束时清理。
"""
import re
import sys
import time
import urllib.parse

from ndcluster import (NODES, POOL, call, check, entry_base, host, node_id_of, pool_of,
                       roster, section, skip, ssh, ssh_out, summary, wait)

GROUP = "bal-e2e"
PREFIX = "bal-e2e-"


def main():
    if not NODES:
        raise SystemExit("ND_NODES 必填")
    n = len(NODES)
    A = NODES[0]
    ids = [node_id_of(b) for b in NODES]
    by_id = {node_id_of(b): b for b in NODES}
    base = entry_base()

    tmpl = _template_group(base)
    image, config = tmpl.get("SystemImageID"), tmpl.get("SystemConfigID")
    if not (image and config):
        skip("0", "负载均衡", "没有可复用的镜像/配置，跳过整个矩阵")
        return summary()

    _sweep_leftovers(base)
    group_id, made = None, []
    try:
        section(f"0 前置（{n} 节点）")
        check("0a", "拿到可复用的镜像与配置", True, f"{image}/{config}")
        online = [x for x in roster(base).values() if x.get("online")]
        check("0b", f"{n} 台都在线", len(online) == n, str([x["ip"] for x in online]))

        # 放置计数是决策数而非实际会话数，会被前面矩阵开过的机器打偏，导致「摊开」「粘住」假红。
        # 产品会用实际会话对账（ReconcileLoad），所以先等残留克隆清空；等不到也照跑，但先提示。
        settled = wait(lambda: all(
            int(ssh(h, f"zfs list -H -o name -r {pool_of(h)}/run 2>/dev/null | grep -c CLIENT- || true").stdout.strip() or 0) == 0
            for h in [host(b) for b in NODES]), 120, 5)
        if settled is None:
            print("    注意：仍有残留的客户机克隆，放置计数可能偏斜")

        section("1 自动均衡：不绑定时摊开")
        group_id = _make_group(base, GROUP, tmpl, node=None)
        check("1a", "分组默认不绑定存储节点", group_id is not None, str(group_id))
        if group_id is None:
            # 没有分组无从测试，继续跑只会产生误导性的失败。
            skip("1-5", "放置相关全部检查", "建组失败，见上一行")
            return summary()
        # 每个节点两台机，数量取节点数整倍数，才能区分真正摊平和碰巧。
        count = n * 2
        portal = _portal_to_node(base)
        norm = lambda t: portal.get(t, t)
        placed = {}
        for i in range(count):
            mac = f"0e:2e:c1:0b:00:{i:02x}"
            tid = _register(base, group_id, mac, f"{PREFIX}{i}")
            if tid:
                made.append(tid)
            placed[mac] = norm(_boot_target(base, mac))
            time.sleep(0.4)
        seen = {}
        for mac, tgt in placed.items():
            seen.setdefault(tgt, []).append(mac)
        check("1b", f"{count} 台全部拿到落点", all(placed.values()),
              str({m: t for m, t in placed.items() if not t}))
        if n > 1:
            check("1c", f"落点覆盖到了 {n} 个不同地址", len(seen) == n,
                  {k: len(v) for k, v in seen.items()})
            spread = sorted(len(v) for v in seen.values())
            # 摊得均不均：最多的一台和最少的一台不该差到一倍以上
            check("1d", "各节点承载数量接近", spread[-1] - spread[0] <= 1, str(spread))
        else:
            check("1c", "单机时全部落在本机", len(seen) == 1, str(list(seen)))

        section("2 上次落点粘住：同一台机器重复开机不换节点")
        mac0 = list(placed)[0]
        again = [norm(_boot_target(base, mac0)) for _ in range(3)]
        check("2a", "连开三次都落回同一节点", len(set(again)) == 1 and again[0] == placed[mac0],
              f"{placed[mac0]} -> {again}")

        if n < 2:
            skip("3", "显式绑定", "只有一台，无处可绑")
            skip("4", "节点故障重放置", "只有一台")
            return summary()

        section("3 显式绑定压过自动均衡")
        target = ids[-1]                     # 绑到最后一台
        st, body = _set_group_node(base, group_id, target)
        check("3a", "分组绑定到指定节点", st in (200, 204), f"HTTP {st} {str(body)[:120]}")
        time.sleep(1)
        # 新终端必须落到绑定的那台，不管它当时闲不闲
        pinned = []
        for i in range(count, count + 2):
            mac = f"0e:2e:c1:0b:00:{i:02x}"
            tid = _register(base, group_id, mac, f"{PREFIX}{i}")
            if tid:
                made.append(tid)
            pinned.append(norm(_boot_target(base, mac)))
        want_host = host(by_id[target])
        check("3b", "绑定后的新终端都落到那一台",
              all(t == want_host for t in pinned), f"want {want_host}, got {pinned}")

        section("4 解绑后回到自动均衡")
        st, body = _set_group_node(base, group_id, "")
        check("4a", "解除绑定", st in (200, 204), f"HTTP {st} {str(body)[:120]}")
        time.sleep(1)
        fresh = []
        for i in range(count + 2, count + 2 + n):
            mac = f"0e:2e:c1:0b:00:{i:02x}"
            tid = _register(base, group_id, mac, f"{PREFIX}{i}")
            if tid:
                made.append(tid)
            fresh.append(norm(_boot_target(base, mac)))
        check("4b", "解绑后不再全部挤在那一台", len({t for t in fresh if t}) > 1 or n == 1,
              str(fresh))

        section("5 节点掉线：它的客户机改投健康节点")
        # 停一台非主机：停持 VIP 的那台就成了故障切换演练（归 ha_matrix / failover_matrix），这里只测重放置。
        r_now = roster(base)
        cands = [(b, i) for b, i in zip(NODES, ids) if (r_now.get(i) or {}).get("ha_state") != "active"]
        if not cands:
            skip("5", "掉线重放置", "找不到非主机的节点可停")
            return summary()
        victim, vid = cands[-1]
        vhost = host(victim)
        # 找一台原本落在 victim 上的终端
        victim_macs = [m for m, t in placed.items() if t == vhost]
        if not victim_macs:
            skip("5a", "掉线重放置", f"没有终端原本落在 {vhost} 上")
        else:
            ssh(vhost, "systemctl stop ndiskless")
            try:
                gone = wait(lambda: not (roster(base).get(vid) or {}).get("online", True), 180, 10)
                check("5a", "花名册把它标为离线", bool(gone),
                      "" if gone else f"{vhost} 在 180s 内仍算在线")
                mac = victim_macs[0]
                moved = wait(lambda: (lambda t: t if t and t != vhost else None)(norm(_boot_target(base, mac))), 90, 5)
                check("5b", "它的客户机改投健康节点", bool(moved),
                      moved or f"仍指向 {vhost}")
            finally:
                ssh(vhost, "systemctl start ndiskless")
            back = wait(lambda: (roster(base).get(vid) or {}).get("online"), 180, 10)
            check("5c", "恢复后重新在线", bool(back),
                  "" if back else f"{vhost} 未在 180s 内回到在线")

        return summary()
    finally:
        for tid in made:
            call(base, "DELETE", f"/api/terminals/{tid}")
        if group_id:
            call(base, "DELETE", f"/api/groups/{group_id}")


def _sweep_leftovers(base):
    """先清掉上一轮留下的同名分组与终端。

    上一轮可能被中断（超时、人为 kill、机器重启），清理的 finally 没跑到。留着的话
    这一轮建组会撞上「分组已存在」，group_id 拿不到，后面每一项都跟着失败——而报出来
    的是「落点没摊开」「分组不存在」这类指向别处的错，真正的原因只在第一行。
    """
    st, body = call(base, "GET", "/api/terminals")
    for t in (body.get("items") or []) if st == 200 else []:
        if str(t.get("Name", "")).startswith(PREFIX):
            call(base, "DELETE", f"/api/terminals/{t['ID']}")
    st, body = call(base, "GET", "/api/groups")
    for g in (body.get("items") or []) if st == 200 else []:
        if g.get("Name") == GROUP:
            for t in (call(base, "GET", f"/api/terminals?group_id={g['ID']}")[1].get("items") or []):
                call(base, "DELETE", f"/api/terminals/{t['ID']}")
            call(base, "DELETE", f"/api/groups/{g['ID']}")


def _template_group(base):
    """建组要用的网络参数与镜像。

    网络参数优先从既有分组借；写死网段的话，换一套环境就会被产品（正确地）以
    「不在客户机网卡的网段内」拒绝，后面每一项都跟着连锁失败。
    但分组不是总有：清库重装之后，镜像和配置会从池里恢复，分组不会——它只存在
    于库里。这时退回按本机地址推导网段，那正是客户机网卡所在的那一段。
    """
    st, body = call(base, "GET", "/api/groups")
    items = (body.get("items") or []) if st == 200 else []
    if items:
        return items[0]
    img, cfg = _any_image(base)
    if not img:
        return {}
    head = host(base).rsplit(".", 1)[0]
    return {"StartIP": f"{head}.1", "Netmask": "255.255.255.0", "Gateway": "",
            "SystemImageID": img, "SystemConfigID": cfg}


def _any_image(base):
    """任一可用的镜像与它的一个配置。清库后这两样由启动时的目录恢复带回来。"""
    st, body = call(base, "GET", "/api/images")
    for img in (body.get("items") or []) if st == 200 else []:
        st2, cfgs = call(base, "GET", f"/api/images/{img['ID']}/configs")
        for c in (cfgs.get("items") or []) if st2 == 200 else []:
            return img["ID"], c["ID"]
    return None, None


def _make_group(base, name, tmpl, node):
    head = (tmpl.get("StartIP") or "192.168.10.100").rsplit(".", 1)[0]
    body = {"name": name, "start_ip": f"{head}.150", "client_max": 40,
            "gateway": tmpl.get("Gateway", ""), "netmask": tmpl.get("Netmask", "255.255.255.0"),
            "dns1": "114.114.114.114", "dns2": "223.5.5.5",
            "system_image_id": tmpl.get("SystemImageID"),
            "system_config_id": tmpl.get("SystemConfigID")}
    if node:
        body["storage_server_id"] = node
    st, res = call(base, "POST", "/api/groups", body)
    if st in (200, 201):
        return res.get("ID") or res.get("id")
    print("    建组失败:", st, str(res)[:220])
    return None


def _set_group_node(base, group_id, node):
    """改绑定要把整份分组回填。只发一个字段会被 400 挡下——更新走的是整体校验，
    缺了网段、镜像这些必填项就不成立，这跟界面上按「保存」提交整表是一致的。"""
    st, g = call(base, "GET", f"/api/groups/{group_id}")
    if st != 200:
        return st, g
    # is_default 也要带回：清库后这是唯一分组即默认分组，丢掉会被「必须保留一个默认分组」拒绝。
    body = {"name": g.get("Name"), "start_ip": g.get("StartIP"),
            "client_max": g.get("ClientMax"), "gateway": g.get("Gateway", ""),
            "netmask": g.get("Netmask", ""), "dns1": g.get("DNS1", ""), "dns2": g.get("DNS2", ""),
            "is_default": bool(g.get("IsDefault")),
            "system_image_id": g.get("SystemImageID"), "system_config_id": g.get("SystemConfigID"),
            "storage_server_id": node}
    return call(base, "PUT", f"/api/groups/{group_id}", body)


def _register(base, group_id, mac, name):
    st, res = call(base, "POST", "/api/terminals",
                   {"name": name, "mac": mac, "group_id": group_id})
    if st in (200, 201):
        return res.get("ID") or res.get("id")
    return None


def _boot_target(base, mac):
    """这台客户机的盘实际在哪台服务器上，取自 sanhook 的 portal 地址。

    只取地址段：portal 写作 `iscsi:<ip>:::0:<iqn>`，而 iqn 里带 MAC，连着一起
    取的话每台客户机都会得到一个互不相同的「落点」，摊没摊开就永远看不出来。

    带登录会话请求，尽管客户机自己是匿名走这条路的。/boot 现在核对来源地址：
    只有那台机器本人、或者已登录的管理员，才answers得到它的启动配置。矩阵跑在
    另一台机器上，代问六台客户机的落点——正是这道门要拦的事，匿名过不去（实测
    六台落点全空）。用登录会话是这里唯一诚实的办法：它就是运维在控制台上查看
    「这台机器落在哪」的那条路径。客户机自己那条路径由 boot_matrix 覆盖。"""
    st, body = call(base, "GET", f"/boot?mac={urllib.parse.quote(mac)}", timeout=60)
    if st != 200:
        return ""
    if not isinstance(body, str):
        return ""
    m = re.search(r"iscsi:([0-9a-fA-F.:]+?):::", body)
    return m.group(1) if m else ""


def _portal_to_node(base):
    """portal 地址 → 节点 IP。主机对外报的是 VIP，备机报自己的地址；不折算的话
    「主机」和「VIP」会被当成两个不同的落点，均衡的判断就废了。"""
    m = {}
    for nid, n in roster(base).items():
        ip = n.get("ip")
        if ip:
            m[ip] = ip
        p = n.get("portal_ip")
        if p:
            m[p] = ip
    return m


if __name__ == "__main__":
    sys.exit(main())
