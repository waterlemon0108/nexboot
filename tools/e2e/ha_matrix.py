#!/usr/bin/env python3
"""双机热备的功能矩阵：VIP 漂移、故障切换、回归、计划切换，对着真实双节点跑。

环境变量：
    ND_NODE_A       主节点 API（如 http://192.168.50.1:18080）
    ND_NODE_B       备节点 API（如 http://192.168.50.11:18080）
    ND_VIP          虚 IP（如 192.168.50.250）
    ND_PASSWORD     A 的登录密码
    ND_B_PASSWORD   B 的登录密码（默认同 ND_PASSWORD）
    ND_SSH_PASSWORD 两台的 SSH 密码（默认同各自的登录密码）
    ND_CLUSTER_TOKEN 集群令牌
    ND_TEST_MAC     一个已登记终端的 MAC（/boot 恢复计时用）

故障注入经各节点自己的真实地址 SSH（VIP 会漂，不能拿它做管道）。
本脚本会在 A 上建名为 ha-e2e-* 的还原点并在结束时清理；不动其它资产。
"""
import os
import shlex
import subprocess
import sys
import time
import urllib.parse
import urllib.request

# 三台都参与 VRRP，VIP 和接管方可能在任何一台，所以 A/B 每一步都从集群角色里发现；
# ND_NODE_A/B 只作节点清单的兜底。
NODES = [b.rstrip("/") for b in
         (os.environ.get("ND_NODES")
          or ",".join(x for x in (os.environ.get("ND_NODE_A"), os.environ.get("ND_NODE_B")) if x)
          ).split(",") if b.strip()]
if len(NODES) < 2:
    raise SystemExit("至少要两台：设 ND_NODES 或 ND_NODE_A/ND_NODE_B")
A = NODES[0]
B = NODES[1]
VIP = os.environ["ND_VIP"]
TOKEN = os.environ["ND_CLUSTER_TOKEN"]
PASS_A = os.environ["ND_PASSWORD"]
PASS_B = os.environ.get("ND_B_PASSWORD", PASS_A)
# 登录口令和系统账号口令是两样东西；混用时 SSH 静默失败并读到空，断言会据此得出错误结论。
SSH_PASS = os.environ.get("ND_SSH_PASSWORD", "")
TEST_MAC = os.environ.get("ND_TEST_MAC", "")
PORT = urllib.parse.urlparse(A).port or 8080
VIP_BASE = f"http://{VIP}:{PORT}"

CHECKS = []


def check(cid, label, ok, detail=""):
    CHECKS.append((cid, label, bool(ok), detail))
    mark = "PASS" if ok else "FAIL"
    print(f"  {mark} {cid} {label}" + (f"  — {detail}" if detail and (not ok or "s" in cid.lower() or True) else ""))


def section(title):
    print(f"\n### {title}")


SKIPPED = []


def skip(cid, label, why):
    """没跑，也不算通过。跳过的检查若印成 PASS，矩阵就是在谎报覆盖面。"""
    SKIPPED.append((cid, label, why))
    print(f"  SKIP {cid} {label}  — {why}")


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


def http(base, path, method="GET", token=None, timeout=8):
    req = urllib.request.Request(base + path, method=method)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:  # noqa: BLE001 — 网络断掉正是被测场景
        return 0, str(e)


SSH_USER = os.environ.get("ND_SSH_USER", "root")


def ssh(host, password, cmd, timeout=30):
    password = SSH_PASS or password
    """在节点上以 root 权限执行。非 root 登录时用 sudo -S 读同一个密码——
    交付现场未必开放 root 直登。"""
    if SSH_USER != "root":
        cmd = f"echo {shlex.quote(password)} | sudo -S -p '' bash -c {shlex.quote(cmd)}"
    r = subprocess.run(
        ["sshpass", "-e", "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "PubkeyAuthentication=no", "-o", f"ConnectTimeout=5", f"{SSH_USER}@{host}", cmd],
        capture_output=True, text=True, timeout=timeout,
        env={"SSHPASS": password, "PATH": os.environ.get("PATH", "")},
    )
    return r.stdout.strip()


HOST_A = urllib.parse.urlparse(A).hostname
HOST_B = urllib.parse.urlparse(B).hostname


def host_of(base):
    return urllib.parse.urlparse(base).hostname


def role_of(base):
    """这台自己怎么说。读不到时返回空字典，调用方据此判断「联系不上」。"""
    return ha_status(base)


def find_active():
    """集群里此刻在写的那台。没有就返回 None——那是「没有主机」，不是「是某一台」。"""
    for b in NODES:
        if role_of(b).get("role") == "active":
            return b
    return None


def find_standby(exclude=()):
    for b in NODES:
        if b in exclude:
            continue
        if role_of(b).get("role") == "standby":
            return b
    return None


def holder_of_vip():
    """持有 VIP 的那台的 base；没人持有返回 None，多人持有抛出——那是脑裂，
    不该被一个「取第一个」的写法掩盖过去。"""
    holders = [b for b in NODES
               if ssh(host_of(b), PASS_A, f"ip addr show | grep -c '{VIP}/' || true") == "1"]
    if len(holders) > 1:
        raise AssertionError(f"VIP 同时挂在多台上：{holders}")
    return holders[0] if holders else None


def ha_status(base):
    st, body = http(base, "/internal/ha/status", token=TOKEN)
    if st != 200:
        return {}
    import json
    return json.loads(body)


def wait(pred, timeout, step=1.0):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if pred():
            return time.time() - t0
        time.sleep(step)
    return None


def vip_holder():
    """保留旧名字给还没改写的调用点：返回 A/B 之外时给出真实地址，
    这样断言失败的信息里能看见 VIP 究竟在哪，而不是一个空字符串。"""
    h = holder_of_vip()
    if h is None:
        return ""
    if h == A:
        return "A"
    if h == B:
        return "B"
    return host_of(h)


def guid_inventory(host, password, root):
    out = ssh(host, password, f"zfs list -H -p -r -t all -o name,guid,origin {root} 2>/dev/null")
    rows = []
    for line in out.splitlines():
        parts = line.split("\t")
        if len(parts) != 3:
            continue
        name, guid, origin = parts
        rel = name[len(root):]
        if "@rep-" in rel or "@ndbackup-" in rel:
            continue
        rows.append(f"{rel}\t{guid if '@' in rel else origin.replace(root, 'R')}")
    return sorted(rows)


section("0 前置：角色与 VIP")
# 当前主机由上一次切换决定，可能是任意一台；每轮开始时问集群。
A = find_active() or A
B = find_standby(exclude=(A,)) or next((b for b in NODES if b != A), B)
HOST_A, HOST_B = host_of(A), host_of(B)
print(f"  本轮 A（主机）={HOST_A}  B（备机）={HOST_B}  共 {len(NODES)} 台")
sa, sb = ha_status(A), ha_status(B)


def _actives():
    return [host_of(b) for b in NODES if ha_status(b).get("role") == "active"]


# 要等收敛，不能只取样一次：紧跟破坏性套件时，角色标记可能还写着 active 而自愈尚未跑到（十几秒）。
if wait(lambda: len(_actives()) == 1, 90, step=3) is None:
    actives = _actives()
else:
    actives = _actives()
    A = find_active() or A
    B = find_standby(exclude=(A,)) or B
    HOST_A, HOST_B = host_of(A), host_of(B)
check("0a", "有且只有一台主机", len(actives) == 1, f"active={actives}")
check("0b", "选出的 B 是备机", sb.get("role") == "standby", str(sb))
check("0c", "VIP 在主机上", holder_of_vip() == A, f"VIP 在 {vip_holder() or '没人'}，主机是 {HOST_A}")
st, _ = http(VIP_BASE, "/healthz")
check("0d", "healthz 经 VIP 可达", st == 200)

section("1 复制收敛与 guid 一致")
# 逐台问各自的数据池名：各节点池名互不相干，用同一个 ND_POOL 去比会两边都读到空，「一致」恒成立。
def data_pool(host, password):
    name = ssh(host, password, "cat /var/lib/ndiskless/data-pool 2>/dev/null")
    if not name:
        # 有备份池时按字母序排前的可能是它（ndbak < ndpool）。
        name = ssh(host, password, "zfs list -H -o name -d 0 | grep -v '^bpool$' | head -1")
    return name or os.environ.get("ND_POOL", "tank")


pool_a = data_pool(HOST_A, PASS_A)
pool_b = data_pool(HOST_B, PASS_B)
# 180 秒按稳态定，别再往上调：安静集群三台追平中位 63 秒、最大 119 秒。
# 在整轮里超时是起点不对（前面的套件刚做完大量切换），起点由 all.sh 套件之间的 wait_settled 保证。
CONVERGE_WINDOW = 180
ok = wait(lambda: guid_inventory(HOST_A, PASS_A, f"{pool_a}/nd") == guid_inventory(HOST_B, PASS_B, f"{pool_b}/nd"),
          CONVERGE_WINDOW, 5)
check("1a", "两侧目录 guid 清单一致（排除系统快照）", ok is not None,
      f"{'%.0f' % ok}s" if ok is not None else f"{CONVERGE_WINDOW}s 未收敛")

section("2 故障切换：杀 A 的服务")
t0 = time.time()
ssh(HOST_A, PASS_A, "systemctl stop keepalived ndiskless")
# 接管方由 VRRP 选举决定，只断言「有人接管」，写死一台会给出与产品无关的红。
def _drifted_away():
    """VIP 已经离开 A 了？

    这里的双持有窗口比别处都宽：刚杀掉写入者，旧主的 VIP 还没被内核撤下、
    新主已经发过 GARP，每 0.5 秒采一次几乎必然撞上。撞上就抛异常打断整个矩阵
    ——而这恰恰是切换最正常的中间态。真没人接管由 wait 超时来判。"""
    try:
        h = holder_of_vip()
    except AssertionError:
        return False
    return h is not None and h != A


drift = wait(_drifted_away, 60, 0.5)
check("2a", "VIP 漂到另一台", drift is not None,
      f"{'%.1f' % (drift or 0)}s → {vip_holder() or '没人接手'}")
# 先拿到 VIP 的不一定是接管方：它若比另一台备机落后一轮，会把 VIP 让给更新的那台。
# 接管方是最终成为主机并持有 VIP 的那台。
def _took_over():
    try:
        h = holder_of_vip()
    except AssertionError:
        return None
    return h if h and h != A and ha_status(h).get("role") == "active" else None


active = wait(lambda: _took_over() is not None, 90, 1)
took = _took_over()
if took:
    B, HOST_B = took, host_of(took)
check("2b", f"接管方（{HOST_B}）激活为主机并持有 VIP", active is not None,
      f"+{'%.1f' % (active or 0)}s" + ("" if took else f"；VIP 在 {vip_holder() or '无人'}"))
healthy = wait(lambda: http(VIP_BASE, "/healthz")[0] == 200, 60, 0.5)
t_recover = time.time() - t0
check("2c", "healthz 经 VIP 恢复", healthy is not None, f"故障到恢复共 {'%.1f' % t_recover}s")
if TEST_MAC:
    st, body = http(VIP_BASE, f"/boot?mac={TEST_MAC}", timeout=60)
    check("2d", "已登记终端经 VIP 从 B 开机", st == 200 and "sanboot" in body, f"HTTP {st}")
epoch_b = ha_status(B).get("epoch", 0)
check("2e", "epoch 已提升", epoch_b >= 1, f"epoch={epoch_b}")

section("3 旧主回归：成为备机且不抢占")
ssh(HOST_A, PASS_A, "systemctl start ndiskless keepalived")
back = wait(lambda: ha_status(A).get("role") == "standby", 120, 2)
check("3a", "A 回归为备机（epoch 门生效）", back is not None, f"{'%.0f' % (back or 0)}s")
time.sleep(5)
check("3b", "VIP 仍在接管方（nopreempt）", holder_of_vip() == B, vip_holder())

# 改动必须发给此刻真正的写入者：三节点里 A 倒下后接管的不一定是 B，
# 发给一台却在另一台的池里等快照，会把「快照不在这台」误报成复制没到。
WRITER_NOW = holder_of_vip() or find_active() or B
HOST_W = host_of(WRITER_NOW)
PASS_W = PASS_A if WRITER_NOW == A else PASS_B
tok_b = None
import json as _json
try:
    req = urllib.request.Request(f"{WRITER_NOW}/api/login", method="POST",
                                 data=_json.dumps({"username": "admin", "password": PASS_A}).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        tok_b = _json.loads(resp.read())["token"]
except Exception as e:  # noqa: BLE001
    pass
# 把「改动做出来了」和「改动复制过去了」分开判；吞掉建还原点的异常会把失败误报成复制问题。
made = False
if not tok_b:
    check("3c0", "能登录到当前主机", False, f"{WRITER_NOW} 登录失败，反向复制无从验证")
else:
    cfgs = _json.loads(http(WRITER_NOW, "/api/images", token=tok_b)[1])
    img = (cfgs.get("items") or [{}])[0].get("ID", "")
    # 配置 ID 从接口查，不要按 f"{img}_default" 猜，猜错的 404 看起来和「复制慢」一样。
    cfg_id = ""
    if img:
        cl = _json.loads(http(WRITER_NOW, f"/api/images/{img}/configs", token=tok_b)[1])
        cfg_id = ((cl.get("items") or [{}])[0]).get("ID", "")
    # 集群里没有镜像时反向复制无从验证，是前提不成立而不是失败；别的矩阵收尾常会删光镜像。
    if not (img and cfg_id):
        skip("3c0", "反向复制（需要集群里已有镜像）",
             "集群里没有镜像与配置，本段无从验证；先导入一个再跑")
    else:
        check("3c0", "拿到镜像与配置", True, f"{img}/{cfg_id}")
    if cfg_id:
        # 在自己 fork 的配置上建还原点：现场配置可能正被超管机编辑，清理时还会改别人在用的应用点。
        TMP_CFG = ""
        try:
            base = (_json.loads(http(WRITER_NOW, f"/api/configs/{cfg_id}/reductions", token=tok_b)[1]).get("items") or [])
            tmp_name = "ha-e2e-" + time.strftime("%H%M%S")
            if base:
                urllib.request.urlopen(urllib.request.Request(
                    f"{WRITER_NOW}/api/configs/{cfg_id}/fork", method="POST",
                    data=_json.dumps({"name": tmp_name, "reduction_id": base[-1]["ID"]}).encode(),
                    headers={"Content-Type": "application/json", "Authorization": "Bearer " + tok_b}), timeout=20).close()
                def fork_seen():
                    # 集群刚折腾完时接口偶尔回 503（非 JSON），当成「还没看到」继续等。
                    try:
                        items = _json.loads(http(WRITER_NOW, f"/api/images/{img}/configs", token=tok_b)[1]).get("items") or []
                    except Exception:  # noqa: BLE001
                        return None
                    return next((c for c in items if c.get("Name") == tmp_name), None)
                # wait 返回的是等待秒数，不是查到的结果，等到后再取一次。
                got = fork_seen() if wait(fork_seen, 180, 5) is not None else None
                TMP_CFG = (got or {}).get("ID", "")
        except Exception:  # noqa: BLE001
            pass
        check("3c0b", "临时配置已就绪", bool(TMP_CFG), TMP_CFG or "fork 没出来")
        cfg_id = TMP_CFG
    if cfg_id:
        # 先清掉上一轮遗留的同名还原点，否则建同名会 409，没有新快照，被误报成复制没到。
        try:
            rl = _json.loads(http(WRITER_NOW, f"/api/configs/{cfg_id}/reductions", token=tok_b)[1])
            for r in rl.get("items") or []:
                # 还原点的 Name 带 @ 前缀，DisplayName 才是裸名，两者都要比。
                if r.get("DisplayName") == "ha-e2e-back" or r.get("Name", "").lstrip("@") == "ha-e2e-back":
                    # 503 是写闸门（新主还没激活完）；重试到闸门打开，否则清理形同未做。
                    for _ in range(20):
                        try:
                            urllib.request.urlopen(urllib.request.Request(
                                f"{WRITER_NOW}/api/reductions/{r['ID']}", method="DELETE",
                                headers={"Authorization": "Bearer " + tok_b}), timeout=20).close()
                            break
                        except urllib.error.HTTPError as he:
                            if he.code == 404:
                                break
                            if he.code != 503:
                                raise
                        except Exception:  # noqa: BLE001
                            pass
                        time.sleep(3)
                    # 删除是异步任务，要等它从列表里消失，否则下一轮建同名会 409。
                    wait(lambda: not any(
                        (x.get("DisplayName") == "ha-e2e-back"
                         or x.get("Name", "").lstrip("@") == "ha-e2e-back")
                        for x in (_json.loads(http(WRITER_NOW, f"/api/configs/{cfg_id}/reductions",
                                                   token=tok_b)[1]).get("items") or [])), 60, 3)
        except Exception:  # noqa: BLE001
            pass
        req = urllib.request.Request(f"{WRITER_NOW}/api/configs/{cfg_id}/reductions", method="POST",
                                     data=_json.dumps({"name": "ha-e2e-back"}).encode(),
                                     headers={"Content-Type": "application/json", "Authorization": "Bearer " + tok_b})
        st_post, body_post = 0, ""
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                st_post, body_post = resp.status, resp.read().decode()[:120]
        except urllib.error.HTTPError as e:
            st_post, body_post = e.code, e.read().decode()[:160]
        except Exception as e:  # noqa: BLE001
            body_post = str(e)[:160]
        # 202 只表示受理，还要等快照真的落到主机的池上。
        made = st_post in (200, 201, 202) and wait(
            lambda: "ha-e2e-back" in ssh(HOST_W, PASS_W, f"zfs list -H -o name -t snapshot -r {data_pool(HOST_W, PASS_W)}/nd | grep ha-e2e-back || true"),
            120, 5) is not None
        check("3c1", "改动已在当前主机上落地", made, f"HTTP {st_post} {body_post}")
if made:
    if True:
        # 复制是两段各 60 秒的轮次（主机打 rep- 标记、备机拉取），切换后还有重启首轮开销。
        # 稳态追平约 54 秒、切换后可达三四分钟；与 1a 同口径，再留一轮给建还原点任务落地。
        back_window = CONVERGE_WINDOW + 180
        seen = wait(lambda: "ha-e2e-back" in ssh(HOST_A, PASS_A, f"zfs list -H -o name -t snapshot -r {pool_a}/nd | grep ha-e2e-back || true"),
                    back_window, 5)
        check("3c", "B 的改动反向复制到 A", seen is not None,
              f"{'%.0f' % (seen or 0)}s" if seen else f"{back_window}s 未到")

section("4 计划切换回 A")
# 按 VIP 判当前主机，而不是「谁先自称 active」：旧主回归后被 epoch 栅栏降级前会短暂自报 active，
# 发到它会被拒。VIP 无人持有时（切换窗口）退回问角色。
# 还要先等 A 重新具备参选资格：刚降级的节点会让出一分钟（退位标记，见 nodeHealth），
# 期间 /healthz 503、keepalived 处于 FAULT，此时发起切换 VIP 会落到别的备机。
eligible = wait(lambda: ssh(HOST_A, PASS_A,
                            f"curl -s -m 5 -o /dev/null -w '%{{http_code}}' http://127.0.0.1:{PORT}/healthz"
                            ) == "200", 120, 5)
if eligible is None:
    print(f"    注意：{HOST_A} 120 秒内未恢复参选资格，切换目标可能落到别处")
cur_active = holder_of_vip() or find_active() or B

# 再等各备机把目录追平才发起切换：排空要求所有活着的备机三分钟内拉到最新一轮，
# 上一段刚做完多次切换，余波未平时产品回 409「切换前同步未完成」是对的。
def _latest_rep(host_ip, password):
    pool = data_pool(host_ip, password)
    out = ssh(host_ip, password, f"zfs list -H -o name -t snapshot -r {pool}/nd | grep '@rep-' | tail -1")
    return out.split("@")[-1] if out else ""


settled = wait(lambda: len({_latest_rep(HOST_A, PASS_A), _latest_rep(HOST_B, PASS_B)}) == 1
               and _latest_rep(HOST_A, PASS_A) != "", 240, 10)
if settled is None:
    print("    注意：两侧目录 240 秒内未追平，切换很可能被排空护栏正确地拒绝")
st = 0
# JWT secret 因节点而异，向当前主机重新登录拿它的 token。
tok_cur = None
try:
    req = urllib.request.Request(f"{cur_active}/api/login", method="POST",
                                 data=_json.dumps({"username": "admin", "password": PASS_A}).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        tok_cur = _json.loads(resp.read())["token"]
except Exception:  # noqa: BLE001
    pass
if tok_cur:
    req = urllib.request.Request(f"{cur_active}/api/ha/planned-switch", method="POST",
                                 headers={"Authorization": "Bearer " + tok_cur})
    body = ""
    try:
        with urllib.request.urlopen(req, timeout=240) as resp:
            st = resp.status
    except urllib.error.HTTPError as e:
        st, body = e.code, e.read().decode()[:160]
    except Exception:  # noqa: BLE001
        st = 0
# 421「指错机器」与 409「同步未完成」处置不同，把理由一并带出，只记状态码分不清该换机还是该等。
check("4a", "计划切换受理（drain 完成后退位）", st in (202, 0),
      f"HTTP {st} {body}（0=连接随服务重启断开，属预期）")
# 计划切换只让当前主机退位，不能指定接班人：VIP 归优先级最高的那台（主机 150、备机 100 起递增），
# 断言写死 A 会假红。要验的是退位发生、另一台接住、B 当主期间的改动跟了过去。
def _settled_elsewhere():
    """切换已经落定到另一台？

    holder_of_vip() 在两台同时持有时会抛异常——而 VRRP 转换中本来就有那个瞬时
    窗口，正好采样到就把整个脚本打断了。切换中不是失败，是还没落定，等下一轮。"""
    try:
        h = holder_of_vip()
    except AssertionError:
        return None
    return h if (h and h != cur_active and ha_status(h).get("role") == "active") else None


moved = wait(_settled_elsewhere, 180, 2)
try:
    new_active = holder_of_vip()
except AssertionError:
    new_active = None
check("4b", "退位后另一台接住（VIP 与主机身份一致）", moved is not None,
      f"{host_of(new_active) if new_active else '无人'} 用时 {'%.0f' % (moved or 0)}s")
if not made:
    # 段 3 没做出改动（多半没有镜像）时无从跟踪，跳过而不是判 FAIL。
    skip("4c", "B 当主期间的改动经切换跟到了新主上", "段 3 未产生可跟踪的改动")
elif new_active:
    nh = host_of(new_active)
    npool = data_pool(nh, PASS_A)
    seen = wait(lambda: "ha-e2e-back" in ssh(nh, PASS_A, f"zfs list -H -o name -t snapshot -r {npool}/nd | grep ha-e2e-back || true"), 60, 2)
    check("4c", "B 当主期间的改动经切换跟到了新主上", seen is not None, nh)
else:
    check("4c", "B 当主期间的改动经切换跟到了新主上", False, "没有新主，无从验证")

section("5 隔离：丢 VRRP 但保 HTTP → 激活门拒绝")
# 段 4 之后谁是备机不确定，取任意一台此刻的备机：隔离它后，对端仍在服务时它不许自我激活。
iso = find_standby() or B
HOST_ISO = host_of(iso)
wait(lambda: ha_status(iso).get("role") == "standby", 60, 2)
print(f"    本段隔离对象：{HOST_ISO}")
# 只丢弃 VRRP（协议 112），HTTP 不动。
ssh(HOST_ISO, PASS_B, "iptables -I INPUT -p 112 -j DROP")
# 收不到 VRRP 通告它会自认 MASTER，出现双持有窗口；此时拦住它的是「对端仍在服务」这道判据。
time.sleep(15)
status_b = ha_status(iso)
check("5a", f"隔离下 {HOST_ISO} 的激活被门拦住（仍为备机）",
      status_b.get("role") == "standby", str(status_b))
ssh(HOST_ISO, PASS_B, "iptables -D INPUT -p 112 -j DROP")
# 给足收敛时间：VRRP 恢复通告、让出 VIP、产品侧再对账一轮。
def _converged_to_one():
    """收敛到单一持有者了？

    和 _settled_elsewhere 同一个道理：解除隔离后，让出 VIP 与恢复通告之间必然
    有一段双持有，holder_of_vip() 对此抛异常。裸塞进 wait 就是在赌采样点——
    真实后果是本段之后的所有段和收尾清理全都不跑，最后一行是 Traceback，
    看上去像产品脑裂，其实是这一秒还没收敛完。真不收敛由 wait 超时来判。"""
    try:
        return holder_of_vip() is not None
    except AssertionError:
        return False


conv = wait(_converged_to_one, 60, 2)
one = True
try:
    holder_of_vip()
except AssertionError:
    one = False
check("5b", "解除隔离后 VIP 收敛回单持有者", conv is not None and one, vip_holder() or "无人持有")

section("6 清理")
if tok_b is None:
    check("6a", "清理跳过（B 登录不可用）", True)
else:
    tok_a = None
    # 删除走 VIP：A 可能早已不是写入者，发给备机会被写闸门挡成 503。
    # 清理结果要如实检查，留下的还原点会在后续回滚里失去快照，绊倒下一套矩阵。
    cleaned, why = False, ""
    try:
        req = urllib.request.Request(f"{VIP_BASE}/api/login", method="POST",
                                     data=_json.dumps({"username": "admin", "password": PASS_A}).encode(),
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=15) as resp:
            tok_v = _json.loads(resp.read())["token"]
        # 测试还原点建在临时配置上，删配置一并带走。按前缀认，fork 了却没记住的也要收走。
        imgs = _json.loads(http(VIP_BASE, "/api/images", token=tok_v)[1])
        leftover = [(im.get("ID", ""), c.get("ID", "")) for im in (imgs.get("items") or [])
                    for c in (_json.loads(http(VIP_BASE, f"/api/images/{im.get('ID')}/configs", token=tok_v)[1]).get("items") or [])
                    if c.get("Name", "").startswith("ha-e2e-")]
        tmp = globals().get("TMP_CFG", "")
        for _, cid in leftover:
            if cid and cid != tmp:
                try:
                    urllib.request.urlopen(urllib.request.Request(
                        f"{VIP_BASE}/api/configs/{cid}", method="DELETE",
                        headers={"Authorization": "Bearer " + tok_v}), timeout=30).close()
                except Exception:  # noqa: BLE001
                    pass
        if not tmp:
            cleaned = True
        else:
            img = next((im for im, cid in leftover if cid == tmp), "") or (imgs.get("items") or [{}])[0].get("ID", "")

            def tmp_gone():
                items = _json.loads(http(VIP_BASE, f"/api/images/{img}/configs", token=tok_v)[1]).get("items") or []
                return not any(c.get("ID") == tmp for c in items)

            # 503 是写闸门、409 可能是备机正在整体同步，都等一等再删。
            for _ in range(20):
                try:
                    urllib.request.urlopen(urllib.request.Request(
                        f"{VIP_BASE}/api/configs/{tmp}", method="DELETE",
                        headers={"Authorization": "Bearer " + tok_v}), timeout=30).close()
                except urllib.error.HTTPError as e:
                    if e.code == 404:
                        break
                    if e.code not in (409, 503):
                        raise
                except Exception:  # noqa: BLE001
                    pass
                if wait(tmp_gone, 30, 5) is not None:
                    break
            cleaned = tmp_gone()
            why = "" if cleaned else f"临时配置 {tmp} 还在"
    except Exception as e:  # noqa: BLE001
        why = str(e)[:120]
    check("6a", "临时配置已清理（留下它会绊倒后面的矩阵）", cleaned, why)

sys.exit(summary())
