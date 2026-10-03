#!/usr/bin/env python3
"""Offline checks for the e2e CLI environment; never contact a cluster."""
import json
import os
from pathlib import Path
import subprocess
import sys

E2E = Path(__file__).resolve().parent
CHILD = """
import io, itertools, json, os, runpy, shlex, subprocess, sys
from pathlib import Path
from unittest.mock import patch
script, addresses = sys.argv[1], json.loads(sys.argv[2])
sys.path.insert(0, str(Path(script).parent))
sys.argv = [script]
local_run = subprocess.run
def blocked(*args, **kwargs):
    raise SystemExit("external access before ND_VIP validation")
def http(*args, **kwargs):
    response = io.BytesIO(b'{"access_token":"offline","token":"offline","items":[],"ok":true,"status":"ok"}')
    response.status = 200
    return response
def remote(args, **kwargs):
    cmd = args[-1]
    if cmd.startswith("ip -4 -o addr show"):
        rows = "\\n".join("2: eth0 inet " + address + "/24" for address in addresses)
        shell = "ip() { printf '%s\\n' " + shlex.quote(rows) + "; }; " + cmd
        return local_run(["/bin/sh", "-c", shell], capture_output=True, text=True)
    if "curl " in cmd:
        out = '{"status":"ok"}'
    elif "cat /var/lib/ndiskless/data-pool" in cmd:
        out = "tank"
    elif "name,creation" in cmd:
        out = "tank/nd@rep-offline\\t1001"
    elif "grep @rep-" in cmd:
        out = "tank/nd@rep-offline"
    else:
        out = ""
    return subprocess.CompletedProcess(args, 0, stdout=out, stderr="")
configured = bool(os.environ.get("ND_VIP"))
with patch("subprocess.run", remote if configured else blocked), \\
     patch("urllib.request.urlopen", http if configured else blocked), \\
     patch("time.sleep", (lambda seconds: None) if configured else blocked), \\
     patch("time.time", side_effect=itertools.count(1000, 300)):
    runpy.run_path(script, run_name="__main__")
"""


def run_cli(script, vip=None, addresses=()):
    env = {k: v for k, v in os.environ.items() if not k.startswith("ND_")}
    env.update(ND_NODES="http://203.0.113.10:8080", ND_PASSWORD="offline-test",
               ND_CLUSTER_TOKEN="offline-test", ND_SSH_USER="root")
    if vip is not None:
        env["ND_VIP"] = vip
    return subprocess.run([sys.executable, "-c", CHILD, str(E2E / script), json.dumps(addresses)],
                          env=env, capture_output=True, text=True, timeout=5)


if __name__ == "__main__":
    for script in ("reset.py", "wait_settled.py"):
        result = run_cli(script)
        assert result.returncode != 0 and "请先设置 ND_VIP" in result.stderr, (
            script, result.returncode, result.stdout, result.stderr)
        for addresses, found in ((["203.0.113.80", "203.0.113.8"], True),
                                 (["203.0.113.80"], False)):
            result = run_cli(script, "203.0.113.8", addresses)
            assert (result.returncode == 0) == found, (
                script, addresses, result.returncode, result.stdout, result.stderr)
    print("e2e CLI environment checks passed (offline)")
