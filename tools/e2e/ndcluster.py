"""Shared client for the multi-node matrices.

`ndapi` talks to one server; these matrices talk to several and have to keep
straight which node answered. Reporting (check/skip/section/summary) is reused
from ndapi so every matrix tallies the same way.

Addressing rule, learned the hard way: fault injection goes to a node's **own**
address, never the VIP. The VIP moves — during exactly the scenarios these
matrices exercise — so a command sent to it lands on whichever node happens to
hold it, which is the one case the test is trying to distinguish.

    ND_NODES          comma-separated node API bases, in install order
                      (http://10.0.0.3:8080,http://10.0.0.4:8080,...)
    ND_VIP            virtual IP, when a keepalived pair exists
    ND_PASSWORD       login password
    ND_SSH_PASSWORD   SSH password (falls back to ND_PASSWORD)
    ND_CLUSTER_TOKEN  cluster token for the /internal/ endpoints
    ND_SSH_USER       root
    ND_POOL           tank
"""
import json
import os
import shlex
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

from ndapi import check, section, skip, summary  # noqa: F401  (one tally for all matrices)

NODES = [n.strip().rstrip("/") for n in os.environ.get("ND_NODES", "").split(",") if n.strip()]
VIP = os.environ.get("ND_VIP", "")
TOKEN = os.environ.get("ND_CLUSTER_TOKEN", "")
PASSWORD = os.environ.get("ND_PASSWORD", "")
SSH_PASSWORD = os.environ.get("ND_SSH_PASSWORD") or PASSWORD
POOL = os.environ.get("ND_POOL", "tank")
SSH_USER = os.environ.get("ND_SSH_USER", "root")
PORT = (urllib.parse.urlparse(NODES[0]).port if NODES else None) or 8080
VIP_BASE = f"http://{VIP}:{PORT}" if VIP else (NODES[0] if NODES else "")

_tokens = {}


def host(base):
    return urllib.parse.urlparse(base).hostname


def login(base):
    if base not in _tokens:
        req = urllib.request.Request(
            base + "/api/login", method="POST",
            data=json.dumps({"username": "admin", "password": PASSWORD}).encode(),
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=15) as r:
            _tokens[base] = json.load(r)["token"]
    return _tokens[base]


def forget_tokens():
    """After a node restarts its JWT secret may differ; stale tokens 401."""
    _tokens.clear()


def call(base, method, path, body=None, timeout=30):
    """One API call against a named node. Returns (status, parsed-or-raw)."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + urllib.parse.quote(path, safe="/?&=%"),
                                 method=method, data=data)
    try:
        req.add_header("Authorization", "Bearer " + login(base))
    except Exception as e:
        return 0, {"error": f"login failed: {e}"}
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip().startswith(("{", "[")) else raw.strip())
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"error": raw}
    except Exception as e:
        return 0, {"error": str(e)}


def node_call(base, method, path, body=None, timeout=30):
    """A peer-facing /internal/ call, authenticated by the cluster token rather
    than a session — the same way one node talks to another."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, method=method, data=data)
    req.add_header("Authorization", "Bearer " + TOKEN)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip().startswith(("{", "[")) else raw.strip())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode().strip()
    except Exception as e:
        return 0, str(e)


def anon_get(base, path, timeout=10):
    """No credentials at all — for asserting that an endpoint refuses."""
    try:
        with urllib.request.urlopen(base + path, timeout=timeout) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)


def anon_post(base, path, body, timeout=15):
    """完全不带凭据的 POST —— 用来断言一个免认证端点该拒绝的时候真的拒绝。"""
    data = json.dumps(body).encode()
    rq = urllib.request.Request(base + path, data=data, method="POST",
                                headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:  # noqa: BLE001
        return 0, str(e)


def ssh(node_host, cmd, timeout=90):
    if SSH_USER != "root":
        cmd = f"echo {shlex.quote(SSH_PASSWORD)} | sudo -S -p '' bash -c {shlex.quote(cmd)}"
    return subprocess.run(
        ["sshpass", "-p", SSH_PASSWORD, "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "PubkeyAuthentication=no", "-o", "ConnectTimeout=8",
         f"{SSH_USER}@{node_host}", cmd],
        capture_output=True, text=True, timeout=timeout)


def ssh_out(node_host, cmd, timeout=90):
    """(读到了吗, 输出)。分开返回是必要的：ssh 本身连不上时 stdout 也是空串，
    而空串会被断言当成「那台机器上什么都没有」——比报错更糟，因为它给出的是一个
    看起来成立的结论。调用方必须能区分「读到了空」和「没读到」。"""
    try:
        r = ssh(node_host, cmd, timeout)
    except Exception as e:
        return False, str(e)
    if r.returncode != 0 and not r.stdout.strip():
        return False, (r.stderr or "").strip()[:160]
    return True, r.stdout.strip()


def entry_base():
    """管理入口：VIP 真的应答就用 VIP，否则用第一台。

    单机形态没有 keepalived，VIP 是个不存在的地址；对它发的请求会一路超时，
    而超时在调用方看来就是「什么都没查到」——矩阵会据此跳过自己，还报成通过。
    探一下再决定，比假设拓扑便宜得多。"""
    if VIP_BASE and NODES and VIP_BASE != NODES[0]:
        st, _ = anon_get(VIP_BASE, "/healthz", timeout=4)
        if st == 200:
            return VIP_BASE
    return NODES[0] if NODES else VIP_BASE


def roster(base):
    st, body = call(base, "GET", "/api/cluster/nodes")
    return {n["id"]: n for n in body.get("items", [])} if st == 200 else {}


def node_id_of(base):
    """A node's own id, asked of the node itself — the roster is not enough:
    it lists every node, and matching by address breaks under a moved VIP.

    Falls back to the session API when no cluster token is configured: a
    single-node install has no reason to have one, and refusing to identify the
    node there would turn a correct configuration into a wall of failures."""
    st, body = node_call(base, "GET", "/internal/ha/status")
    if st == 200 and isinstance(body, dict) and body.get("node_id"):
        return body["node_id"]
    st, body = call(base, "GET", "/api/cluster/pools")
    if st == 200 and isinstance(body, dict):
        for n in body.get("nodes") or []:
            if n.get("is_self"):
                return n.get("node_id", "")
    return ""


def has_cluster_token(base):
    """Does this node accept the token we were given? A standalone install has
    none configured, and then every /internal/ endpoint refuses everyone —
    which is correct, not a defect."""
    st, _ = node_call(base, "GET", "/internal/node/pools")
    return st == 200


def own_pools(base, nid):
    """This node's own pools, whichever door is open. The peer endpoint is the
    real one; the session API is the fallback for a node with no cluster token,
    filtered to this node so the answer means the same thing either way."""
    st, body = node_call(base, "GET", "/internal/node/pools")
    if st == 200 and isinstance(body, dict):
        return True, body.get("items") or []
    st, body = call(base, "GET", "/api/pools")
    if st == 200 and isinstance(body, dict):
        return True, [p for p in (body.get("items") or []) if p.get("ServerID") == nid]
    return False, []


_pool_of = {}


def pool_of(node_host):
    """那台机器**自己**的数据池名。

    各节点的池名互不相干——目录里从不存带池名的绝对路径，池记录也按 (节点, 池名)
    区分。套件里写死一个 ND_POOL 去 ssh 别的节点，等于假设全集群同名：真机上三台
    分别叫 data / tank / data1，写死的那个在两台上根本不存在，于是断言查了个空目录
    还「通过」（或者体面地 skip 掉）——都比红更糟，因为它们看起来是结论。

    先读产品自己记的那份（/var/lib/ndiskless/data-pool，节点本地、不随目录复制），
    这既精确又顺带钉住了那个机制；读不到再退回「顶层数据集」这个启发式。
    """
    if node_host in _pool_of:
        return _pool_of[node_host]
    got, name = ssh_out(node_host, "cat /var/lib/ndiskless/data-pool 2>/dev/null")
    if not (got and name):
        got, name = ssh_out(
            node_host, "zfs list -H -o name -d 0 2>/dev/null | grep -v '^bpool$' | head -1")
    _pool_of[node_host] = (name.strip() if got and name.strip() else POOL)
    return _pool_of[node_host]


def wait_roster_online(base, want, seconds=300, step=10):
    """等花名册里有 want 台且都在线。

    固定 sleep 是猜：心跳新鲜度有一百多秒的窗口，上一轮刚重启过节点的话，进程
    早就好了、花名册却还标着离线。据此开跑，第一条断言就会说「只有两台在线」，
    而那既不是产品的问题，也不是这一轮该测的东西。
    """
    def ok():
        r = roster(base)
        if len(r) < want:
            return None
        return r if all(n.get("online") for n in r.values()) else None
    return wait(ok, seconds, step)


def wait(cond, seconds, step=3):
    deadline = time.time() + seconds
    while time.time() < deadline:
        v = cond()
        if v:
            return v
        time.sleep(step)
    return None
