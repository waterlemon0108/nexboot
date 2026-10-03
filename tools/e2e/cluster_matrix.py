#!/usr/bin/env python3
"""三节点集群矩阵：节点注册、分组放置、故障回退、quorum 激活门。

拓扑（对着真实三节点跑）：
    A  active（VIP 持有者，keepalived）
    B  standby（keepalived 对端，复制目录）
    C  存储节点（standby 角色、无 keepalived，只承载被放置的客户机）

环境变量：
    ND_NODE_A / ND_NODE_B / ND_NODE_C   各节点自身 API（http://ip:18080）
    ND_VIP                              虚 IP
    ND_PASSWORD                         登录/SSH 密码（三节点一致时）
    ND_CLUSTER_TOKEN                    集群令牌
    ND_POOL                             数据池名（默认 tank）

本脚本创建 cluster-e2e-* 分组与终端并在结束时清理；不动其它资产。
故障注入经各节点自身地址 SSH（VIP 会漂，不能拿它做管道）。
"""
import json
import os
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

A = os.environ["ND_NODE_A"].rstrip("/")
B = os.environ["ND_NODE_B"].rstrip("/")
C = os.environ["ND_NODE_C"].rstrip("/")
VIP = os.environ["ND_VIP"]
TOKEN = os.environ["ND_CLUSTER_TOKEN"]
PASSWORD = os.environ["ND_PASSWORD"]
SSH_PASSWORD = os.environ.get("ND_SSH_PASSWORD") or PASSWORD
POOL = os.environ.get("ND_POOL", "tank")
PORT = urllib.parse.urlparse(A).port or 8080
VIP_BASE = f"http://{VIP}:{PORT}"
C_IP = urllib.parse.urlparse(C).hostname
# 换 C 之前的三台：C 可能被换成某台备机，此后 (A, B, C) 会重复、漏掉写入者，「在所有节点里找」都用它。
ALL_NODES = list(dict.fromkeys([A, B, C]))

TEST_MAC = "0e:2e:c1:05:7e:01"
NORM_MAC = TEST_MAC.replace(":", "").upper()

CHECKS = []


SKIPPED = []


def check(cid, label, ok, detail=""):
    CHECKS.append((cid, label, bool(ok), detail))
    print(f"  {'PASS' if ok else 'FAIL'} {cid} {label}" + (f"  — {detail}" if detail else ""))


def skip(cid, label, why):
    """没跑，也不算通过。

    跳过的检查若印成 PASS，矩阵就是在谎报自己的覆盖面——而那正是最贵的一种
    假绿：流水线是绿的，那块地方其实一次都没测过。
    """
    SKIPPED.append((cid, label, why))
    print(f"  SKIP {cid} {label}  — {why}")


def section(title):
    print(f"\n### {title}")


def summary():
    failed = [c for c in CHECKS if not c[2]]
    line = f"\n{len(CHECKS) - len(failed)}/{len(CHECKS)} 通过"
    if SKIPPED:
        line += f"，{len(SKIPPED)} 跳过"
    print(line)
    for cid, label, _, detail in failed:
        print(f"  失败: {cid} {label} {detail}")
    for cid, label, why in SKIPPED:
        print(f"  跳过: {cid} {label} {why}")
    # 一条都没跑不算通过：`0/0 通过` 配退出码 0 会被读成「一切正常」。
    if not CHECKS:
        print("  没有执行任何检查——这不算通过，先看上面的跳过原因")
        return 1
    return 1 if failed else 0


_tokens = {}


def login(base):
    """拿不到令牌时返回空串，不抛。

    这个函数在 call() 的 try 之外被调用，所以它一抛，整个矩阵当场死掉——而
    「此刻连不上」正是故障注入期间的常态：VIP 在漂、被停的那台还没起来。
    脚本该继续轮询，由断言的超时来判定，而不是被一次 ECONNREFUSED 打断。
    """
    if base not in _tokens:
        try:
            req = urllib.request.Request(base + "/api/login", method="POST",
                                         data=json.dumps({"username": "admin", "password": PASSWORD}).encode(),
                                         headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=15) as r:
                _tokens[base] = json.load(r)["token"]
        except Exception:  # noqa: BLE001
            return ""
    return _tokens.get(base, "")


def call(base, method, path, body=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, method=method, data=data)
    tok = login(base)
    if tok:
        req.add_header("Authorization", "Bearer " + tok)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            return r.status, json.loads(raw) if raw.strip() else {}
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"error": raw}


def http_get(base, path, timeout=10):
    """带登录会话的 GET。

    /boot 会核对来源地址：只有那台机器本人、或已登录的管理员，才问得到它的启动
    配置（否则一台客户机就能拿到另一台的盘坐标，而那块盘正被挂载着）。矩阵跑在
    另一台机器上、代所有被测客户机发问，匿名过不去——实测四条断言齐刷刷
    「404 unknown mac」。带会话是这里唯一诚实的办法：它就是运维在控制台上查看
    「这台机器落在哪」走的那条路。客户机自己那条匿名路径由 boot_matrix 覆盖。"""
    req = urllib.request.Request(base + path)
    try:
        req.add_header("Authorization", "Bearer " + login(base))
    except Exception:
        pass  # 登不上就照匿名发：/healthz 这类本来就不需要会话，不该被这一步拖红
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)


SSH_USER = os.environ.get("ND_SSH_USER", "root")


def ssh(host, cmd, timeout=60):
    if SSH_USER != "root":
        cmd = f"echo {shlex.quote(SSH_PASSWORD)} | sudo -S -p '' bash -c {shlex.quote(cmd)}"
    return subprocess.run(
        ["sshpass", "-p", SSH_PASSWORD, "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "ConnectTimeout=8", f"{SSH_USER}@{host}", cmd],
        capture_output=True, text=True, timeout=timeout)


_pools = {}


def pool_of(host):
    """那台机器**自己**的数据池名（与 ndcluster.pool_of 同义；本脚本刻意不依赖它）。

    各节点的池名互不相干：目录里从不存带池名的绝对路径，池记录也按 (节点, 池名)
    区分。写死一个 ND_POOL 去 ssh 别的节点等于假设全集群同名——真机上三台分别叫
    data / tank / data1，写死的那个在两台上根本不存在，断言查了个空目录还「通过」
    了，比红更糟。先读产品自己记的那份（节点本地、不随目录复制），读不到再猜。
    """
    if host not in _pools:
        r = ssh(host, "cat /var/lib/ndiskless/data-pool 2>/dev/null")
        name = r.stdout.strip() if r.returncode == 0 else ""
        if not name:
            r = ssh(host, "zfs list -H -o name -d 0 2>/dev/null | grep -v '^bpool$' | head -1")
            name = r.stdout.strip() if r.returncode == 0 else ""
        _pools[host] = name or POOL
    return _pools[host]


def writer_host():
    """此刻真正在写的那台。前面几段做过停机与回退，写入者未必还是 A。"""
    for base in ALL_NODES:
        try:
            st, body = call(base, "GET", "/api/ha", timeout=6)
            if st == 200 and isinstance(body, dict) and body.get("role") == "active":
                return urllib.parse.urlparse(base).hostname
        except Exception:  # noqa: BLE001
            continue
    return ""


def nodes(base):
    st, body = call(base, "GET", "/api/cluster/nodes")
    return {n["id"]: n for n in body.get("items", [])} if st == 200 else {}


def wait(cond, seconds, step=3):
    deadline = time.time() + seconds
    while time.time() < deadline:
        v = cond()
        if v:
            return v
        time.sleep(step)
    return None


def cleanup(group_id, term_id):
    for base in (VIP_BASE, A):
        try:
            if term_id:
                call(base, "DELETE", f"/api/terminals/{term_id}")
            if group_id:
                call(base, "DELETE", f"/api/groups/{group_id}")
            break
        except Exception:
            continue


def any_image(base):
    """任一可用镜像和它的一个配置，用于在没有样板分组时建组。"""
    st, body = call(base, "GET", "/api/images")
    for img in (body.get("items") or []) if st == 200 and isinstance(body, dict) else []:
        st2, cfgs = call(base, "GET", f"/api/images/{img['ID']}/configs")
        for c in (cfgs.get("items") or []) if st2 == 200 and isinstance(cfgs, dict) else []:
            return img["ID"], c["ID"]
    return None, None


def cleanup_stale():
    st, groups = call(VIP_BASE, "GET", "/api/groups")
    if st != 200 or not isinstance(groups, dict):
        return
    for g in groups.get("items", []):
        if not g.get("Name", "").startswith("cluster-e2e"):
            continue
        st2, terms = call(VIP_BASE, "GET", "/api/terminals")
        if st2 == 200 and isinstance(terms, dict):
            for t in terms.get("items", []):
                if t.get("GroupID") == g["ID"]:
                    call(VIP_BASE, "DELETE", f"/api/terminals/{t['ID']}")
        call(VIP_BASE, "DELETE", f"/api/groups/{g['ID']}")
    # 上一轮没删掉的临时配置（要先删分组才删得掉，所以放在后面）。
    st, images = call(VIP_BASE, "GET", "/api/images")
    for img in (images.get("items") or []) if st == 200 and isinstance(images, dict) else []:
        st2, cfgs = call(VIP_BASE, "GET", f"/api/images/{img['ID']}/configs")
        for c in (cfgs.get("items") or []) if st2 == 200 and isinstance(cfgs, dict) else []:
            if c.get("Name", "").startswith("cluster-e2e-"):
                call(VIP_BASE, "DELETE", f"/api/configs/{c['ID']}")


def broken_reductions():
    """快照已经不在池上的还原点（一致性检查的 missing_snapshot）。

    只读。本矩阵在自己 fork 的配置上开机，fork 时避开这些即可；现场配置上的坏点
    交给运维处理——自动删掉它们会连带删掉从旁置副本找回数据的线索，挪当前应用点
    则是在改别人正在开机用的内容。
    """
    st, rep = call(VIP_BASE, "GET", "/api/system/consistency", timeout=40)
    if st != 200 or not isinstance(rep, dict) or rep.get("ok"):
        return set()
    broken = {i.get("ref") for i in rep.get("issues", []) if i.get("kind") == "missing_snapshot"}
    if broken:
        print(f"    现场有快照已丢失的还原点（不处理，fork 时避开）：{sorted(broken)}")
    return broken


def main():
    # 先清掉上一轮崩掉留下的测试资产，否则「分组已存在」会让本轮从第一步就失真。
    cleanup_stale()
    broken = broken_reductions()
    section("0 三节点在册")
    roster = wait(lambda: (lambda n: n if len(n) >= 3 else None)(nodes(VIP_BASE)), 90)
    check("0a", "roster 含三节点", roster and len(roster) >= 3, f"{sorted(roster or {})}")
    # 这段测「承载客户机的那台死了，放置回退到主机」，C 必须是备机；
    # ND_NODE_C 可能正是当前主机，停掉它就成了故障切换，还会把 VIP 打漂。
    global C, C_IP
    c_id = None
    for nid, n in (roster or {}).items():
        if (n.get("portal_ip") == C_IP or n.get("ip") == C_IP) and n.get("ha_state") != "active":
            c_id = nid
    if not c_id:
        for nid, n in (roster or {}).items():
            if n.get("ha_state") == "standby" and n.get("online"):
                c_id, C_IP = nid, n.get("ip") or n.get("portal_ip")
                C = f"http://{C_IP}:{PORT}"
                print(f"  （ND_NODE_C 此刻是主机，本轮改用备机 {C_IP} 作为 C）")
                break
    check("0b", "C 在册且在线", c_id and roster[c_id].get("online"), f"c_id={c_id}")
    st, body = http_get(C, "/healthz")
    check("0c", "C healthz 可用", st in (200, 503), f"{st} {body[:80]}")
    if not c_id:
        return summary()

    section("1 分组放置到 C")
    st, groups = call(VIP_BASE, "GET", "/api/groups")
    base_group = ((groups.get("items") or [{}])[0]) if isinstance(groups, dict) else {}
    img, cfg = base_group.get("SystemImageID"), base_group.get("SystemConfigID")
    if not (img and cfg):
        # 环境里没有分组是干净状态而非故障，镜像和配置直接问目录要。
        img, cfg = any_image(VIP_BASE)
    if not (img and cfg):
        # 没有镜像时后面每一条都会失败且指向别处，一条明确的跳过更有用。
        skip("1-5", "放置与超管相关全部检查", "目录里没有可用的镜像/配置，先导入一个再跑")
        return summary()
    check("1a", "有可复用的镜像/配置", True, f"{img}/{cfg}")
    # 用自己 fork 的配置：3.5 要设超管、存还原点、改当前应用点，共用现场配置会撞上别人的超管机，也会改别人在用的配置。
    _, reds = call(VIP_BASE, "GET", f"/api/configs/{cfg}/reductions")
    base_reds = [r for r in ((reds or {}).get("items") or [] if isinstance(reds, dict) else [])
                 if r.get("ID") not in broken]
    tmp_cfg_name = "cluster-e2e-" + time.strftime("%H%M%S")
    tmp_cfg = None
    if base_reds:
        call(VIP_BASE, "POST", f"/api/configs/{cfg}/fork",
             {"name": tmp_cfg_name, "reduction_id": base_reds[-1]["ID"]})
        tmp_cfg = wait(lambda: next((c for c in (call(VIP_BASE, "GET", f"/api/images/{img}/configs")[1] or {}).get("items", [])
                                     if c.get("Name") == tmp_cfg_name), None), 180, 5)
    check("1a2", "套件自己的临时配置已就绪", bool(tmp_cfg), tmp_cfg_name)
    if not tmp_cfg:
        return summary()
    cfg = tmp_cfg["ID"]
    # C 还没收到配置快照时开机，产品会正确地退回本机服务，克隆留在写入者上；等 C 收到再往下走。
    on_c = wait(lambda: ssh(C_IP, f"zfs list -H -t snapshot -o name -r {pool_of(C_IP)}/nd/{cfg} 2>/dev/null "
                                  f"| grep -c @ || true").stdout.strip() not in ("", "0") or None, 300, 5)
    check("1a3", "C 已收到临时配置（否则开机会退回写入者）", bool(on_c), cfg)
    # 网络参数从既有分组借，写死网段换环境就会被拒；没有样板分组时从 VIP 推，VIP 必在客户机网段内。
    tmpl_ip = base_group.get("StartIP", "") or VIP
    head = tmpl_ip.rsplit(".", 1)[0]
    start_ip = f"{head}.210"
    st, g = call(VIP_BASE, "POST", "/api/groups", {
        "name": "cluster-e2e", "start_ip": start_ip, "client_max": 5,
        "gateway": base_group.get("Gateway", ""), "netmask": base_group.get("Netmask", "255.255.255.0"),
        "dns1": "114.114.114.114", "dns2": "223.5.5.5",
        "system_image_id": img, "system_config_id": cfg, "storage_server_id": c_id})
    check("1b", "建组并 pin 到 C", st in (200, 201) and g.get("StorageServerID") == c_id, f"{st} {g if st not in (200, 201) else ''}")
    gid = g.get("ID")
    st, t = call(VIP_BASE, "POST", "/api/terminals",
                 {"name": "cluster-e2e-t1", "mac": TEST_MAC, "ip": start_ip, "group_id": gid})
    check("1c", "登记终端", st in (200, 201), f"{st} {t if st not in (200, 201) else ''}")
    tid = t.get("ID")

    st, script = http_get(VIP_BASE, f"/boot?mac={urllib.parse.quote(TEST_MAC)}", timeout=60)
    check("1d", "/boot 落到 C（sanhook 指 C 地址）", st == 200 and f"iscsi:{C_IP}" in script, f"{st} {script[:120]}")
    r = ssh(C_IP, f"zfs list -H -o name -r {pool_of(C_IP)}/run 2>/dev/null | grep -c 'CLIENT-{NORM_MAC}' || true")
    check("1e", "C 上出现该客户机克隆", r.returncode == 0 and int(r.stdout.strip() or 0) >= 1, r.stdout.strip())
    a_host = urllib.parse.urlparse(A).hostname
    r = ssh(a_host, f"zfs list -H -o name -r {pool_of(a_host)}/run 2>/dev/null | grep -c 'CLIENT-{NORM_MAC}' || true")
    check("1f", "A 上没有该克隆", r.returncode == 0 and int(r.stdout.strip() or 0) == 0, r.stdout.strip())

    # guest 里的脚本向给它盘的那台（iSCSI 会话的 TargetAddress）要网关/DNS 和数据盘盘符，那台多半是备机。
    # 备机的写闸门不能拦 /boot 家族，也必须认得这台客户机，否则客户机拿到盘却拿不到网络配置。
    C_BASE = f"http://{C_IP}:{PORT}"
    st, body = http_get(C_BASE, f"/boot/net-config?mac={urllib.parse.quote(TEST_MAC)}", timeout=30)
    check("1g", "客户机向承载它的节点要得到网关/DNS", st == 200 and "gateway" in body, f"{st} {body[:120]}")
    st, body = http_get(C_BASE, f"/boot/data-disks?mac={urllib.parse.quote(TEST_MAC)}", timeout=30)
    check("1h", "客户机向承载它的节点要得到数据盘盘符", st == 200 and "items" in body, f"{st} {body[:120]}")

    # 开了 CHAP 时凭据必须真的写进 ACL；写成空 userid/password 时客户机登录会 not authorized，而服务端看起来一切正常。
    probe = (
        "cfg=/sys/kernel/config/target/iscsi; "
        "for t in $(ls $cfg 2>/dev/null | grep client-); do "
        "cat $cfg/$t/tpgt_1/attrib/authentication 2>/dev/null; "
        "for a in $cfg/$t/tpgt_1/acls/*/auth/userid; do "
        "[ -f $a ] && wc -c < $a; done; done"
    )
    nums = [n for n in ssh(C_IP, probe).stdout.split() if n.isdigit()]
    if len(nums) < 2:
        skip("1i", "CHAP target 的 ACL 带着凭据", f"读不到 configfs 或没有 target（{nums}）")
    elif nums[0] == "0":
        skip("1i", "CHAP target 的 ACL 带着凭据", "本环境 CHAP 未开启（demo 模式）")
    else:
        check("1i", "CHAP target 的 ACL 带着凭据（空凭据 = 客户机永远登录不上）",
              int(nums[1]) > 1, f"authentication={nums[0]} userid 字节数={nums[1]}")

    section("2 C 故障：回退本机")
    ssh(C_IP, "systemctl stop ndiskless")
    down = wait(lambda: not nodes(VIP_BASE).get(c_id, {}).get("online"), 150)
    check("2a", "C 失联后 roster 标离线（≤150s）", bool(down))
    st, script = http_get(VIP_BASE, f"/boot?mac={urllib.parse.quote(TEST_MAC)}", timeout=60)
    check("2b", "/boot 回退主机（sanhook 指 VIP）", st == 200 and f"iscsi:{VIP}" in script, f"{st} {script[:120]}")

    section("3 C 恢复：放置回去")
    ssh(C_IP, "systemctl start ndiskless")
    up = wait(lambda: nodes(VIP_BASE).get(c_id, {}).get("online"), 150)
    check("3a", "C 回归 roster 在线", bool(up))
    st, script = http_get(VIP_BASE, f"/boot?mac={urllib.parse.quote(TEST_MAC)}", timeout=60)
    check("3b", "/boot 再次落到 C", st == 200 and f"iscsi:{C_IP}" in script, f"{st} {script[:120]}")

    section("3.5 超管保存必须落在写入者上")
    # 目录容器 <池>/nd/ 从写入者单向复制、收流用 recv -F，非写入者上对它的写入会被下一轮整体回滚。
    # 超管保存要往目录里 promote 并打快照，若在备机上做，快照会消失而还原点行留在库里，
    # 之后该分组 /boot 一直 500，且当前点拒绝删除，恢复路径被堵死。所以超管机必须落在写入者上。
    # 用独立终端：超管会把放置挪到写入者，与前几段共用一台会让第 5 段「C 上克隆已清除」假红。
    SUP_MAC = "0e:2e:c1:05:7e:0f"
    SUP_NORM = SUP_MAC.replace(":", "").upper()
    sup_tid = f"terminal-{SUP_NORM}"
    call(VIP_BASE, "DELETE", f"/api/terminals/{sup_tid}")
    st, _ = call(VIP_BASE, "POST", "/api/terminals",
                 {"name": "cluster-e2e-super", "mac": SUP_MAC, "ip": f"{head}.211",
                  "group_id": gid, "state": "offline"})
    check("3.5a", "登记超管测试机", st in (200, 201), f"HTTP {st}")
    st_sup, body_sup = call(VIP_BASE, "POST", f"/api/terminals/{sup_tid}/super")
    # 启用失败要如实报：否则保存以「该终端不是超管机」回 409，和「还原点重名」的 409 分不清。
    check("3.5a2", "启用超管", st_sup in (200, 201), f"HTTP {st_sup} {str(body_sup)[:110]}")
    st, script = http_get(VIP_BASE, f"/boot?mac={urllib.parse.quote(SUP_MAC)}", timeout=60)
    # 写入者的对外地址就是 VIP：分组虽 pin 在 C 上，超管机也必须挪到写入者。
    check("3.5b", "超管机的盘落在写入者上（sanhook 指 VIP，而非 pin 的 C）",
          st == 200 and f"iscsi:{VIP}" in script, f"{st} {script[:100]}")

    if st == 200:
        # 保存要求终端已离线，这里手工置为 offline；留着 unknown 会等满两分钟再以「需要先关机」失败。
        call(VIP_BASE, "PUT", f"/api/terminals/{sup_tid}", {"state": "offline"})
        # 先清掉上一轮遗留的同名还原点，否则保存直接 409。删除走 /api/reductions/<id>，
        # 且删除是异步的，要等它从列表里消失。
        def drop_super_reduction():
            _, body = call(VIP_BASE, "GET", f"/api/configs/{cfg}/reductions")
            items = (body or {}).get("items") or []
            left = [r for r in items
                    if r.get("DisplayName") == "e2e-super"
                    or r.get("Name", "").lstrip("@") == "e2e-super"]
            if not left:
                return True
            # 当前点拒绝删除，超管保存又会把它设为当前点，所以先挪开当前点再删，否则 DELETE 受理了行却还在。
            other = next((r.get("ID") for r in items
                          if r.get("ID") not in {x.get("ID") for x in left}), "")
            if other:
                call(VIP_BASE, "POST", f"/api/reductions/{other}/apply", timeout=40)
            for r in left:
                call(VIP_BASE, "DELETE", f"/api/reductions/{r.get('ID')}", timeout=40)
            return False

        if not drop_super_reduction():
            wait(drop_super_reduction, 90, 5)

        # /boot 返回只表示脚本发出去了，克隆和导出是异步的；等盘真的出现在写入者上再保存，否则被拒「还没有带着这块盘以超管机开机过」。
        def super_volume_ready():
            for b in ALL_NODES:
                h = urllib.parse.urlparse(b).hostname
                r = ssh(h, f"zfs list -H -o name -r {pool_of(h)}/run 2>/dev/null "
                           f"| grep -c 'SCLIENT-{SUP_NORM}' || true")
                if r.returncode == 0 and int(r.stdout.strip() or 0) > 0:
                    return True
            return False

        ready = wait(super_volume_ready, 120, 5)
        check("3.5b2", "超管机的盘已经建出来（保存的前提）", ready is not None,
              "" if ready is not None else "120 秒内没有任何节点出现 SCLIENT 卷")
        # 记下保存前各备机的时间，3.5f 只看这之后的日志。
        saved_at = {urllib.parse.urlparse(b).hostname: ssh(urllib.parse.urlparse(b).hostname, "date -u '+%Y-%m-%d %H:%M:%S'").stdout.strip()
                    for b in ALL_NODES}
        st, body = call(VIP_BASE, "POST", f"/api/terminals/{sup_tid}/super/stop",
                        {"reduction_name": "e2e-super"})
        # 带上原文：409 有三种来路（不是超管机 / 还没开机过 / 还原点重名），只看状态码分不清。
        check("3.5c", "超管保存受理", st in (200, 202), f"HTTP {st} {str(body)[:140]}")

        def has_row():
            _, body = call(VIP_BASE, "GET", f"/api/configs/{cfg}/reductions")
            return any(r.get("DisplayName") == "e2e-super" for r in (body.get("items") or []))

        check("3.5d", "还原点的行已写入", wait(has_row, 240, 5) is not None)

        # 行与快照必须同生共死。查当前写入者自己的池：写入者不一定还是 A，各节点池名也互不相干，
        # 写死会查一个不存在的路径，grep 空目录同样「没找到」。
        w_host = writer_host() or urllib.parse.urlparse(A).hostname
        snap = wait(lambda: "e2e-super" in ssh(
            w_host, f"zfs list -H -o name -t snapshot -r {pool_of(w_host)}/nd | grep e2e-super || true").stdout,
            180, 5)
        check("3.5e", "写入者的池上有对应快照（在别处保存会被复制流回滚）",
              snap is not None, f"{w_host}:{pool_of(w_host)}")

        # 超管保存把配置换了身份，备机应逐个数据集追上而不是整份重建（整份一次 15G、一个多小时）。
        standbys = [urllib.parse.urlparse(b).hostname for b in ALL_NODES if urllib.parse.urlparse(b).hostname != w_host]
        arrived = wait(lambda: all("e2e-super" in ssh(h, f"zfs list -H -o name -t snapshot -r {pool_of(h)}/nd | grep e2e-super || true").stdout
                                   for h in standbys) or None, 300, 10)
        rebuilt = {h: ssh(h, f"journalctl -u ndiskless --since '{saved_at.get(h, '')}' -o cat | grep -c 'rebuilding the copy from scratch' || true").stdout.strip()
                   for h in standbys}
        caught = {h: ssh(h, f"journalctl -u ndiskless --since '{saved_at.get(h, '')}' -o cat | grep -c 'caught up dataset by dataset' || true").stdout.strip()
                  for h in standbys}
        check("3.5f", "超管保存之后备机逐个数据集追上，没有整份重建",
              bool(arrived) and all(v == "0" for v in rebuilt.values()),
              f"整份重建 {rebuilt}，逐个追赶 {caught}" + ("" if arrived else "；300 秒内还原点没到齐"))

        # 收尾：先把当前点切回原来的再删这个还原点，当前点不允许直接删。
        _, rl = call(VIP_BASE, "GET", f"/api/configs/{cfg}/reductions")
        prev = next((r["ID"] for r in (rl.get("items") or [])
                     if r.get("DisplayName") != "e2e-super"), "")
        if prev:
            call(VIP_BASE, "POST", f"/api/reductions/{prev}/apply")
            time.sleep(8)
        call(VIP_BASE, "DELETE", f"/api/reductions/{cfg}_e2e-super")
        time.sleep(8)
    call(VIP_BASE, "DELETE", f"/api/terminals/{sup_tid}")

    section("3.8 各节点的数据池名互不相干")
    # 池名不能存在随目录复制的 system_settings.data_pool：各节点池名各不相同，
    # 共用一个名字会让接管方去找别台的池、healthz 永远 503。这里钉住每台绑的是自己的池且都健康。
    names = {}
    for base in ALL_NODES:
        h = urllib.parse.urlparse(base).hostname
        names[h] = pool_of(h)
    check("3.8a", "每台都报得出自己的数据池", all(names.values()), str(names))
    bad = []
    for h, want in names.items():
        # 进程实际绑定的池：它建出来的容器就在自己那个池下面。
        r = ssh(h, f"zfs list -H -o name {want}/nd 2>/dev/null | head -1")
        if r.returncode != 0 or not r.stdout.strip():
            bad.append(f"{h}:{want}")
    check("3.8b", "每台的目录容器都在自己的池里（不是别台的名字）", not bad, f"缺失={bad}" if bad else str(names))
    unhealthy = []
    for base in ALL_NODES:
        st, _ = http_get(base, "/healthz", timeout=8)
        if st != 200:
            unhealthy.append(urllib.parse.urlparse(base).hostname)
    check("3.8c", "池名不同也全都健康（不同名不是故障）", not unhealthy, f"不健康={unhealthy}")

    section("4 激活门（隔离 keepalived 对中的 standby 一方）")
    # 隔离此刻的 standby：它的 keepalived 会自认 MASTER 并请求激活。守门的是「对端仍在服务就拒绝」这道直接判据；
    # 若隔离彻底（连对端 API 也探不到），按设计它会接管，那时靠 4c 验证愈合后只剩一个 active。
    a_ip = urllib.parse.urlparse(A).hostname
    b_ip = urllib.parse.urlparse(B).hostname
    st_a, ha_a = call(A, "GET", "/api/ha")
    iso_base, iso_host, other_ip = (A, a_ip, b_ip)
    if st_a == 200 and ha_a.get("role") == "active":
        iso_base, iso_host, other_ip = (B, b_ip, a_ip)
    b_host = iso_host
    B_local = iso_base
    # 精准隔离：只断 VRRP，集群 API 要留着，否则「探不到任何人」正是设计允许接管的情形（见 role.go），测的就不是激活门。
    # SSH 也必须保住：脚本可能就跑在被隔离的机器上，全量 DROP 会切断自己的连接。
    rules = "iptables -I INPUT -p vrrp -j DROP; iptables -I OUTPUT -p vrrp -j DROP"
    # 用隔离前的时间戳做日志起点：相对窗口慢一拍就落在窗口外，开大了又会捞到前面几段的旧记录。
    t_iso = ssh(b_host, "date '+%Y-%m-%d %H:%M:%S'").stdout.strip() or "-5 min"
    ssh(b_host, rules, timeout=90)
    try:
        # 自愈循环每 10~15 秒一轮，给它几次机会再判定。
        time.sleep(45)
        # /api/ha 走 JWT；被隔离节点上用集群内部端点（cluster token）读角色。
        r = ssh(b_host, f"curl -s -m 5 -H 'Authorization: Bearer {TOKEN}' http://127.0.0.1:{PORT}/internal/ha/status")
        b_state = {}
        try:
            b_state = json.loads(r.stdout or "{}")
        except Exception:
            pass
        # 读不到状态时不判失败：iptables 规则顺序可能让它连回环都读不到，此时以 4b 的日志为准。
        got_role = b_state.get("role")
        check("4a", "被隔离的一方不自我激活（对端仍在服务）", got_role == "standby",
              (r.stdout[:160] or "（读不到状态，以 4b 的日志为准）"))
        # 拒绝理由有两种措辞（对端持有 VIP / 对端 epoch 更高），只钉共同的后半句。
        r = ssh(b_host, f"journalctl -u ndiskless --since {shlex.quote(t_iso)} --no-pager | grep -c '拒绝激活以避免双写' || true")
        check("4b", "被隔离一方日志写明拒绝理由", int(r.stdout.strip() or 0) >= 1, r.stdout.strip())
    finally:
        undo = "iptables -D INPUT -p vrrp -j DROP; iptables -D OUTPUT -p vrrp -j DROP"
        ssh(b_host, undo, timeout=90)
    def vip_active():
        try:
            st, body = call(VIP_BASE, "GET", "/api/ha")
            return st == 200 and body.get("role") == "active"
        except Exception:
            return False
    conv = wait(vip_active, 90)
    check("4c", "解除隔离后 VIP 上仍是唯一 active", bool(conv))

    section("5 清理（回收路由回 C）")

    # 刚解除隔离时 VIP 可能还在漂，发往 VIP 的请求会挂满 30s 超时；清理不是断言对象，超时就等一等再发，别把测试资产留在现场。
    def del_via_vip(path, seconds=90):
        deadline = time.time() + seconds
        while True:
            try:
                st, body = call(VIP_BASE, "DELETE", path)
                # 503 是闸门：VIP 此刻落在一台还没激活的备机上，同样只是未稳。
                if st != 503 or time.time() > deadline:
                    return st, body
            except Exception as e:  # noqa: BLE001 — 超时/连接被拒都只是 VIP 未稳
                if time.time() > deadline:
                    return 0, {"error": str(e)}
            time.sleep(3)

    if tid:
        st, _ = del_via_vip(f"/api/terminals/{tid}")
        check("5a", "删除终端（清理发往 C）", st in (200, 204), str(st))
        r = ssh(C_IP, f"zfs list -H -o name -r {pool_of(C_IP)}/run 2>/dev/null | grep -c 'CLIENT-{NORM_MAC}' || true")
        check("5b", "C 上克隆已清除", r.returncode == 0 and int(r.stdout.strip() or 0) == 0, r.stdout.strip())
    if gid:
        # 分组下还挂着客户机时删除会正确地 409「分组正在使用中」，要清完自己建的全部（含 3.5 段的超管机）。
        # 「读到了空」和「没读到」必须分开：VIP 漂移或写闸门关着时 GET 拿回空壳，当成组里没人就一个都不会删。
        leftovers, read_ok = [], False
        for _ in range(20):
            st_list, listing = call(VIP_BASE, "GET", "/api/terminals")
            if st_list != 200 or not isinstance(listing, dict):
                time.sleep(3)
                continue
            read_ok = True
            leftovers = [t for t in (listing.get("items") or [])
                         if t.get("GroupID") == gid]
            if not leftovers:
                break
            for t in leftovers:
                del_via_vip(f"/api/terminals/{t.get('ID')}")
            time.sleep(3)
        st, body = del_via_vip(f"/api/groups/{gid}")
        check("5c", "删除测试分组", st in (200, 204),
              f"{st} {str(body)[:120]}"
              + (f"（组里还剩 {[t.get('MAC') for t in leftovers]}）" if leftovers
                 else "（终端清单读到了空）" if read_ok
                 else "（20 次都没读到终端清单，无从判断组里还有谁）"))
    # 备机正在整份同步目录时，删配置会被正确地拒绝（409）；磁盘慢时同步要一个多小时，
    # 如实跳过，临时配置留给下一轮的 cleanup_stale。
    gone, st, body = False, 0, ""
    for _ in range(10):
        st, body = del_via_vip(f"/api/configs/{cfg}")
        gone = wait(lambda: not any(c.get("ID") == cfg for c in
                                    (call(VIP_BASE, "GET", f"/api/images/{img}/configs")[1] or {}).get("items", [])), 60, 5)
        if gone or (st == 409 and "整份目录同步" in str(body)):
            break
    if not gone and st == 409 and "整份目录同步" in str(body):
        skip("5d", "删除套件的临时配置", f"备机正在整份同步目录，删除按设计被拒；留给下一轮清理（{cfg}）")
    else:
        check("5d", "删除套件的临时配置", bool(gone), f"{cfg} HTTP {st} {str(body)[:80]}")
    return summary()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
