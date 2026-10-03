#!/usr/bin/env python3
"""集群管理矩阵：跨节点看池、跨节点操作、池身份、内部端点的认证边界。

按 ND_NODES 的节点数自适应：单机跑降级断言（集群端点必须仍然答得体面），
两台及以上加跑跨节点部分。覆盖的是单元测试结构上够不到的东西——目录库整体
复制之后，谁拥有哪个池、谁能对谁下命令、点错节点会不会真的动手。

    ND_NODES=http://10.0.0.3:8080,http://10.0.0.4:8080,...
    ND_CLUSTER_TOKEN / ND_PASSWORD / ND_POOL / ND_SSH_USER

只读为主。写的部分有两处，都是有意为之：往对端下发一次无害服务重启（zfs-zed），
以及第 11 段重启一台**备机**的 ndiskless——「本机身份活过重启」只能这么验，而这正是
一个 P0 藏身的地方（池名从不落盘，重启后退回集群级旧名字）。写入者不碰。
"""
import sys
import time

import json

from ndcluster import (NODES, POOL, TOKEN, anon_get, anon_post, call, check,
                       entry_base, has_cluster_token,
                       host, node_call, node_id_of, own_pools, pool_of, roster,
                       section, skip, ssh, ssh_out, summary)


def main():
    if not NODES:
        raise SystemExit("ND_NODES 必填")
    n = len(NODES)
    A = NODES[0]
    # 写操作一律走 VIP（永远指向当前写入节点）；拿 NODES[0] 当写入方赌输了会是一片 503「本节点为备机」。
    W = entry_base()
    ids = {base: node_id_of(base) for base in NODES}

    section(f"0 拓扑：{n} 节点")
    check("0a", "每台都报得出自己的身份", all(ids.values()), str(list(ids.values())))
    r = roster(A)
    check("0b", f"花名册有 {n} 台", len(r) == n, f"{len(r)} 台：{sorted(r)}")
    for base, nid in ids.items():
        check(f"0c-{host(base)}", "自报身份在花名册里", nid in r, nid)

    section("1 内部端点的认证边界")
    # 这些端点让一台机器对另一台下命令，没有令牌就能访问等于把控制面向整个网段敞开。
    for base in NODES:
        st, _ = anon_get(base, "/internal/node/pools")
        check(f"1a-{host(base)}", "无令牌读池被拒", st == 401, f"HTTP {st}")
        st, _ = anon_get(base, "/internal/node/disks")
        check(f"1b-{host(base)}", "无令牌读盘被拒", st == 401, f"HTTP {st}")
    # 带令牌必须能过：内部路由若被挂进需要登录的那组，单测照样绿，真机跨节点调用全 401。
    tokened = {base: has_cluster_token(base) for base in NODES}
    for base in NODES:
        if not tokened[base]:
            # 单机装机本就不配集群令牌，此时内部端点对所有人关门是对的。
            skip(f"1c-{host(base)}", "带集群令牌可读", "这台没有配集群令牌（单机形态的正常状态）")
            continue
        st, body = node_call(base, "GET", "/internal/node/pools")
        check(f"1c-{host(base)}", "带集群令牌可读（不经登录会话）",
              st == 200 and isinstance(body, dict) and "items" in body, f"HTTP {st}")

    section("2 池身份：每台认领自己的池")
    # 目录库整体复制，三台共用同一张 pools 表；池 ID 只由名字派生时同名池会压成一行，其余节点就此没有存储。
    owned = {}
    for base in NODES:
        got, items = own_pools(base, ids[base])
        if not got:
            check(f"2a-{host(base)}", "读得到本机的池", False, "内部端点与会话接口都问不出来")
            continue
        owned[base] = items
        # 问这台机器自己的池名，别用 ND_POOL 套：名字在别的机器上不存在时这条会静默 skip，什么都没测。
        mypool = pool_of(host(base))
        got, real = ssh_out(host(base), f"zpool list -H -o name {mypool} 2>/dev/null")
        if not got:
            check(f"2a-{host(base)}", "读得到本机的池状态", False, f"SSH 失败：{real}")
            continue
        if not real:
            skip(f"2a-{host(base)}", "本机确有数据池", f"{mypool} 未导入，跳过归属核对")
            continue
        mine = [p for p in items if p.get("Name") == mypool]
        check(f"2a-{host(base)}", "本机的数据池在自己名下", len(mine) == 1,
              f"zpool 有 {real}，API 报 {[p.get('Name') for p in items]}")
        if mine:
            check(f"2b-{host(base)}", "池 ID 带上了节点",
                  ids[base] and ids[base] in mine[0].get("ID", ""),
                  mine[0].get("ID"))
            check(f"2c-{host(base)}", "ServerID 就是本机", mine[0].get("ServerID") == ids[base],
                  mine[0].get("ServerID"))
    if n > 1:
        all_ids = [p.get("ID") for items in owned.values() for p in items]
        check("2d", "各台的池 ID 互不相同（没被折叠成一行）",
              len(all_ids) == len(set(all_ids)), str(all_ids))

    section("3 集群聚合：容量是全集群的，不是本机的")
    st, agg = call(A, "GET", "/api/cluster/pools")
    check("3a", "聚合端点可用", st == 200, f"HTTP {st}")
    if st == 200:
        check("3b", f"列出全部 {n} 个节点", len(agg.get("nodes") or []) == n,
              f"{len(agg.get('nodes') or [])} 个")
        reachable = [x for x in (agg.get("nodes") or []) if x.get("reachable")]
        check("3c", "全部节点都读得到", len(reachable) == n,
              str([(x.get("ip"), x.get("error")) for x in (agg.get("nodes") or []) if not x.get("reachable")]))
        # 合计只统计读得到的，给读不到的节点补 0 会把集群余量说小。
        want = sum(int(p.get("Capacity") or 0) for x in reachable for p in (x.get("pools") or []))
        check("3d", "总容量等于各节点之和", int(agg.get("total_capacity") or 0) == want,
              f"聚合 {agg.get('total_capacity')} vs 逐台相加 {want}")
        check("3e", "恰好一台自称本机", sum(1 for x in (agg.get("nodes") or []) if x.get("is_self")) == 1)

    section("4 按节点看盘：同一个 /dev/sdb 在每台是不同的盘")
    for base in NODES:
        nid = ids[base]
        st, body = call(A, "GET", f"/api/cluster/nodes/{nid}/disks")
        api_paths = sorted(d.get("Path") for d in (body.get("items") or [])) if st == 200 else []
        # zvol 在 lsblk 里也报 TYPE=disk，但不是候选盘；产品排除了它们，断言要同一口径。
        got, raw = ssh_out(host(base),
                           "lsblk -dn -o NAME,TYPE | awk '$2==\"disk\" && $1 !~ /^zd/ {print \"/dev/\"$1}'")
        if not got:
            check(f"4a-{host(base)}", "读得到那台自己的盘", False, f"SSH 失败：{raw}")
            continue
        real = sorted(x for x in raw.split() if x)
        check(f"4a-{host(base)}", "经控制台看到的盘就是那台自己的盘",
              st == 200 and api_paths == real, f"API {api_paths} vs lsblk {real}")

    if n < 2:
        section("5 单机形态：集群面必须降级得体面")
        check("5a", "花名册只有本机", len(r) == 1, str(sorted(r)))
        st, agg = call(A, "GET", "/api/cluster/pools")
        check("5b", "聚合端点在单机也答得出", st == 200 and len(agg.get("nodes") or []) == 1, f"HTTP {st}")
        skip("5c", "跨节点操作", "只有一台，无对端可点名")
        return summary()

    section("5 跨节点服务操作")
    # 挑一个不是写入方的节点操作，否则重启主机自己的服务会打断这一轮。
    peer = next((b for b in NODES if ids[b] != writer_id(ids, W)), NODES[1])
    pid = ids[peer]
    before = _svc_since(peer, "zfs")
    st, _ = call(W, "POST", f"/api/cluster/nodes/{pid}/services/zfs/action", {"action": "restart"})
    check("5a", "从 A 下发重启对端的服务", st in (200, 202), f"HTTP {st}")
    after = _wait_changed(peer, "zfs", before)
    check("5b", "对端的服务确实重启了（起始时刻变了）", bool(after) and after != before,
          f"{before} -> {after}")
    st, body = call(W, "GET", f"/api/cluster/nodes/{pid}/services/zfs/logs?lines=5")
    check("5c", "读得到对端的日志", st == 200 and bool((body or {}).get("logs")), f"HTTP {st}")

    section("7 上传：分片、断点续传、跨节点会话")
    # 镜像几十 G，断点续传是这个入口的全部意义。
    payload = b"ndiskless-upload-probe-" * 512  # ~12KB
    st, sess = call(W, "POST", "/api/images/uploads",
                    {"file_name": "e2e-upload.zfs", "size_bytes": len(payload)})
    check("7a", "建上传会话", st == 201 and bool((sess or {}).get("upload_id")),
          f"HTTP {st} {str(sess)[:90]}")
    if st == 201:
        uid = sess["upload_id"]
        check("7b", "会话编号带上了节点标识", any(uid.startswith(i + "-") for i in ids.values()), uid)
        half = len(payload) // 2
        st1, r1 = patch_chunk(W, uid, 0, payload[:half], len(payload))
        check("7c", "传第一片", st1 == 200 and (r1 or {}).get("received") == half,
              f"HTTP {st1} {str(r1)[:80]}")
        # 断线后浏览器先问「你收到哪了」，这就是续传的起点。
        st2, r2 = call(W, "GET", f"/api/images/uploads/{uid}")
        check("7d", "查得到断点", st2 == 200 and (r2 or {}).get("received") == half,
              f"HTTP {st2} received={(r2 or {}).get('received')}")
        # 偏移跳空必须被拒：写进去会留下空洞，长度对得上、内容是坏的，要到导入体检才炸。
        st3, r3 = patch_chunk(W, uid, half + 99, b"xxxx", len(payload))
        check("7e", "偏移跳空被拒绝，并告知当前位置", st3 == 409 and str(half) in str(r3),
              f"HTTP {st3} {str(r3)[:90]}")
        # 重发已收的分片是网络重试的常态，不能因此把文件写长。
        st4, _ = patch_chunk(W, uid, 0, payload[:half], len(payload))
        st5, r5 = call(W, "GET", f"/api/images/uploads/{uid}")
        check("7f", "重发已收分片不会把文件写长", st4 == 200 and (r5 or {}).get("received") == half,
              f"HTTP {st4} received={(r5 or {}).get('received')}")
        st6, r6 = patch_chunk(W, uid, half, payload[half:], len(payload))
        check("7g", "从断点续完，落地到导入目录",
              st6 == 200 and (r6 or {}).get("complete") and bool((r6 or {}).get("path")),
              f"HTTP {st6} {str(r6)[:110]}")
        # 内容必须一字不差：分片拼错位置的文件大小也是对的。
        if (r6 or {}).get("path"):
            # 文件落在写入节点上，查错机器只会得到空 md5，看起来像内容不一致。
            import hashlib
            wid = writer_id(ids, W)
            whost = next((host(b) for b, i in ids.items() if i == wid), host(A))
            ok, out = ssh_out(whost, f"md5sum {r6['path']} | awk '{{print $1}}'")
            want = hashlib.md5(payload).hexdigest()
            check("7h", "落地文件与原始内容一致（分片拼错位置时大小照样对得上）",
                  ok and out.strip() == want, f"{whost}: {out.strip() or '(读不到)'} vs 原始 {want}")
            ssh(whost, f"rm -f {r6['path']}")

    if n > 1:
        # 切换后会话连同半截文件留在旧主上，新主必须明确说「这不是我的会话」，而不是含糊的「不存在」。
        st, body = call(W, "GET", f"/api/images/uploads/{other_node_id(ids, W)}-deadbeefdeadbeef")
        check("7i", "别的节点的会话编号被认出来并说明主机已切换",
              st == 409 and "主机" in str(body), f"HTTP {st} {str(body)[:100]}")
    st, body = call(W, "POST", "/api/images/uploads", {"file_name": "notes.txt", "size_bytes": 10})
    check("7j", "不支持的后缀在建会话时就拒（不是传完再说）", st == 400, f"HTTP {st}")
    st, body = call(W, "POST", "/api/images/uploads", {"file_name": "../../etc/passwd.zfs", "size_bytes": 10})
    check("7k", "路径穿越被拒", st == 400, f"HTTP {st}")

    section("6 点错节点必须拒绝，而不是照做")
    # 花名册地址会过期：换了 IP 的旧记录可能指向另一台，「销毁 B 的池」会原样在 C 上执行，所以按 node 核对身份。
    st, body = node_call(peer, "DELETE", "/internal/node/pools/whatever?node=definitely-not-this-node")
    check("6a", "被点名的不是自己 → 409", st == 409, f"HTTP {st} {str(body)[:60]}")
    st, _ = node_call(peer, "DELETE", f"/internal/node/pools/no-such-pool?node={pid}")
    check("6b", "点名正确则放行到业务层（池不存在 → 404，不是 409）", st == 404, f"HTTP {st}")
    # 代理这一路也要通：控制台点名的是谁，请求就到谁那里。
    st, body = call(A, "GET", f"/api/cluster/nodes/{pid}/pools/{_pool_id(peer, ids[peer])}")
    check("6c", "经代理读对端的池", st == 200 and (body or {}).get("ServerID") == pid,
          f"HTTP {st} ServerID={(body or {}).get('ServerID')}")

    section("8 摘除节点：不可逆，所以要有护栏")
    # 删错节点没法撤回，两条硬护栏必须挡住。
    st, body = call(W, "DELETE", f"/api/cluster/nodes/{writer_id(ids, W)}")
    check("8a", "不能摘掉正在处理这个请求的那台", st == 409 and "本机" in str(body),
          f"HTTP {st} {str(body)[:90]}")
    # 按规则拒绝要回 4xx 带原文，回不带话的 500 会让运维以为产品坏了。
    check("8b", "拒绝是 409 带原文，不是没头没脑的 500", st == 409, f"HTTP {st}")
    if n > 1:
        peer2 = next((b for b in NODES if ids[b] != writer_id(ids, W)), NODES[1])
        # 护栏挡的是「名下还有池记录」：从没当过主机的备机，池可能不在集群目录里，此时放行是对的，跳过才诚实；
        # 直接断言 409 会误报，还可能真把节点删掉。判据与护栏读同一份数据：写入者目录里 ServerID 指向它的池记录。
        _, all_pools = call(W, "GET", "/api/pools")
        peer2_pools = [p for p in ((all_pools or {}).get("items") or [])
                       if p.get("ServerID") == ids[peer2]]
        if not peer2_pools:
            skip("8c", "还持有存储池的节点摘不掉（池记录会悬空）",
                 f"{ids[peer2]} 名下没有已注册的池，护栏无从触发（见 分布式演进方案 §8.7.4）")
        else:
            st, body = call(W, "DELETE", f"/api/cluster/nodes/{ids[peer2]}")
            check("8c", "还持有存储池的节点摘不掉（池记录会悬空）",
                  st == 409 and ("存储池" in str(body) or "客户机" in str(body)),
                  f"HTTP {st} {str(body)[:110]}")
    st, _ = call(W, "DELETE", "/api/cluster/nodes/no-such-node-at-all")
    check("8d", "不存在的节点报 404，而不是静静地成功", st == 404, f"HTTP {st}")
    # 护栏挡完之后，集群必须原封不动。
    check("8e", "三条拒绝之后花名册没少人", len(roster(W)) == n, f"{len(roster(A))} 台")

    section("9 出厂态纳管：全网段唯一的免认证入口")
    # 「界面上添加节点」的两个端点有意不认证（控制台还没有新机器的任何秘密），只在出厂态（无池、无集群）成立；
    # 在役节点必须拒绝，否则纳管会把它变成备机、目录被复制流整个回滚。
    # 用 192.0.2.1（RFC 5737 文档网段，保证不可达）当 VIP：万一护栏破了，join 取不到集群配置就报 502，不会真改这台机器。
    UNREACHABLE_VIP = "192.0.2.1"
    for base in NODES:
        st, raw = anon_get(base, "/internal/cluster/identity")
        ident = {}
        try:
            ident = json.loads(raw or "{}")
        except Exception:  # noqa: BLE001
            pass
        check(f"9a-{host(base)}", "身份端点免认证可读（控制台扫描全靠它）",
              st == 200 and ident.get("node_id") and "adoptable" in ident,
              f"HTTP {st} {str(raw)[:90]}")
        check(f"9b-{host(base)}", "在役节点自报不可纳管", ident.get("adoptable") is False,
              f"adoptable={ident.get('adoptable')}")
        st, raw = anon_post(base, "/internal/cluster/adopt",
                            {"vip": UNREACHABLE_VIP, "cluster_token": "who-ever-you-are"})
        # 必须是 409「不是出厂态」，即拒在取配置之前；502 说明护栏没挡住，只是碰巧连不上假 VIP。
        check(f"9c-{host(base)}", "陌生人无法纳管在役节点（409，且拒在取配置之前）",
              st == 409, f"HTTP {st} {str(raw)[:110]}")
    # 「拒绝了」得是真拒绝：花名册没少人，也没人被改成别人的备机。
    check("9d", "三台被试探之后集群原样", len(roster(W)) == n, f"{len(roster(W))} 台")

    section("10 挂载态：属性对不等于挂载对")
    # 除了 ZFS 属性还要查 mount 表：属性 mountpoint=none 时 mount 表里可能仍挂着已被 rename 走的旧对象（读出 EIO），
    # 数据库副本永远挂不上；挂载点是绝对路径，send -R 会把写入者的池名一起送过来。
    for base in NODES:
        h = host(base)
        mypool = pool_of(h)
        got, raw = ssh_out(h, f"zfs get -H -o name,value mountpoint -r -t filesystem {mypool}/nd")
        if not got:
            check(f"10a-{h}", "读得到目录容器的挂载点", False, f"SSH 失败：{raw}")
            continue
        # 任何挂载点都不许指向别台的池：路径的第一段就是池名。
        strays = []
        for line in raw.splitlines():
            parts = line.split("\t")
            if len(parts) != 2:
                continue
            name, mp = parts[0], parts[1].strip()
            root = f"/ndiskless/{mypool}"
            if mp.startswith("/") and not (mp == root or mp.startswith(root + "/")):
                strays.append(f"{name}→{mp}")
        check(f"10a-{h}", "目录里没有指向别台池名的挂载点", not strays,
              f"越界={strays}" if strays else f"全在 /ndiskless/{mypool} 下")
        # 属性 mounted=yes 的，mount 表里必须真有，反之亦然。
        got, zfs_mounted = ssh_out(
            h, f"zfs get -H -o name,value mounted -r -t filesystem {mypool}/nd | awk '$2==\"yes\"{{print $1}}'")
        # 两侧口径必须同一棵子树（<池>/nd 之下），否则池根和 nd-diverged-*/nd-rebuilding-* 保留副本会被误算。
        got2, kernel = ssh_out(h, f"mount | awk '$5==\"zfs\"{{print $1}}' | grep -E '^{mypool}/nd(/|$)' || true")
        if not (got and got2):
            check(f"10b-{h}", "属性与 mount 表一致", False, "读不到")
            continue
        prop_set, kern_set = set(zfs_mounted.split()), set(kernel.split())
        check(f"10b-{h}", "ZFS 属性说挂着的，内核里真挂着（反之亦然）",
              prop_set == kern_set,
              f"属性有内核没有={sorted(prop_set - kern_set)} 内核有属性没有={sorted(kern_set - prop_set)}")
        # 最后一道：目录容器与数据库副本都得读得通；EIO 的挂载在属性和内核两侧都看起来正常。
        got, why = ssh_out(h, f"ls /ndiskless/{mypool}/nd >/dev/null 2>&1 && ls /ndiskless/{mypool}/nd/db >/dev/null 2>&1 "
                              f"&& echo READABLE || echo 'FAILED: '$(ls /ndiskless/{mypool}/nd 2>&1 | head -1)")
        check(f"10c-{h}", "目录容器与数据库副本读得通（不是 EIO 的陈旧挂载）",
              got and why.strip() == "READABLE", why.strip()[:120])

    section("11 重启存活：本机身份必须活过一次重启")
    # 节点本地状态必须熬过重启：数据池名要落盘，不能每次启动从随目录复制的单行推断，
    # 推不出又退回 env 里的集群级旧名字。绑错池时 `zfs recv` 每 60 秒失败、healthz 503，界面却看不出异常。
    # 挑备机重启：写入者重启会把虚 IP 甩出去，那是另一条用例的事。
    victim = next((b for b in NODES if ids[b] != writer_id(ids, W)), None)
    if victim is None:
        skip("11a", "重启后仍认得自己的池", "只有一台，没有可安全重启的备机")
    else:
        vh = host(victim)
        before_pool = pool_of(vh)
        _, before_file = ssh_out(vh, "cat /var/lib/ndiskless/data-pool 2>/dev/null")
        ssh(vh, "systemctl restart ndiskless")
        healthy = wait_healthy(vh, 120)
        check(f"11a-{vh}", "重启后自己起得来", healthy, "" if healthy else "120 秒未恢复健康")
        _, after_file = ssh_out(vh, "cat /var/lib/ndiskless/data-pool 2>/dev/null")
        check(f"11b-{vh}", "重启后本机记的池名没变（也不是空的）",
              after_file.strip() != "" and after_file.strip() == before_pool,
              f"重启前 {before_file.strip()!r} / 池 {before_pool!r} → 重启后 {after_file.strip()!r}")
        # 最要命的是复制收进哪个数据集：绑错池时报错只在日志里，这里直接读它发出的 recv 命令。
        target = wait_recv_target(vh, 150)
        check(f"11c-{vh}", "重启后复制收进的是本机自己的池",
              target == f"{before_pool}/nd",
              f"recv 目标={target or '150 秒内没有一次接收'}，期望 {before_pool}/nd")

    section("12 备机不该留着超管卷")
    # 超管机只能放在写入者上，所以切换会在旧节点留下无主的 SCLIENT-*：备机静默只清 CLIENT-*，离线回收器跳过超管，run/ 不参与复制。
    # 超管开机会复用同名克隆，残留会在该节点再当写入者时被捡起，保存时旧内容会被提升成还原点覆盖全集群。
    # 这是不变量，跑在整套矩阵之后最有价值：前面做过大量切换，缺陷若在，残留一定已产生。
    writer_id_now = writer_id(ids, W)
    for base in NODES:
        h = host(base)
        mypool = pool_of(h)
        got, raw = ssh_out(h, f"zfs list -H -o name,used -r {mypool}/run 2>/dev/null "
                              f"| grep SCLIENT- || true")
        if not got:
            check(f"12a-{h}", "读得到本机的运行态克隆", False, f"SSH 失败：{raw}")
            continue
        supers = [line.split()[0] for line in raw.splitlines() if line.strip()]
        if ids[base] == writer_id_now:
            # 写入者上有超管卷是正常的，那是运维正在用的盘。
            skip(f"12a-{h}", "备机上没有超管克隆", f"这台是写入者，持有 {len(supers)} 份是正常的")
            continue
        check(f"12a-{h}", "备机上没有超管克隆（切换会把它留在旧节点，而它再也没有主人）",
              not supers, f"残留 {supers}" if supers else "无")

    section("12.5 每台节点实际有的池，写入者的目录里都有记录")
    # 备机上建的池，记录只写进它自己的库，接任时会被写入者的副本覆盖；
    # 所以节点在心跳里上报自己的池，由写入者补记录（心跳 10 秒一轮，这里给足一分钟）。
    def catalogue_pools():
        st, body = call(W, "GET", "/api/pools")
        items = (body or {}).get("items", []) if st == 200 and isinstance(body, dict) else []
        return {(p.get("ServerID"), p.get("Name")) for p in items}

    for b in NODES:
        _, real = ssh_out(host(b), "for p in $(zpool list -H -o name); do "
                                   "[ \"$(zfs get -H -o value mountpoint $p)\" = \"/ndiskless/$p\" ] && echo $p; done")
        names = [x for x in real.split() if x]
        missing = lambda: [nm for nm in names if (ids[b], nm) not in catalogue_pools()]
        left = wait_for(lambda: missing() == [] or None, 60, step=5)
        check(f"12.5a-{host(b)}", "本机的每个池在目录里都有记录", bool(left),
              f"本机有 {names}，目录里缺 {missing()}")

    section("13 池不在了、记录还在：如实报「未找到」，记录能删、名字能再用")
    # 数据盘换新后池没了、库里那行还在时，界面显示 UNKNOWN、销毁被拦、同名池也建不回来。
    # 这里在备机上用 loop 设备建备份池，在产品之外毁掉，再走一遍恢复；数据池那条路由单元测试覆盖。
    gone_name = "e2egone"
    target = next((b for b in NODES if ids[b] != writer_id(ids, W)
                   and len((own_pools(b, ids[b])[1] or [])) < 2), None)
    if not target:
        skip("13a", "池不在了的记录处理", "没有一台备机还空着备份池的位置")
        return summary()
    th, tid = host(target), ids[target]
    img = "/var/tmp/e2e-gone.img"
    loop = ""
    try:
        _, loop = ssh_out(th, f"truncate -s 1G {img} && losetup -f --show {img}")
        check("13a", "在备机上备好一块测试盘", loop.startswith("/dev/loop"), f"{th} {loop}")
        if not loop.startswith("/dev/loop"):
            return summary()

        def pool_row():
            _, items = own_pools(target, tid)
            return next((p for p in items if p.get("Name") == gone_name), None)

        # 每台一个数据池、一个备份池：已有数据池时再建要被拒，并点名已有的那个。
        data_pool = pool_of(th)
        st, body = call(W, "POST", f"/api/cluster/nodes/{tid}/pools",
                        {"name": "e2edata2", "disks": [loop], "layout": "stripe", "role": "data"})
        check("13a2", "已有数据池时再建数据池被拒，并点名已有的那个",
              st == 409 and data_pool in str(body), f"HTTP {st} {str(body)[:120]}")

        st, body = call(W, "POST", f"/api/cluster/nodes/{tid}/pools",
                        {"name": gone_name, "disks": [loop], "layout": "stripe", "role": "backup"})
        row = wait_for(lambda: (lambda r: r if r and r.get("Health") == "ONLINE" else None)(pool_row()), 120)
        check("13b", "经产品建出备份池", st in (200, 202) and bool(row), f"HTTP {st} {str(body)[:100]}")
        if not row:
            return summary()

        # 池挂在产品目录下，不占根目录的名字。
        _, mp = ssh_out(th, f"zfs get -H -o value mountpoint {gone_name}")
        check("13b1", "新池挂在 /ndiskless 下", mp.strip() == f"/ndiskless/{gone_name}", mp)
        # 两种都有了，什么都不能再建。
        st, body = call(W, "POST", f"/api/cluster/nodes/{tid}/pools",
                        {"name": "e2ebak2", "disks": [loop], "layout": "stripe", "role": "backup"})
        check("13b3", "两种都有时再建被拒，并点名两个池",
              st == 400 and data_pool in str(body) and gone_name in str(body), f"HTTP {st} {str(body)[:120]}")
        # 建池任务记在执行它的节点上，写入者的任务列表要能看到并标出是哪台。
        def create_task_seen():
            _, hist = call(W, "GET", "/api/tasks/history?size=50")
            items = (hist or {}).get("items", []) if isinstance(hist, dict) else []
            return next((t for t in items if t.get("Type") == "create_pool" and t.get("TargetRef") == gone_name), None)
        seen = wait_for(create_task_seen, 30)
        check("13b4", "写入者的任务列表里能看到别台执行的建池任务，并标出服务器",
              bool(seen) and seen.get("Node") == th, f"{(seen or {}).get('Node')} / 期望 {th}")

        ssh_out(th, f"zpool destroy {gone_name}")
        gone = wait_for(lambda: (lambda r: r if r and r.get("Health") == "MISSING" else None)(pool_row()), 60)
        check("13c", "产品之外毁掉后，记录报「未找到」", bool(gone), str(pool_row())[:120])
        check("13d", "不再顶着旧容量", bool(gone) and not gone.get("Capacity") and not gone.get("Used"),
              f"Capacity={(gone or {}).get('Capacity')} Used={(gone or {}).get('Used')}")
        _, agg = call(W, "GET", "/api/cluster/pools")
        node_view = next((n for n in (agg or {}).get("nodes", []) if n.get("node_id") == tid), {})
        listed = next((p for p in node_view.get("pools", []) if p.get("Name") == gone_name), {})
        check("13e", "集群视图里同样是「未找到」、容量为 0",
              listed.get("Health") == "MISSING" and not listed.get("Capacity"), str(listed)[:120])

        st, body = call(W, "DELETE", f"/api/cluster/nodes/{tid}/pools/{row.get('ID')}")
        removed = wait_for(lambda: pool_row() is None or None, 90)
        check("13f", "记录能删掉（不再被「正在使用中」拦住）", st in (200, 202, 204) and bool(removed),
              f"HTTP {st} {str(body)[:100]}")
        _, still = ssh_out(th, f"losetup {loop} >/dev/null && echo yes")
        check("13g", "删记录没碰盘", still == "yes", still)

        # 模拟换上一块新盘：清掉旧标签。
        ssh_out(th, f"zpool labelclear -f {loop} 2>/dev/null; echo ok")
        st, body = call(W, "POST", f"/api/cluster/nodes/{tid}/pools",
                        {"name": gone_name, "disks": [loop], "layout": "stripe", "role": "backup"})
        again = wait_for(lambda: (lambda r: r if r and r.get("Health") == "ONLINE" else None)(pool_row()), 120)
        check("13h", "同名的池能重新建", st in (200, 202) and bool(again), f"HTTP {st} {str(body)[:100]}")
        if again:
            call(W, "DELETE", f"/api/cluster/nodes/{tid}/pools/{again.get('ID')}")
            wait_for(lambda: pool_row() is None or None, 90)
    finally:
        # 不管走到哪一步都还原现场：先毁池，再拆 loop、删文件。
        ssh_out(th, f"zpool destroy -f {gone_name} 2>/dev/null; "
                    f"[ -n '{loop}' ] && losetup -d {loop} 2>/dev/null; rm -f {img}; echo done")
        row = next((p for p in (own_pools(target, tid)[1] or []) if p.get("Name") == gone_name), None)
        if row:
            call(W, "DELETE", f"/api/cluster/nodes/{tid}/pools/{row.get('ID')}")
        _, left = ssh_out(th, f"ls {img} 2>/dev/null; zpool list -H -o name {gone_name} 2>/dev/null; echo end")
        check("13i", "测试盘与测试池已清理", left.strip() == "end", left)

    return summary()


def wait_for(cond, seconds, step=3):
    deadline = time.time() + seconds
    while time.time() < deadline:
        got = cond()
        if got:
            return got
        time.sleep(step)
    return None


def wait_healthy(node_host, seconds):
    deadline = time.time() + seconds
    while time.time() < deadline:
        got, out = ssh_out(node_host, "curl -s -m 5 http://127.0.0.1:8080/healthz")
        if got and '"status":"ok"' in out:
            return True
        time.sleep(5)
    return False


def wait_recv_target(node_host, seconds):
    """备机下一次 `zfs recv` 收进哪个数据集。没有接收就返回空串。"""
    deadline = time.time() + seconds
    while time.time() < deadline:
        got, out = ssh_out(
            node_host,
            "journalctl -u ndiskless --since '-3 min' --no-pager "
            "| grep -oE '\"recv\",\"-F\"[^]]*' | tail -1")
        if got and out.strip():
            # 形如 "recv","-F","-u","-s","-x","mountpoint","data1/nd
            last = out.strip().split(",")[-1].strip('"')
            if last:
                return last
        time.sleep(10)
    return ""


def writer_id(ids, w):
    """当前写入节点的 id。入口是 VIP 时地址对不上任何一台，就问它自己。"""
    for base, nid in ids.items():
        if base == w:
            return nid
    return node_id_of(w)


def other_node_id(ids, w):
    """随便一个不是写入方的节点 id，用来构造「别人的会话编号」。"""
    me = writer_id(ids, w)
    for nid in ids.values():
        if nid != me:
            return nid
    return "someone-else"


def patch_chunk(base, upload_id, offset, data, total):
    """发一个分片。偏移由 Content-Range 显式声明——缺省从 0 写会把续传变成覆盖。"""
    import urllib.error
    import urllib.request
    from ndcluster import login
    req = urllib.request.Request(f"{base}/api/images/uploads/{upload_id}", method="PATCH", data=data)
    req.add_header("Authorization", "Bearer " + login(base))
    req.add_header("Content-Range", f"bytes {offset}-{offset + len(data) - 1}/{total}")
    req.add_header("Content-Type", "application/octet-stream")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip().startswith("{") else raw)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw


def _svc_since(base, key):
    st, body = node_call(base, "GET", "/internal/node/services")
    if st != 200:
        st, body = call(base, "GET", "/api/services")
    if st != 200 or not isinstance(body, dict):
        return ""
    for s in body.get("items") or []:
        if s.get("key") == key:
            return s.get("active_since") or ""
    return ""


def _wait_changed(base, key, before, tries=10):
    import time
    for _ in range(tries):
        time.sleep(2)
        now = _svc_since(base, key)
        if now and now != before:
            return now
    return _svc_since(base, key)


def _pool_id(base, nid):
    _, items = own_pools(base, nid)
    for p in items:
        if p.get("Name") == pool_of(host(base)):
            return p.get("ID", "")
    return items[0].get("ID", "") if items else "none"


if __name__ == "__main__":
    sys.exit(main())
