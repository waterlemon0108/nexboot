#!/usr/bin/env python3
"""拓扑分级套件：把同一批机器依次拉成单机 → 双机 → 三机，每一级跑该级该有的检查。

为什么要按这个顺序走，而不是在一个三节点集群上把三种形态"模拟"出来：
产品只支持往上加节点，没有摘节点的操作；而升级路径本身正是最容易出事的地方——
第二台加入时，花名册里只有第一台的那一行曾被当成"本机的旧身份"，新节点会把它
改名、夺走它的池、再删掉它，而且那段代码在外层事务里又开了一个事务，SQLite
单连接下不报错、直接挂死。这条路径只有真的走一遍才测得到。

    ND_HOSTS          三台的地址，按安装顺序（10.0.0.3,10.0.0.4,10.0.0.5）
    ND_VIP            虚 IP
    ND_PASSWORD       管理员密码
    ND_SSH_PASSWORD   系统账号密码（默认同 ND_PASSWORD）
    ND_CLUSTER_TOKEN  集群令牌
    ND_BINARY         节点上二进制的路径（默认 /tmp/ndiskless-new）
    ND_POOL_DISK      建池用的盘（默认 /dev/sdb）
    ND_STAGES         要跑哪几级，默认 1,2,3
    ND_SSH_USER       root

破坏性：会在每台上重装并重建数据池。只对测试机跑。
"""
import os
import shlex
import subprocess
import sys
import time

HOSTS = [h.strip() for h in os.environ.get("ND_HOSTS", "").split(",") if h.strip()]
VIP = os.environ.get("ND_VIP", "")
PASSWORD = os.environ["ND_PASSWORD"]
# 管理员口令和系统账号口令不同。混用时 sudo 被拒只表现为返回空，脚本会把没执行的清理当成已执行。
SSH_PASSWORD = os.environ.get("ND_SSH_PASSWORD") or PASSWORD
TOKEN = os.environ.get("ND_CLUSTER_TOKEN", "e2e-cluster-token")
BINARY = os.environ.get("ND_BINARY", "/tmp/ndiskless-new")
POOL_DISK = os.environ.get("ND_POOL_DISK", "/dev/sdb")
POOL = os.environ.get("ND_POOL", "tank")
SSH_USER = os.environ.get("ND_SSH_USER", "root")
PORT = os.environ.get("ND_PORT", "8080")
STAGES = [int(s) for s in os.environ.get("ND_STAGES", "1,2,3").split(",") if s.strip()]
# 开机链路（含超管保存）也要纳入分级套件，否则「超管保存落在备机上」这类缺陷测不到。
WINDOWS_SOURCE = os.environ.get("ND_WINDOWS_SOURCE", "")
BACKUP_POOL = os.environ.get("ND_BACKUP_POOL", "tank2")
IMPORT_DIR = os.environ.get("ND_IMPORT_DIR", "/var/lib/ndiskless/imports")
SCRIPTS = os.environ.get("ND_SCRIPTS", "/opt/ndiskless/scripts")
E2E_DIR = os.environ.get("ND_E2E_DIR", f"/home/{SSH_USER}/e2e")

JWT = None


def ssh(hostname, cmd, timeout=900):
    if SSH_USER != "root":
        cmd = f"echo {shlex.quote(SSH_PASSWORD)} | sudo -S -p '' bash -c {shlex.quote(cmd)}"
    return subprocess.run(
        ["sshpass", "-p", SSH_PASSWORD, "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "PubkeyAuthentication=no", "-o", "ConnectTimeout=10",
         f"{SSH_USER}@{hostname}", cmd],
        capture_output=True, text=True, timeout=timeout)


def say(msg):
    print(msg, flush=True)


def wipe(hostname):
    """回到装机前的样子：清掉目录库、配置和 keepalived。留着旧库的话，测的就不是
    「新装一台」而是「重装一台」，而升级路径上的问题恰恰出在花名册里已经有谁的时候。

    数据池留着不动。镜像、配置、还原点本来就住在池里，服务启动时会把它们重新
    认领回来（RecoverCatalogue），所以没必要为了清一张表去重导几十 G 的镜像；
    分组、终端、用户这三样恢复不了，而它们恰好是每个矩阵自己建、自己清的。

    但 <池>/nd/db 里那份目录库副本必须一起清掉。它是复制器用 VACUUM INTO 写下的
    整库快照，随池存活——只删 /var/lib/ndiskless 的话，节点接管时会从池里把它恢复
    回来，连同一份**过期的花名册**：已经拆掉的机器又出现在集群里，而且看起来在线。
    这不是产品缺陷（那份副本正是为了灾难恢复而存在的），但它意味着「删掉数据目录」
    不等于「把这台机器恢复成出厂状态」。"""
    ssh(hostname, "systemctl stop ndiskless keepalived 2>/dev/null; "
                  "systemctl disable keepalived 2>/dev/null; "
                  "rm -rf /var/lib/ndiskless /etc/ndiskless; "
                  f"rm -f /{POOL}/nd/db/*.db /{POOL}/nd/db/*.db-wal /{POOL}/nd/db/*.db-shm 2>/dev/null; "
                  "true")


def install(hostname, *, first, role=None, peer=None, node_addr=None, create_pool=True,
            keepalived=True):
    args = [f"bash {SCRIPTS}/install-go.sh", f"--binary {BINARY}", f"--addr :{PORT}",
            f"--bootstrap-password {shlex.quote(PASSWORD)}"]
    # 池还在就直接指过去；只有真的没有池时才建（第一次在空机器上跑）。
    if create_pool and not pool_exists(hostname):
        args += [f"--create-pool {POOL}", f"--pool-disks {POOL_DISK}"]
    else:
        args += [f"--pool {POOL}"]
    if not first:
        args += [f"--cluster-token {shlex.quote(TOKEN)}",
                 f"--ha-role {role}", f"--peer {peer}", f"--node-addr {node_addr}"]
        # 所有节点都参与 VRRP，否则不参选的节点在主备都挂时也无法接管；承载客户机的盘由放置决定，与写入者无关。
        if keepalived:
            args += [f"--vip {VIP}"]
        if JWT:
            args += [f"--jwt-secret {shlex.quote(JWT)}"]
    cmd = " ".join(args)
    say(f"  [{hostname}] {cmd}")
    r = ssh(hostname, cmd)
    tail = "\n".join((r.stdout + r.stderr).strip().splitlines()[-4:])
    say(f"    -> rc={r.returncode}\n    {tail}")
    return r.returncode == 0


def pool_exists(hostname):
    return ssh(hostname, f"zpool list -H -o name {POOL} 2>/dev/null").stdout.strip() == POOL


def jwt_of(hostname):
    return ssh(hostname, "grep -oP 'NDISKLESS_JWT_SECRET=\\K.*' /etc/ndiskless/ndiskless.env").stdout.strip()


def joined(base, want, seconds=240):
    """等花名册收敛到 want 台。

    healthz 通了只说明进程起来了，不代表它已经在集群里：注册和心跳是启动之后
    才发生的。差这一步就开跑，看到的是「少一台」，而由此连锁出来的每一条失败
    ——聚合少一个节点、按节点看盘看不到、跨节点操作 404「不在花名册里」——
    都指向别处，排查会走很远的弯路。"""
    import json as _json
    import urllib.request as _u
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            req = _u.Request(base + "/api/login", method="POST",
                             data=_json.dumps({"username": "admin", "password": PASSWORD}).encode(),
                             headers={"Content-Type": "application/json"})
            with _u.urlopen(req, timeout=10) as r:
                tok = _json.load(r)["token"]
            req = _u.Request(base + "/api/cluster/nodes")
            req.add_header("Authorization", "Bearer " + tok)
            with _u.urlopen(req, timeout=10) as r:
                items = _json.load(r).get("items") or []
            if len(items) >= want and all(n.get("online") for n in items):
                return items
        except Exception:
            pass
        time.sleep(5)
    return None


def healthy(hostname, seconds=120):
    """服务起来了没有——能应答就算。

    /healthz 回答的是「该不该把虚 IP 给这台」，不是「进程活着吗」。新装的备机
    在收到第一份数据库副本之前会如实回答 503：它确实顶不上，不该参选。但它
    装好了、在监听了，这里要判的正是这件事。按 200 判就等于要求「装完立刻就
    能当主机」，而那要等两轮复制。"""
    for _ in range(seconds // 3):
        r = ssh(hostname, f"curl -s -m 3 -o /dev/null -w '%{{http_code}}' http://127.0.0.1:{PORT}/healthz")
        code = r.stdout.strip()
        if code and code != "000":
            return True
        time.sleep(3)
    return False


def ready(hostname, seconds=240):
    """够格参选了没有——必须 200。

    矩阵要跑故障切换，前提是备机真的顶得上：数据库副本已经复制到位。这要等
    主机打一轮标记、备机再拉一轮，稳态下约两分钟。"""
    for _ in range(seconds // 3):
        r = ssh(hostname, f"curl -s -m 3 -o /dev/null -w '%{{http_code}}' http://127.0.0.1:{PORT}/healthz")
        if r.stdout.strip() == "200":
            return True
        time.sleep(3)
    return False


def run_matrix(runner_host, script, env):
    """矩阵在集群内部跑：节点之间要能直连，从外面经端口转发过来是够不着的。"""
    exports = " ".join(f"{k}={shlex.quote(str(v))}" for k, v in env.items())
    r = ssh(runner_host, f"cd {E2E_DIR} && {exports} python3 {script}", timeout=2400)
    out = (r.stdout + r.stderr).strip()
    say(out)
    return r.returncode, out


def stage_env(bases):
    # ND_SSH_PASSWORD 必须传下去，否则矩阵里的 SSH 退回管理员口令，读对端状态全为空，断言会得出错误结论。
    return {"ND_NODES": ",".join(bases), "ND_VIP": VIP, "ND_PASSWORD": PASSWORD,
            "ND_SSH_PASSWORD": SSH_PASSWORD, "ND_CLUSTER_TOKEN": TOKEN,
            "ND_POOL": POOL, "ND_SSH_USER": SSH_USER,
            "ND_BASE": bases[0], "ND_SSH_HOST": HOSTS[0],
            # 开机与备份矩阵需要能过体检的 Windows 镜像源，没有就跳过（它们自带前置守卫）。
            "ND_WINDOWS_SOURCE": WINDOWS_SOURCE,
            "ND_BACKUP_POOL": BACKUP_POOL,
            "ND_IMPORT_DIR": IMPORT_DIR,
            # 告诉矩阵它跑在 HOSTS[0] 上：failover_matrix 会断电、断网，不知道自己在哪台会把自己也打断而挂住。
            "ND_RUNNER_IP": HOSTS[0]}


def preflight():
    """每台都要能 sudo。不先验这一条的话，wipe 会被静默拒绝，装机跑在没清干净的
    机器上，而日志看起来一切正常——这正是第一次跑挂掉的方式。"""
    for h in HOSTS:
        r = ssh(h, "id -u", timeout=40)
        if r.stdout.strip() != "0":
            raise SystemExit(f"{h}: 拿不到 root（sudo 被拒？）rc={r.returncode} "
                             f"out={r.stdout.strip()!r} err={(r.stderr or '').strip()[:120]!r}")
        if not ssh(h, f"test -f {SCRIPTS}/install-go.sh && echo yes").stdout.strip():
            raise SystemExit(f"{h}: 找不到 {SCRIPTS}/install-go.sh")
        if not ssh(h, f"test -f {BINARY} && echo yes").stdout.strip():
            raise SystemExit(f"{h}: 找不到待安装的二进制 {BINARY}")
    say("  预检通过：三台都能 sudo，安装脚本与二进制就位")


def main():
    global JWT
    if len(HOSTS) < 3 or not VIP:
        raise SystemExit("ND_HOSTS 需要三台，ND_VIP 必填")
    preflight()
    base = [f"http://{h}:{PORT}" for h in HOSTS]
    failures = []

    def record(stage, name, rc):
        status = "通过" if rc == 0 else "失败"
        say(f"\n===== 阶段{stage} · {name}: {status} (rc={rc}) =====\n")
        if rc != 0:
            failures.append(f"阶段{stage}/{name}")

    if 1 in STAGES:
        say("\n########## 阶段 1：单机 ##########")
        for h in HOSTS:
            wipe(h)
        if not install(HOSTS[0], first=True):
            raise SystemExit("单机安装失败")
        if not healthy(HOSTS[0]):
            raise SystemExit("单机 healthz 不通")
        JWT = jwt_of(HOSTS[0])
        say(f"  JWT 已取得（{len(JWT)} 字符），后续节点复用")
        record(1, "本机已登记在册", 0 if joined(base[0], 1) else 1)
        env = stage_env(base[:1])
        record(1, "集群管理矩阵", run_matrix(HOSTS[0], "cluster_ops_matrix.py", env)[0])
        record(1, "负载均衡矩阵", run_matrix(HOSTS[0], "balance_matrix.py", env)[0])
        # 开机链路先在单机上跑，此时没有多节点变量，最好定位问题。
        record(1, "开机链路矩阵", run_matrix(HOSTS[0], "boot_matrix.py", env)[0])
        record(1, "本地备份矩阵", run_matrix(HOSTS[0], "backup_matrix.py", env)[0])

    if 2 in STAGES:
        say("\n########## 阶段 2：单机 → 双机 ##########")
        if JWT is None:
            JWT = jwt_of(HOSTS[0])
        # 顺序不能反：先把有数据的老节点改成主机，再装空的新节点。
        say("  2.1 老节点改主机")
        if not install(HOSTS[0], first=False, role="active", peer=",".join(HOSTS[1:]),
                       node_addr=HOSTS[0], create_pool=False):
            raise SystemExit("老节点转主机失败")
        healthy(HOSTS[0])
        say("  2.2 新节点以备机身份加入")
        if not install(HOSTS[1], first=False, role="standby",
                       peer=",".join([HOSTS[0]] + HOSTS[2:]), node_addr=HOSTS[1]):
            raise SystemExit("第二台安装失败")
        # 新节点不能把花名册里唯一的第一台误认成本机旧身份，否则嵌套事务会让它挂死起不来。
        ok = healthy(HOSTS[1], 180)
        record(2, "第二台在 180s 内起得来（不夺权、不挂死）", 0 if ok else 1)
        members = joined(base[0], 2)
        record(2, "两台都在花名册里且在线", 0 if members else 1)
        # 后面的矩阵要做故障切换，先等备机真的顶得上（收到数据库副本）。
        record(2, "备机已具备接管条件（收到数据库副本）", 0 if ready(HOSTS[1]) else 1)
        env = stage_env(base[:2])
        record(2, "集群管理矩阵", run_matrix(HOSTS[0], "cluster_ops_matrix.py", env)[0])
        record(2, "负载均衡矩阵", run_matrix(HOSTS[0], "balance_matrix.py", env)[0])
        record(2, "双机热备矩阵", run_matrix(HOSTS[0], "ha_matrix.py", {
            **env, "ND_NODE_A": base[0], "ND_NODE_B": base[1]})[0])
        record(2, "带客户机的异常切换", run_matrix(HOSTS[0], "failover_matrix.py", {
            **env, "ND_NODE_A": base[0], "ND_NODE_B": base[1]})[0])

    if 3 in STAGES:
        say("\n########## 阶段 3：双机 → 三机 ##########")
        if JWT is None:
            JWT = jwt_of(HOSTS[0])
        # 第三台起的 peer 要指 VIP：VIP 总在写入者上，指某台机器的地址在它变成备机后注册会失败或被复制覆盖。
        # 第三台同样进 VRRP，另外两台都挂时也能接管。
        if not install(HOSTS[2], first=False, role="standby",
                       peer=",".join(HOSTS[:2]), node_addr=HOSTS[2]):
            raise SystemExit("第三台安装失败")
        ok = healthy(HOSTS[2], 180)
        record(3, "第三台在 180s 内起得来", 0 if ok else 1)
        members = joined(base[0], 3)
        record(3, "三台都在花名册里且在线", 0 if members else 1)
        record(3, "第三台已具备接管条件（收到数据库副本）", 0 if ready(HOSTS[2]) else 1)
        env = stage_env(base)
        record(3, "集群管理矩阵", run_matrix(HOSTS[0], "cluster_ops_matrix.py", env)[0])
        record(3, "负载均衡矩阵", run_matrix(HOSTS[0], "balance_matrix.py", env)[0])
        # 三机再跑开机链路：「超管保存必须落在写入者上」只有多节点才能验证。
        record(3, "开机链路矩阵（多节点）", run_matrix(HOSTS[0], "boot_matrix.py", env)[0])
        record(3, "三节点集群矩阵", run_matrix(HOSTS[0], "cluster_matrix.py", {
            **env, "ND_NODE_A": base[0], "ND_NODE_B": base[1], "ND_NODE_C": base[2]})[0])

    say("\n########## 总计 ##########")
    if failures:
        say(f"失败 {len(failures)} 项：")
        for f in failures:
            say(f"  - {f}")
        return 1
    say("全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
