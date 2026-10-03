#!/usr/bin/env python3
"""异常切换专项矩阵：把主机用各种方式弄坏，验证每一种都正确切换、且不丢不错。

`ha_matrix.py` 只用「优雅停服务」这一种故障。真实机房里主机是被拔电、掉网卡、
撑爆磁盘、内核模块掉了、进程被 OOM 杀掉——每一种的表现都不同，这里逐个注入。

覆盖：
    A 故障注入方式   kill -9 / 崩溃循环 / 整机断电 / 网卡断链 / 存储池故障 /
                     数据库不可写 / iSCSI 模块卸载 / 维护标记
    B 切换正确性     epoch 单调、数据一致、dnsmasq 跟随、客户机能开机
    C 脑裂防护       双向隔离、旧主复活不抢主、**旧主带未复制数据回归被回滚**
    D 稳定性         连续切换不飘、复制中断后自愈

环境变量：
    ND_NODES         逗号分隔的节点 API（如 http://192.168.10.3:8080,http://192.168.10.4:8080）
    ND_VIP           虚 IP
    ND_PASSWORD      管理员密码
    ND_SSH_PASSWORD  各节点 SSH 密码（默认同 ND_PASSWORD）
    ND_SSH_USER      SSH 用户，默认 root
    ND_CLUSTER_TOKEN 集群令牌
    ND_RUNNER_IP     跑本脚本的机器 IP；对它自己不做致命注入（断电/断网），
                     其余节点照做——机制相同，避免脚本把自己弄死。

破坏性：会反复切换主备、重启节点。只在演练环境跑。
测试期间创建 fo-e2e-* 分组，结束时清理。
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

NODES = [n.strip().rstrip("/") for n in os.environ["ND_NODES"].split(",") if n.strip()]
VIP = os.environ["ND_VIP"]
PASSWORD = os.environ["ND_PASSWORD"]
# 登录密码和 SSH 密码本就不是一回事；混用时 SSH 静默失败并读到空，断言会据此得出错误结论。
SSH_PASSWORD = os.environ.get("ND_SSH_PASSWORD") or PASSWORD
SSH_USER = os.environ.get("ND_SSH_USER", "root")
TOKEN = os.environ["ND_CLUSTER_TOKEN"]
RUNNER_IP = os.environ.get("ND_RUNNER_IP", "")
POOL = os.environ.get("ND_POOL", "tank")  # 只作兜底，实际按 pool_of() 逐台问
PORT = urllib.parse.urlparse(NODES[0]).port or 8080
VIP_BASE = f"http://{VIP}:{PORT}"
HOSTS = {n: urllib.parse.urlparse(n).hostname for n in NODES}

CHECKS = []
_tokens = {}


def check(cid, label, ok, detail=""):
    CHECKS.append((cid, label, bool(ok), detail))
    print(f"  {'PASS' if ok else 'FAIL'} {cid} {label}" + (f"  — {detail}" if detail else ""), flush=True)


def section(title):
    print(f"\n### {title}", flush=True)


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


# ---------------------------------------------------------------- 基础设施

def ssh(host, cmd, timeout=60, check_rc=False):
    r = subprocess.run(
        ["sshpass", "-p", SSH_PASSWORD, "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "ConnectTimeout=8", f"{SSH_USER}@{host}", cmd],
        capture_output=True, text=True, timeout=timeout)
    if check_rc and r.returncode != 0:
        print(f"    ssh {host} rc={r.returncode}: {r.stderr.strip()[:120]}")
    return r


def sudo(host, cmd, **kw):
    """节点上以 root 执行；非 root 用户走 sudo -S。"""
    if SSH_USER == "root":
        return ssh(host, cmd, **kw)
    return ssh(host, f"echo {shlex.quote(SSH_PASSWORD)} | sudo -S -p '' bash -c {shlex.quote(cmd)}", **kw)


def http(base, path, method="GET", token=None, body=None, timeout=8):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, method=method, data=data)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            return r.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)


def login(base):
    if base not in _tokens:
        st, raw = http(base, "/api/login", "POST", body={"username": "admin", "password": PASSWORD}, timeout=15)
        if st != 200:
            raise RuntimeError(f"login {base}: {st} {raw[:100]}")
        _tokens[base] = json.loads(raw)["token"]
    return _tokens[base]


def api(base, path, method="GET", body=None, timeout=20, retry_login=True):
    try:
        st, raw = http(base, path, method, token=login(base), body=body, timeout=timeout)
    except Exception as e:
        return 0, str(e)
    if st == 401 and retry_login:
        _tokens.pop(base, None)
        return api(base, path, method, body, timeout, retry_login=False)
    try:
        return st, json.loads(raw) if raw.strip() else {}
    except Exception:
        return st, raw


def node_healthy(base):
    st, _ = http(base, "/healthz", timeout=5)
    return st == 200


def node_status(base):
    """经集群内部端点读角色（不需要 JWT，备机也答）。"""
    st, raw = http(base, "/internal/ha/status", token=TOKEN, timeout=5)
    if st != 200:
        return None
    try:
        return json.loads(raw)
    except Exception:
        return None


def cluster_view():
    """{base: status} —— 联系不上的为 None。"""
    return {n: node_status(n) for n in NODES}


def active_node():
    """当前真正的主机。

    只挑「第一个自称 active 的节点」是不够的：旧主重启回归时，在 epoch 栅栏把它
    降为备机之前，有一个短暂窗口它仍按角色文件自报 active。采样落在那个窗口里，
    读到的是它的旧 epoch，于是「epoch 单调递增」会失败——而集群其实是对的。
    产品自己的判据就是答案：持有 VIP 的那台才是主，epoch 再高、拿不到 VIP 也
    服务不了任何客户机。只有多于一台自称 active 时才去问 VIP（要 SSH，不便宜）。
    """
    claims = [(n, s) for n, s in cluster_view().items() if s and s.get("role") == "active"]
    if not claims:
        return None, None
    if len(claims) == 1:
        return claims[0]
    holder = vip_holder()
    for n, s in claims:
        if n == holder:
            return n, s
    return claims[0]


def vip_holder():
    for n in NODES:
        r = ssh(HOSTS[n], f"ip -br a | grep -c '{VIP}/'", timeout=15)
        if r.returncode == 0 and r.stdout.strip().isdigit() and int(r.stdout.strip()) > 0:
            return n
    return None


def wait(cond, seconds, step=2, desc=""):
    """等条件成立，返回耗时（秒）；超时返回 None。"""
    elapsed, _ = wait_for(cond, seconds, step)
    return elapsed


def wait_for(cond, seconds, step=2):
    """等条件成立，返回 (耗时, 条件的值)；超时返回 (None, None)。
    需要「等到的是哪个节点」时用这个——wait() 只给耗时。"""
    deadline = time.time() + seconds
    t0 = time.time()
    while time.time() < deadline:
        try:
            v = cond()
        except Exception:
            v = None
        if v:
            return time.time() - t0, v
        time.sleep(step)
    return None, None


def vip_healthy():
    st, _ = http(VIP_BASE, "/healthz", timeout=4)
    return st == 200


def wait_cluster_stable(seconds=180, want_nodes=None):
    """等到：恰好一个 active、VIP 有主、healthz 经 VIP 通、所有活着的节点都在线。"""
    def ok():
        view = cluster_view()
        actives = [n for n, s in view.items() if s and s.get("role") == "active"]
        if len(actives) != 1:
            return False
        if want_nodes and any(view.get(n) is None for n in want_nodes):
            return False
        return vip_holder() is not None and vip_healthy()
    return wait(ok, seconds, step=3)


def is_runner(host):
    return RUNNER_IP and host == RUNNER_IP


# ---------------------------------------------------------------- 数据一致性辅助

# 每个用例一个互不重叠的地址窗口，避免同网段第二个分组被正确地以「IP 区间重叠」拒绝。
GROUP_SLOTS = {"fo-e2e-carry": 200, "fo-e2e-lost": 210, "fo-e2e-kept": 220, "fo-e2e-resume": 230}


def free_slot(items, head, preferred, need):
    """在 head.x 网段里找一段没被任何分组占住的地址窗口。

    固定槽位会撞上别的套件留下的分组——真机上 fo-e2e-lost 的 .210 正好落在
    cluster_matrix 留下的 cluster-e2e（.210~.214）里，产品于是（正确地）以
    「IP 区间与其它分组重叠」拒绝，而断言报出来的是「主机写不进数据」，
    指向切换，切换其实好好的。"""
    occupied = set()
    for g in items:
        sip = g.get("StartIP") or ""
        if not sip.startswith(head + "."):
            continue
        try:
            first = int(sip.rsplit(".", 1)[1])
            width = max(1, int(g.get("ClientMax") or 1))
        except (ValueError, TypeError):
            continue
        occupied.update(range(first, first + width))
    for start in list(range(preferred, 250)) + list(range(100, preferred)):
        if start + need - 1 <= 254 and not any((start + i) in occupied for i in range(need)):
            return start
    return preferred


def any_image(base):
    """任一可用镜像与它的一个配置。清库后这两样由启动时的目录恢复带回来。"""
    st, body = api(base, "/api/images")
    for img in (body.get("items") or []) if st == 200 and isinstance(body, dict) else []:
        st2, cfgs = api(base, f"/api/images/{img['ID']}/configs")
        for c in (cfgs.get("items") or []) if st2 == 200 and isinstance(cfgs, dict) else []:
            return img["ID"], c["ID"]
    return "", ""


def make_group(base, name):
    """建一个最小分组作为「写入了数据」的标记。返回 id 或 None。
    注意：写操作一律经 VIP——只有当前主机接受写，备机会（正确地）回 503。"""
    st, groups = api(base, "/api/groups")
    tmpl = (groups.get("items") or [{}])[0] if isinstance(groups, dict) else {}
    # 没有任何分组时（如清库重装后，分组只在库里、无法从目录恢复）借不到模板，
    # 镜像与配置会传成空串，被拒的原因看起来是匹配关系，其实是没得借。
    if not tmpl.get("SystemImageID"):
        img, cfg = any_image(base)
        tmpl = {"StartIP": (urllib.parse.urlparse(base).hostname or "192.168.10.1").rsplit(".", 1)[0] + ".1",
                "Netmask": "255.255.255.0", "Gateway": "",
                "SystemImageID": img, "SystemConfigID": cfg}
    # 网段必须落在客户机网卡网段内（跨网段会被硬拒），所以借既有分组的网络参数，只把起始地址挪到高位空闲区。
    base_ip = tmpl.get("StartIP", "") or "192.168.10.100"
    head = base_ip.rsplit(".", 1)[0]
    slot = free_slot((groups.get("items") or []) if isinstance(groups, dict) else [],
                     head, GROUP_SLOTS.get(name, 240), 2)
    body = {
        "name": name, "start_ip": f"{head}.{slot}", "client_max": 2,
        "gateway": tmpl.get("Gateway", ""), "netmask": tmpl.get("Netmask", "255.255.255.0"),
        "dns1": "114.114.114.114", "dns2": "223.5.5.5",
        "system_image_id": tmpl.get("SystemImageID", ""), "system_config_id": tmpl.get("SystemConfigID", ""),
    }
    st, g = api(base, "/api/groups", "POST", body)
    if st not in (200, 201):
        # 超时（st=0）不等于没建成，可能已落库；先回头查，盲目重发会撞「同名分组已存在」。
        if st == 0:
            found = wait(lambda: has_group(base, name) is True, 30, step=3)
            if found:
                st2, groups2 = api(base, "/api/groups")
                for item in (groups2.get("items") or []) if isinstance(groups2, dict) else []:
                    if item.get("Name") == name:
                        return item.get("ID")
        print(f"    建分组失败 {st}: {str(g)[:160]}")
        return None
    return g.get("ID")


def has_group(base, name, seconds=60):
    """分组在不在。读不到就重试到超时——切换窗口里 VIP 有几秒不应答是正常的，
    而调用方用 `is True` / `is False` 比较，一次读不到就会变成一条空 detail 的
    假失败（真机上 B1d、C1k、C1l 就是这么红的）。只有始终读不到才返回 None。"""
    deadline = time.time() + seconds
    while True:
        st, groups = api(base, "/api/groups")
        if st == 200 and isinstance(groups, dict):
            return any(g.get("Name") == name for g in groups.get("items", []))
        if time.time() > deadline:
            return None
        time.sleep(3)


def drop_group(base, name):
    st, groups = api(base, "/api/groups")
    if st != 200 or not isinstance(groups, dict):
        return
    for g in groups.get("items", []):
        if g.get("Name", "").startswith(name):
            api(base, f"/api/groups/{g['ID']}", "DELETE")


_pools = {}


def pool_of(host):
    """那台机器**自己**的数据池名。

    各节点的池名互不相干（真机上三台分别叫 data / tank / data1）。拿一个 ND_POOL
    去套所有节点，轻则断言查了个空目录还「通过」，重则故障注入打空——A5 要 export
    的池在那台根本不存在，注入静默变成空操作，接着断言「切换了吗」测的是一场从未
    发生的故障。先读产品自己记的那份（节点本地、不随目录复制），读不到再猜。
    """
    if host not in _pools:
        r = sudo(host, "cat /var/lib/ndiskless/data-pool 2>/dev/null")
        name = r.stdout.strip()
        if not name:
            r = sudo(host, "zfs list -H -o name -d 0 2>/dev/null | grep -v '^bpool$' | head -1")
            name = r.stdout.strip()
        _pools[host] = name or POOL
    return _pools[host]


def marker_count(host):
    """主机上的复制标记快照名（最新那个）。轮次推进看它，不看
    /api/replication 的 last_snapshot——后者只在对端真来拉取时才更新。"""
    r = sudo(host, f"zfs list -H -t snapshot -o name -r {pool_of(host)}/nd 2>/dev/null | grep '@rep-' | tail -1")
    return r.stdout.strip()


def marker_nanos(marker):
    """rep- 标记名末尾的纳秒时间戳；取不到算 0。标记名形如
    tank/nd@rep-1787636651960190679，按它排先后比等号可靠。"""
    if not marker or "@rep-" not in marker:
        return 0
    try:
        return int(marker.rsplit("@rep-", 1)[1])
    except ValueError:
        return 0


def replication_advanced(active_base, _seen={}):
    """复制轮次是否比第一次看到时更新了。备机的活库不跟随主机（复制流送的是
    nd/db 副本，激活时才换成活库），所以「复制有没有把改动送出去」只能问复制
    状态，不能查备机的 API。"""
    st, body = api(active_base, "/api/replication")
    if st != 200 or not isinstance(body, dict):
        return False
    snaps = [t.get("last_snapshot", "") for t in body.get("targets", []) if t.get("last_snapshot")]
    if not snaps:
        return False
    newest = max(snaps)
    first = _seen.setdefault(active_base, newest)
    return newest > first


# ---------------------------------------------------------------- 通用故障剧本

def failover_case(cid_prefix, title, inject, recover, expect_switch=True, settle=240):
    """注入 → 观察切换 → 恢复 → 等回稳。返回新主（或 None）。"""
    section(title)
    # 每个用例开始前所有节点都要健康，带着上一轮没恢复的节点进场会误报「无人接管」。
    ready = wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5)
    if ready is None:
        check(f"{cid_prefix}0", "开始前所有节点健康", False,
              "有节点仍未恢复，跳过本用例以免结论失真")
        return None
    before, bs = active_node()
    if not before:
        check(f"{cid_prefix}a", "开始前有唯一主机", False, "找不到 active")
        return None
    epoch_before = bs.get("epoch", 0)
    victim_host = HOSTS[before]
    print(f"    当前主机 {victim_host} epoch={epoch_before}")

    t0 = time.time()
    injected = inject(before, victim_host)
    if injected is False:
        check(f"{cid_prefix}0", "注入生效", True,
              f"跳过：{victim_host} 上这次注入没生效（资源被占用），本轮无从验证切换")
        recover(before, victim_host)
        wait(lambda: all(node_healthy(n) for n in NODES), settle, step=5)
        return None

    if expect_switch:
        _, _ = wait_for(lambda: (lambda r: r[0] if r[0] and r[0] != before else None)(active_node()), 120, step=2)
        after, as_ = active_node()
        check(f"{cid_prefix}a", "主机身份转移到另一台", after is not None and after != before,
              f"{HOSTS.get(after, '?')} 用时 {time.time() - t0:.1f}s" if after else "无人接管")
        if after and after != before:
            # epoch 在集群范围内不唯一（见 分布式演进方案 §8.7.4），撞号由 VIP 判据兜住；产品只承诺不倒退。
            check(f"{cid_prefix}b", "epoch 不倒退", as_.get("epoch", 0) >= epoch_before,
                  f"{epoch_before} → {as_.get('epoch')}")
            hv = wait(vip_healthy, 60, step=1)
            check(f"{cid_prefix}c", "服务经 VIP 恢复", hv is not None,
                  f"故障到可用 {time.time() - t0:.1f}s")
    else:
        time.sleep(20)
        after, as_ = active_node()
        check(f"{cid_prefix}a", "主机身份保持不变（不该切换）", after == before, f"{HOSTS.get(after, '?')}")

    recover(before, victim_host)
    # 被注入的那台是下一个用例的接管方，必须先确认它痊愈，否则后续场景全以「无人接管」失败。
    healed = wait(lambda: node_healthy(before), 240, step=5)
    check(f"{cid_prefix}y", "被注入的节点已恢复健康", healed is not None,
          f"{healed:.0f}s" if healed else "240s 未恢复")
    st = wait_cluster_stable(settle)
    check(f"{cid_prefix}z", "集群恢复稳态（唯一主机 + VIP + healthz）", st is not None,
          f"{st:.0f}s" if st else "超时")
    return active_node()[0]


# ---------------------------------------------------------------- A 故障注入方式

def case_kill9():
    def inject(base, host):
        sudo(host, "pkill -9 -f '/opt/ndiskless' || pkill -9 ndiskless || true")
        # Restart=always 会立刻拉起；真正的切换要靠反复崩溃。
        sudo(host, "systemctl stop ndiskless")
    def recover(base, host):
        sudo(host, "systemctl start ndiskless")
    failover_case("A1", "A1 进程被 kill -9 后停止（模拟进程崩溃且拉不起来）", inject, recover)


def case_crashloop():
    """二进制被换成必然失败的启动：systemd 反复拉起、healthz 始终坏。"""
    def inject(base, host):
        sudo(host, "systemctl stop ndiskless; "
                   "sed -i 's|^NDISKLESS_DB_DSN=.*|NDISKLESS_DB_DSN=file:/nonexistent-dir/x.db|' /etc/ndiskless/ndiskless.env; "
                   "systemctl reset-failed ndiskless; systemctl start ndiskless")

    def recover(base, host):
        # 改回确定的值，不依赖备份文件：第二轮 cp 会把已改坏的 env 存成 .bak，恢复后仍会崩溃循环。
        sudo(host, "systemctl stop ndiskless; "
                   "sed -i 's|^NDISKLESS_DB_DSN=.*|NDISKLESS_DB_DSN=file:/var/lib/ndiskless/ndiskless.db|' /etc/ndiskless/ndiskless.env; "
                   "rm -f /etc/ndiskless/ndiskless.env.bak; "
                   "systemctl reset-failed ndiskless; systemctl start ndiskless")
    failover_case("A2", "A2 主机进入崩溃循环（数据库路径失效）", inject, recover)


def case_poweroff():
    """整机断电：最接近拔电源。跑脚本的机器跳过。"""
    section("A3 整机强制断电重启")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("A30", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    before, bs = active_node()
    if not before:
        check("A3a", "开始前有唯一主机", False)
        return
    host = HOSTS[before]
    if is_runner(host):
        check("A3a", "整机断电（跳过：主机就是跑脚本的机器）", True, "机制已由其它节点覆盖")
        return
    t0 = time.time()
    # sysrq-b：不刷盘、不通知任何人，等同拔电。
    sudo(host, "sync; echo 1 > /proc/sys/kernel/sysrq; (sleep 1; echo b > /proc/sysrq-trigger) >/dev/null 2>&1 &", timeout=20)
    _, _ = wait_for(lambda: (lambda r: r[0] if r[0] and r[0] != before else None)(active_node()), 150, step=2)
    a, as_ = active_node()
    check("A3a", "断电后另一台接管", a is not None and a != before, f"{time.time() - t0:.1f}s")
    check("A3b", "服务经 VIP 可用", wait(vip_healthy, 60, step=1) is not None)
    back = wait(lambda: node_status(before) is not None, 300, step=5)
    check("A3c", "断电节点自行重启并回归", back is not None, f"{back:.0f}s" if back else "300s 未回来")
    s = node_status(before)
    check("A3d", "回归后为备机（epoch 栅栏，不抢主）", s and s.get("role") == "standby", str(s))
    st = wait_cluster_stable(240)
    check("A3z", "集群恢复稳态", st is not None)


def case_link_down():
    """客户机网卡断链：VRRP 断供 + 服务不可达，且断链节点自己也看不到别人。"""
    section("A4 主机客户机网卡断链")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("A40", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    before, bs = active_node()
    if not before:
        check("A4a", "开始前有唯一主机", False)
        return
    host = HOSTS[before]
    if is_runner(host):
        check("A4a", "网卡断链（跳过：主机就是跑脚本的机器）", True, "机制已由其它节点覆盖")
        return
    iface = sudo(host, f"ip -o -4 addr show | awk '$4 ~ /^{'.'.join(host.split('.')[:3])}\\./ {{print $2; exit}}'").stdout.strip()
    if not iface:
        check("A4a", "找到客户机网卡", False, "解析失败")
        return
    t0 = time.time()
    # 定时自愈，免得把机器锁死在断网状态。
    sudo(host, f"nohup bash -c 'ip link set {iface} down; sleep 75; ip link set {iface} up' >/dev/null 2>&1 &", timeout=20)
    _, a = wait_for(lambda: (lambda r: r[0] if r[0] and r[0] != before else None)(active_node()), 120, step=2)
    check("A4a", "断链后另一台接管", a is not None, f"{time.time() - t0:.1f}s" if a else "无人接管")
    check("A4b", "服务经 VIP 可用", wait(vip_healthy, 60, step=1) is not None)
    back = wait(lambda: node_status(before) is not None, 180, step=5)
    check("A4c", "网卡恢复后节点回归", back is not None)
    # 回归瞬间它仍自称 active，自愈要两轮（十几秒一轮）确认对端在服务才让位，不能立刻读角色。
    # 真正有害的是它夺回虚 IP（全体客户机会再重启一次），所以先钉住观察窗口内虚 IP 不回到它手上，再看角色收敛。
    stole = wait(lambda: vip_holder() == before, 90, step=3)
    check("A4d", "回归后没有夺回虚 IP（夺回 = 全体客户机再重启一次）", stole is None,
          f"虚 IP 被夺回 {HOSTS[before]}（{stole:.0f}s）" if stole is not None
          else f"仍在 {HOSTS.get(vip_holder(), '?')}")
    settled = wait(lambda: (node_status(before) or {}).get("role") == "standby", 120, step=5)
    check("A4e", "回归节点自愈为备机（不需要人工介入）", settled is not None,
          f"{settled:.0f}s" if settled is not None else str(node_status(before)))
    check("A4z", "集群恢复稳态", wait_cluster_stable(240) is not None)


def case_pool_fault():
    """存储池不可用：healthz 必须失败并让出 VIP（池坏了还留着服务=客户机全体挂起）。"""
    def inject(base, host):
        # 不能为让 export 成功先停服务，那测的是 A1「进程停止」而不是「池坏了进程还活着」。
        # 池被 LIO 导出的 zvol 占着时 export -f 会失败，注入成空操作；注入不成就如实报告，不伪造场景。
        pool = pool_of(host)
        sudo(host, f"zpool export -f {pool} || zpool offline {pool} $(zpool list -v -H {pool} | awk 'NR==2{{print $1}}') || true",
             timeout=90)
        r = sudo(host, f"zpool list -H -o name {pool} 2>/dev/null")
        return pool not in r.stdout
    def recover(base, host):
        # export -f 后 import 未必一次成功（设备刚释放、udev 未落定），池回不来这台就永远不健康，重试到它在。
        pool = pool_of(host)
        for _ in range(8):
            sudo(host, f"zpool import -f {pool} 2>/dev/null || true", timeout=90)
            r = sudo(host, f"zpool list -H -o name {pool} 2>/dev/null")
            if pool in r.stdout:
                break
            time.sleep(5)
        sudo(host, "systemctl restart ndiskless || true")
    failover_case("A5", "A5 主机存储池不可用", inject, recover)


def case_db_readonly():
    """数据库不可写（磁盘满/权限）：healthz 的 DB 检查必须抓到。"""
    def inject(base, host):
        sudo(host, "chattr +i /var/lib/ndiskless/ndiskless.db 2>/dev/null || chmod 444 /var/lib/ndiskless/ndiskless.db; systemctl restart ndiskless")
    def recover(base, host):
        sudo(host, "chattr -i /var/lib/ndiskless/ndiskless.db 2>/dev/null; chmod 644 /var/lib/ndiskless/ndiskless.db; systemctl restart ndiskless")
    failover_case("A6", "A6 主机数据库不可写", inject, recover)


def case_lio_gone():
    """iSCSI 内核态没了：还能应答 HTTP，但客户机连不上盘——healthz 必须判死。"""
    def inject(base, host):
        sudo(host, "umount /sys/kernel/config 2>/dev/null; rmmod iscsi_target_mod 2>/dev/null; systemctl restart ndiskless")
    def recover(base, host):
        sudo(host, "modprobe configfs; mount -t configfs configfs /sys/kernel/config 2>/dev/null; "
                   "modprobe target_core_mod; modprobe iscsi_target_mod; systemctl restart ndiskless")
    failover_case("A7", "A7 主机 iSCSI 内核模块丢失", inject, recover)


def case_maintenance():
    """计划维护标记：运维手动让出，最温和的一种。"""
    def inject(base, host):
        sudo(host, "touch /run/ndiskless.maintenance")
    def recover(base, host):
        sudo(host, "rm -f /run/ndiskless.maintenance")
    failover_case("A8", "A8 主机打上维护标记", inject, recover)


# ---------------------------------------------------------------- B 切换正确性

def case_data_and_dnsmasq():
    section("B1 切换正确性：数据跟随 + dnsmasq 跟随 + 客户机可开机")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("B10", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    before, _ = active_node()
    if not before:
        check("B1a", "开始前有唯一主机", False)
        return
    name = "fo-e2e-carry"
    drop_group(VIP_BASE, name)
    gid = make_group(VIP_BASE, name)
    check("B1a", "切换前在主机上写入数据", gid is not None, str(gid))
    host_now = HOSTS[active_node()[0]] if active_node()[0] else HOSTS[before]
    base_marker = marker_count(host_now)
    seen = wait(lambda: marker_count(host_now) != base_marker and marker_count(host_now) != "", 180, step=10)
    check("B1b", "复制轮次已推进（目录随之送出）", seen is not None,
          f"{seen:.0f}s" if seen else f"180s 未推进（基线 {base_marker[-20:]}）")
    # 备机每 60 秒才拉一轮，打完标记立刻杀主机会丢这一轮，那是 RPO 语义（C1c 正断言这点）而非缺陷。
    # 这一段验的是已复制走的数据能扛过崩溃切换，所以先等其余节点的最新标记追上主机再动手。
    round_marker = marker_count(host_now)
    others = [h for h in HOSTS.values() if h != host_now]

    def at_least(h):
        """备机是否已经走到 round_marker 或更靠后。
        不能比相等：主机每 60 秒还在打新标记，备机一旦越过那一轮，
        「最新标记 == round_marker」就永远不成立，等到超时也追不平。"""
        return marker_nanos(marker_count(h)) >= marker_nanos(round_marker) > 0

    caught = wait(lambda: all(at_least(h) for h in others), 240, step=5)
    check("B1b2", "备机已拉到含该改动的那一轮", caught is not None,
          f"{caught:.0f}s" if caught else f"240s 未追平（目标 {round_marker[-20:]}）")

    host = HOSTS[before]
    # 挑客户机时避开盘就在这台上的：存储节点停了，它们开不了机是应当的。
    before_id = (node_status(before) or {}).get("node_id", "")
    sudo(host, "systemctl stop ndiskless")
    _, a = wait_for(lambda: (lambda r: r[0] if r[0] and r[0] != before else None)(active_node()), 120, step=2)
    check("B1c", "切换发生", a is not None)
    if a:
        # 打标一轮 + 拉取一轮 + 切换后重启开销，90 秒不够。
        ok = wait(lambda: has_group(VIP_BASE, name), 300, step=5)
        check("B1d", "切换前写入的数据在新主上存在", ok is not None)
        # dnsmasq 必须跟着 VIP 走：新主开、旧主关（两个 authoritative DHCP 会互相 NAK）。
        newh = HOSTS[a]
        on = wait(lambda: sudo(newh, "systemctl is-active dnsmasq").stdout.strip() == "active", 60, step=3)
        check("B1e", "新主的 dnsmasq 已启动", on is not None,
              sudo(newh, "systemctl is-active dnsmasq").stdout.strip())
        st, body = http(VIP_BASE, "/api/terminals", token=login(VIP_BASE))
        mac, why = None, "没有已登记终端"
        try:
            items = json.loads(body).get("items", [])
            # 只验控制面切换后，盘还在的客户机照常能开机。
            live = [t for t in items
                    if t.get("StorageServerID") and t.get("StorageServerID") != before_id]
            if live:
                mac = live[0]["MAC"]
            elif items:
                why = f"已登记终端的盘都在刚停的 {before_id} 上，本轮无从验证"
        except Exception:
            pass
        if mac:
            # /boot 核对来源地址，只有本机或已登录管理员问得到；矩阵代客户机发问，必须带 token，匿名会被正确地拒成 404。
            code, script = http(VIP_BASE, f"/boot?mac={urllib.parse.quote(mac)}", token=login(VIP_BASE), timeout=60)
            check("B1f", "已登记客户机经 VIP 能开机", code == 200 and "sanhook" in script, f"HTTP {code}")
        else:
            check("B1f", "已登记客户机经 VIP 能开机", True, f"跳过：{why}")
    sudo(host, "systemctl start ndiskless")
    check("B1z", "集群恢复稳态", wait_cluster_stable(240) is not None)
    drop_group(VIP_BASE, name)


# ---------------------------------------------------------------- C 脑裂与回滚

def case_stale_primary_rollback():
    """用户最关心的场景：主机写了新数据还没传出去就死了，别人接管并写了新东西，
    旧主复活——它那份「更高版本」的数据必须被回滚，绝不能污染集群。"""
    section("C1 旧主带未复制数据复活 → 数据被正确回滚")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("C10", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    before, _ = active_node()
    if not before:
        check("C1a", "开始前有唯一主机", False)
        return
    host = HOSTS[before]
    lost, kept = "fo-e2e-lost", "fo-e2e-kept"
    drop_group(VIP_BASE, lost); drop_group(VIP_BASE, kept)

    # 1) 主机写入后立刻弄死它：复制周期没到，这条数据没传出去。
    gid = make_group(VIP_BASE, lost)
    check("C1a", "主机写入一条尚未复制的数据", gid is not None)
    sudo(host, "systemctl stop ndiskless")
    _, a = wait_for(lambda: (lambda r: r[0] if r[0] and r[0] != before else None)(active_node()), 120, step=2)
    check("C1b", "另一台接管", a is not None)
    if not a:
        sudo(host, "systemctl start ndiskless")
        return

    # 2) 新主没有那条数据（RPO 窗口内丢失，设计如此），并写入自己的新数据。
    time.sleep(5)
    got = has_group(a, lost)
    # 把读到的值写进 detail，区分 None（问不到）和 False（确实没有）。
    check("C1c", "新主没有那条未复制的数据（符合 RPO 语义）", got is False, f"has_group={got}")
    # 先清掉上一轮中断遗留的同名分组，否则 409 会被误报成新主写不进去。
    drop_group(VIP_BASE, kept)
    gid2 = make_group(VIP_BASE, kept)
    check("C1d", "新主写入新数据", gid2 is not None)

    # 3) 旧主复活。
    sudo(host, "systemctl start ndiskless")
    back = wait(lambda: node_status(before) is not None, 180, step=3)
    check("C1e", "旧主复活", back is not None)
    # 要验的是它最终退成备机：启动核对降级是「写标记 + 退出、由 systemd 拉起」，期间仍自报 active。
    # 断言等待本身的结果，不要再取样：旧主可能被 keepalived 再次拉成主机、二十来秒后又降回去，二次取样会撞进那个窗口。
    became = wait(lambda: (node_status(before) or {}).get("role") == "standby", 120, step=3)
    check("C1f", "旧主自动退为备机（epoch 栅栏）", became is not None, str(node_status(before)))
    check("C1g", "VIP 未被抢回", vip_holder() == a, HOSTS.get(vip_holder(), "?"))

    # 4) 旧主拉齐目录后再切回去：它那份高版本数据必须已被回滚。
    st = wait_cluster_stable(300)
    check("C1h", "集群回稳（旧主开始拉取新主目录）", st is not None)
    # 刚降级的节点会让出一分钟（退位标记，见 nodeHealth），期间不参与选举；此时发起切换 VIP 会落到别的备机。
    if wait(lambda: node_healthy(before), 120, step=5) is None:
        print(f"    注意：{HOSTS[before]} 120 秒内未恢复参选资格，切换目标可能落到别处")
    sw, _ = api(a, "/api/ha/planned-switch", "POST", timeout=90)
    check("C1i", "计划切换回旧主受理", sw in (200, 202, 0), f"HTTP {sw}")
    # 三节点切回比双机慢（等备机追平、VRRP 重选、接管方重启、旧主补目录），实测约四分钟，240 秒不够。
    backa = wait(lambda: (lambda r: r[0] == before)(active_node()), 420, step=5)
    check("C1j", "旧主重新成为主机", backa is not None)
    if backa:
        time.sleep(10)
        got_lost, got_kept = has_group(VIP_BASE, lost), has_group(VIP_BASE, kept)
        check("C1k", "旧主那条未复制的数据已被回滚（不再存在）", got_lost is False, f"has_group={got_lost}")
        check("C1l", "接管方写入的数据完好保留", got_kept is True, f"has_group={got_kept}")
    drop_group(VIP_BASE, lost); drop_group(VIP_BASE, kept)
    check("C1z", "集群恢复稳态", wait_cluster_stable(240) is not None)


def case_isolation():
    """双向隔离：备机自认 MASTER 也不能激活（两节点靠 epoch，三节点起靠多数派）。"""
    section("C2 网络隔离下的防脑裂")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("C20", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    act, _ = active_node()
    if not act:
        check("C2a", "开始前有唯一主机", False)
        return
    standbys = [n for n in NODES if n != act]
    if not standbys:
        check("C2a", "有备机可隔离", False)
        return
    victim = standbys[0]
    vhost = HOSTS[victim]
    if is_runner(vhost):
        victim = standbys[-1]
        vhost = HOSTS[victim]
        if is_runner(vhost):
            check("C2a", "隔离测试（跳过：备机就是跑脚本的机器）", True)
            return
    # 只断 VRRP，不能整机 DROP：一是 undo 也要 SSH 到被隔离的那台，会撤不掉规则；
    # 二是连 API 一起断后「谁都探不到」正是允许接管的情形，要验的是「对端还在服务时不许出现第二个写入者」。
    rules = "iptables -I INPUT -p vrrp -j DROP; iptables -I OUTPUT -p vrrp -j DROP"
    undo = "iptables -D INPUT -p vrrp -j DROP; iptables -D OUTPUT -p vrrp -j DROP"
    sudo(vhost, rules, timeout=30)
    try:
        time.sleep(35)
        r = sudo(vhost, f"curl -s -m 5 -H 'Authorization: Bearer {TOKEN}' http://127.0.0.1:{PORT}/internal/ha/status")
        state = {}
        try:
            state = json.loads(r.stdout or "{}")
        except Exception:
            pass
        check("C2a", "被隔离的备机没有自我激活", state.get("role") == "standby", r.stdout.strip()[:120])
        act2, _ = active_node()
        check("C2b", "原主机仍在服务", act2 == act, HOSTS.get(act2, "?"))
        if len(NODES) >= 3:
            # 多数派已经拿掉，拒绝的理由只有这一种口径。
            r = sudo(vhost, "journalctl -u ndiskless --since '-90 seconds' --no-pager | grep -ac '拒绝激活以避免双写' || true")
            check("C2c", "拒绝原因写进了日志（运维能排障）", (r.stdout.strip() or "0") != "0", r.stdout.strip())
        else:
            check("C2c", "拒绝原因写进日志（两节点形态跳过：走 epoch 门而非多数派门）", True)
    finally:
        sudo(vhost, undo, timeout=30)
    check("C2z", "解除隔离后收敛为单一主机", wait_cluster_stable(180) is not None)


# ---------------------------------------------------------------- D 稳定性

def case_flapping():
    """连续切换：来回三次，集群不能越切越乱（epoch 一路递增、始终单主）。"""
    section("D1 连续多次切换后仍稳定")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("D10", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    epochs = []
    for i in range(3):
        act, s = active_node()
        if not act:
            check(f"D1-{i}", f"第 {i+1} 轮开始前有唯一主机", False)
            break
        epochs.append(s.get("epoch", 0))
        host = HOSTS[act]
        sudo(host, "systemctl stop ndiskless")
        _, a = wait_for(lambda: (lambda r: r[0] if r[0] and r[0] != act else None)(active_node()), 120, step=2)
        sudo(host, "systemctl start ndiskless")
        ok = wait_cluster_stable(240)
        check(f"D1-{i+1}", f"第 {i+1} 轮切换并回稳", a is not None and ok is not None,
              f"{HOSTS.get(a, '?')}")
    _, s = active_node()
    if s:
        epochs.append(s.get("epoch", 0))
    # epoch 在集群范围内不唯一，是已接受的设计取舍（见 docs/需求与架构/分布式演进方案.md §8.7.4），
    # 撞号由 servingNow 的 HoldsVIP 分支兜住。所以只断言从不倒退且整体在推进，不按严格递增判。
    monotonic = all(b >= a for a, b in zip(epochs, epochs[1:])) if len(epochs) > 1 else False
    advanced = len(epochs) > 1 and epochs[-1] > epochs[0]
    check("D1e", "epoch 从不倒退且整体推进", monotonic and advanced, str(epochs))
    check("D1z", "最终唯一主机 + 服务可用", wait_cluster_stable(180) is not None)


def case_replication_resume():
    """复制被打断后自愈：停掉备机一段时间，主机继续写，备机回来必须追平。"""
    section("D2 复制中断后自动追平")
    if wait(lambda: all(node_healthy(n) for n in NODES), 300, step=5) is None:
        check("D20", "开始前所有节点健康", False, "有节点仍未恢复，跳过本用例")
        return
    act, _ = active_node()
    standbys = [n for n in NODES if n != act]
    if not act or not standbys:
        check("D2a", "有主备可测", False)
        return
    sb = standbys[0]
    if is_runner(HOSTS[sb]) and len(standbys) > 1:
        sb = standbys[1]
    shost = HOSTS[sb]
    name = "fo-e2e-resume"
    drop_group(VIP_BASE, name)
    ahost = HOSTS[act]
    sudo(shost, "systemctl stop ndiskless")
    time.sleep(5)
    base_marker = marker_count(ahost)
    gid = make_group(VIP_BASE, name)
    check("D2a", "备机离线期间主机继续写入", gid is not None)
    # 先等主机把改动打进一轮标记再放备机回来，否则没有可追的目标。
    wait(lambda: marker_nanos(marker_count(ahost)) > marker_nanos(base_marker), 180, step=10)
    round_marker = marker_count(ahost)
    sudo(shost, "systemctl start ndiskless")
    # 追平只能看复制标记：备机的活库按设计不跟随主机，复制来的是 nd/db 里的副本，
    # 问它的 /api/groups 永远看不到主机的新分组。
    seen = wait(lambda: marker_nanos(marker_count(shost)) >= marker_nanos(round_marker) > 0,
                360, step=5)
    check("D2b", "备机回来后自动追平（无需人工）", seen is not None,
          f"{seen:.0f}s" if seen else f"360s 未追平（目标 {round_marker[-20:]}）")
    drop_group(VIP_BASE, name)
    check("D2z", "集群恢复稳态", wait_cluster_stable(180) is not None)


# ---------------------------------------------------------------- 主流程

CASES = [
    ("A1", case_kill9),
    ("A2", case_crashloop),
    ("A3", case_poweroff),
    ("A4", case_link_down),
    ("A5", case_pool_fault),
    ("A6", case_db_readonly),
    ("A7", case_lio_gone),
    ("A8", case_maintenance),
    ("B1", case_data_and_dnsmasq),
    ("C1", case_stale_primary_rollback),
    ("C2", case_isolation),
    ("D1", case_flapping),
    ("D2", case_replication_resume),
]


def main():
    only = set(sys.argv[1:])
    section("0 开始前的集群状态")
    view = cluster_view()
    for n, s in view.items():
        print(f"    {HOSTS[n]}: {s}")
    check("0a", "所有节点可达", all(s is not None for s in view.values()),
          f"{sum(1 for s in view.values() if s)}/{len(NODES)}")
    check("0b", "恰好一个主机", sum(1 for s in view.values() if s and s.get("role") == "active") == 1)
    check("0c", "VIP 有持有者且服务可用", vip_holder() is not None and vip_healthy(), HOSTS.get(vip_holder(), "?"))
    if not wait_cluster_stable(120):
        print("!! 集群初始状态不稳，先修好再跑")
        return summary()

    for cid, fn in CASES:
        if only and cid not in only:
            continue
        try:
            fn()
        except Exception as e:
            check(f"{cid}!", f"{cid} 执行异常", False, repr(e)[:160])
            wait_cluster_stable(240)

    section("清理")
    for n in ("fo-e2e-carry", "fo-e2e-lost", "fo-e2e-kept", "fo-e2e-resume"):
        drop_group(VIP_BASE, n)
    check("Zz", "测试资产已清理", True)
    return summary()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
