"""Shared client for the end-to-end matrices.

Every check here talks to a real server over HTTP and reads the pool over SSH:
these matrices exist for what unit tests structurally cannot reach — ZFS
refusing an operation, LIO exporting a LUN, a partition node appearing.

Configuration comes from the environment, never from this file:

    ND_BASE       http://<host>:18080     API base URL
    ND_USER       admin                   login user
    ND_PASSWORD                           login password (required)
    ND_SSH_HOST   <host from ND_BASE>     host to read the pool on
    ND_SSH_USER   root
    ND_SSH_PASSWORD                       falls back to ND_PASSWORD
    ND_POOL       tank                    pool name
    ND_IMPORT_DIR /tank/imports           where source images live
"""
import json
import os
import shlex
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("ND_BASE", "http://127.0.0.1:18080").rstrip("/")
USER = os.environ.get("ND_USER", "admin")
PASSWORD = os.environ.get("ND_PASSWORD", "")
SSH_HOST = os.environ.get("ND_SSH_HOST") or urllib.parse.urlparse(BASE).hostname
SSH_USER = os.environ.get("ND_SSH_USER", "root")
SSH_PASSWORD = os.environ.get("ND_SSH_PASSWORD") or PASSWORD
POOL = os.environ.get("ND_POOL", "tank")
IMPORT_DIR = os.environ.get("ND_IMPORT_DIR", "/tank/imports")

results = []
skipped = []
_token = None


def login():
    global _token
    if not PASSWORD:
        raise SystemExit("ND_PASSWORD is required")
    req = urllib.request.Request(BASE + "/api/login", method="POST",
                                 data=json.dumps({"username": USER, "password": PASSWORD}).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        _token = json.load(r)["token"]
    return _token


def call(method, path, body=None):
    """One API call; returns (status, parsed body or raw text)."""
    if _token is None:
        login()
    data = json.dumps(body).encode() if body is not None else None
    # id 可能是中文（如镜像「教学一班」），urllib 不接受请求行里的非 ASCII 字节，需百分号编码。
    req = urllib.request.Request(BASE + urllib.parse.quote(path, safe="/?&=%"), data=data, method=method)
    req.add_header("Authorization", "Bearer " + _token)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip().startswith(("{", "[")) else raw.strip())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode().strip()


def get_bytes(path):
    """A raw GET for endpoints that answer with a file (a bundle archive, an
    xlsx export). call() decodes as text and would choke on the first zip byte.
    """
    if _token is None:
        login()
    req = urllib.request.Request(BASE + urllib.parse.quote(path, safe="/?&=%"))
    req.add_header("Authorization", "Bearer " + _token)
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def ssh(cmd):
    """Read the server's own state — the pool, configfs, the journal.

    非 root 登录时套一层 sudo -S，读同一个密码。交付现场未必开放 root 直登，
    而这里跑的不只是读：清理要 `zfs destroy`、验证要 `mount`，普通用户做不到。
    少这一层的后果是**静默的**——destroy 失败没人看见，备份池里留着上一轮的
    数据，于是「首轮应当是全量」变成了增量，报出来像是备份逻辑错了。
    """
    if SSH_USER != "root":
        cmd = f"echo {shlex.quote(SSH_PASSWORD)} | sudo -S -p '' bash -c {shlex.quote(cmd)}"
    return subprocess.run(
        ["sshpass", "-e", "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "PubkeyAuthentication=no", f"{SSH_USER}@{SSH_HOST}", cmd],
        capture_output=True, text=True,
        env={"SSHPASS": SSH_PASSWORD, "PATH": os.environ.get("PATH", "")},
    ).stdout.strip()


def configs(image):
    return call("GET", f"/api/images/{image}/configs")[1]["items"]


def reductions(config):
    return call("GET", f"/api/configs/{config}/reductions")[1]["items"]


def cfg_by_name(image, name):
    return next((c for c in configs(image) if c["Name"] == name), None)


def task_of(prefix, timeout=180):
    """Wait for the newest task of this kind to settle.

    It scans a window rather than reading the newest row: an import is followed
    immediately by its health check, so `limit=1` would report on the wrong
    task and call a successful import a failure.
    """
    for _ in range(timeout):
        _, d = call("GET", "/api/tasks/history?limit=10")
        items = d if isinstance(d, list) else d.get("items", [])
        for item in items:
            if item["ID"].startswith(prefix):
                if item["Status"] in ("success", "failed"):
                    return item
                break
        time.sleep(1)
    return {}


def mount_probe(clone, script, attempts=6, delay=3):
    """Mount a clone's Windows partition and run a probe script inside it.

    Retried: a just-created clone's partition nodes lag the clone itself by a
    moment (the same udev lag the import bake has to wait out), and a probe that
    ran too early would report an empty disk as a missing file.
    """
    # 分区号从 1 试到 4：真 Windows 系统分区在 part3/4，fixtures/mkwin.sh 的镜像只有 part1，
    # 挂不上 ntfs3 的 EFI/MSR 自然跳过。
    for _ in range(attempts):
        out = ssh(
            'D=$(mktemp -d); for p in 1 2 3 4; do '
            f'mount -t ntfs3 -o ro,force /dev/zvol/{clone}-part$p $D 2>/dev/null || continue; '
            f'( cd $D && {script} ); umount $D; done; rmdir $D')
        if out.strip():
            return out
        time.sleep(delay)
    return ""


def check(cid, label, ok, detail=""):
    results.append((cid, label, ok, detail))
    print(f"  {'PASS' if ok else 'FAIL'} {cid} {label}" + (f"  — {detail}" if detail else ""))
    return ok


def skip(cid, label, why):
    """Not run, and not counted as passing. A skipped check that prints PASS is
    the matrix lying about its own coverage."""
    skipped.append((cid, label, why))
    print(f"  SKIP {cid} {label}  — {why}")


def section(title):
    print(f"\n### {title}")


def summary():
    """Print the tally and return an exit code.

    A run that checked nothing is not a pass. `0/0 通过` with exit 0 reads as
    "everything is fine" to whoever is looking at the pipeline, when what
    actually happened is that the matrix never got far enough to test anything
    — a missing image, an unreachable node, a bad precondition. Being loud here
    is the whole point of the skip/summary split.
    """
    passed = sum(1 for r in results if r[2])
    line = f"\n{passed}/{len(results)} 通过"
    if skipped:
        line += f"，{len(skipped)} 跳过"
    print(line)
    for cid, label, ok, detail in results:
        if not ok:
            print("  失败:", cid, label, detail)
    for cid, label, why in skipped:
        print("  跳过:", cid, label, why)
    if not results:
        print("  没有执行任何检查——这不算通过，先看上面的跳过原因")
        return 1
    return 0 if passed == len(results) else 1
