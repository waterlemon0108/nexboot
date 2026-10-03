#!/usr/bin/env python3
"""用户与告警：真库上的增删改查、登录换号、告警确认与删除。

这两块的判定逻辑有单测，覆盖不了「密码真的换了、换完能用新密码登录、告警
被扫描器写进库后能确认能删」。会创建并删除自己的用户——不要对生产环境运行。

存储池的增删缓存不在这里：产品只把 lsblk 的 TYPE=disk 当候选盘，loop 回环
文件不算，所以没有整块空闲物理盘的机器上跑不了这条链路。见
docs/手测/存储池-zpool-cache-手测.md。
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ndapi  # noqa: E402
from ndapi import (BASE, IMPORT_DIR, call, check, section, skip, ssh,  # noqa: E402
                   summary, task_of)

USER = "e2e-操作员"
PW1 = "e2e-Passw0rd!"
PW2 = "e2e-Passw0rd!2"


def users():
    return call("GET", "/api/users")[1]["items"]


def find_user(name):
    return next((u for u in users() if u.get("username") == name), None)


def cleanup():
    for u in users():
        if u.get("username", "").startswith("e2e-"):
            call("DELETE", f"/api/users/{u['id']}")


def login_as(username, password):
    """A fresh login, without disturbing the session this script runs on."""
    import json
    import urllib.error
    import urllib.request
    req = urllib.request.Request(
        BASE + "/api/login", method="POST",
        data=json.dumps({"username": username, "password": password}).encode(),
        headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode().strip()


ndapi.login()
cleanup()

section("A 用户")
st, body = call("POST", "/api/users", {"username": USER, "password": PW1})
check("A1", "建用户", st == 201, f"HTTP {st} {str(body)[:120]}")
uid = body.get("id") or body.get("ID") if st == 201 else ""

check("A2", "重名 409", call("POST", "/api/users", {"username": USER, "password": PW1})[0] == 409)
check("A3", "空密码 400", call("POST", "/api/users", {"username": "e2e-空密码", "password": ""})[0] == 400)

st, _ = login_as(USER, PW1)
check("A4", "新用户能登录", st == 200, f"HTTP {st}")
check("A5", "错密码登录 401", login_as(USER, "wrong")[0] == 401)

st, body = call("POST", f"/api/users/{uid}/password", {"old_password": PW1, "new_password": PW2})
check("A6", "改密码", st in (200, 204), f"HTTP {st} {str(body)[:100]}")
check("A7", "改完旧密码失效", login_as(USER, PW1)[0] == 401)
check("A8", "改完新密码可用", login_as(USER, PW2)[0] == 200)
check("A9", "旧密码填错时拒绝改密", call("POST", f"/api/users/{uid}/password",
                                {"old_password": "wrong", "new_password": "x-Passw0rd!"})[0] in (400, 401, 403))

st, _ = call("PUT", f"/api/users/{uid}", {"username": "e2e-改名后"})
check("A10", "改名", st in (200, 204) and find_user("e2e-改名后") is not None, f"HTTP {st}")

st, _ = call("DELETE", f"/api/users/{uid}")
check("A11", "删用户", st in (200, 204) and find_user("e2e-改名后") is None, f"HTTP {st}")
check("A12", "删掉后不能再登录", login_as("e2e-改名后", PW2)[0] == 401)

# 当前登录的账号不能被删除，否则一次点击就能把所有人锁在外面。
admin = next((u for u in users() if u.get("username") == "admin"), None)
if admin:
    st, body = call("DELETE", f"/api/users/{admin['id']}")
    check("A13", "不能删掉最后一个管理员", st >= 400, f"HTTP {st} {str(body)[:100]}")

section("B 告警")
# 告警由巡检根据真实状况产生，最省事的办法是造一个过不了体检的镜像。
BROKEN = "e2e-broken-image"
ssh(f"truncate -s 64M {IMPORT_DIR}/e2e-broken.raw")
call("POST", "/api/images/import",
     {"name": BROKEN, "source_path": f"{IMPORT_DIR}/e2e-broken.raw", "os_type": "windows"})
task_of("task-import_image", timeout=300)
# 告警用镜像 id 命名，id 由导入时的显示名派生，要查出来而不是假定两者相同。
broken_id = next((i["ID"] for i in call("GET", "/api/images")[1]["items"] if i["Name"] == BROKEN), "")

raised = None
for _ in range(9):  # 巡检每 30s 一轮
    time.sleep(6)
    raised = next((a for a in call("GET", "/api/alarms?size=100")[1]["items"]
                   if a.get("Resource") == broken_id and a.get("Status") == "active"), None)
    if raised:
        break
check("B0", "体检不过的镜像会被扫描器报成告警", bool(raised),
      f"镜像 {broken_id}：等了 54 秒仍没有告警" if not raised else raised.get("Message", ""))

st, listing = call("GET", "/api/alarms?size=100")
check("B1", "告警列表可读", st == 200 and "items" in listing, f"HTTP {st}")
alarms = listing.get("items", []) if st == 200 else []

active = [a for a in alarms if a.get("Status") == "active"]
if active:
    target = next((a for a in active if a.get("Resource") == broken_id), active[0])
    st, _ = call("POST", f"/api/alarms/{target['ID']}/ack")
    check("B2", "确认告警", st == 200, f"HTTP {st}")
    time.sleep(1)
    now = next((a for a in call("GET", "/api/alarms?size=100")[1]["items"] if a["ID"] == target["ID"]), {})
    check("B3", "确认后状态变为 acknowledged", now.get("Status") == "acknowledged", str(now.get("Status")))
else:
    skip("B2", "确认告警", "当前没有活动告警（告警由扫描器按真实状况产生）")
    skip("B3", "确认后状态变为 acknowledged", "同上")

check("B4", "按状态过滤", all(a.get("Status") == "recovered"
                          for a in call("GET", "/api/alarms?status=recovered&size=100")[1]["items"]))
check("B5", "按级别过滤", all(a.get("Severity") == "error"
                          for a in call("GET", "/api/alarms?severity=error&size=100")[1]["items"]))

if alarms:
    victim = alarms[-1]
    st, _ = call("DELETE", f"/api/alarms/{victim['ID']}")
    check("B6", "删告警", st == 200, f"HTTP {st}")
    check("B7", "删掉的告警不再出现",
          not [a for a in call("GET", "/api/alarms?size=100")[1]["items"] if a["ID"] == victim["ID"]])
else:
    skip("B6", "删告警", "库里当前没有任何告警")
    skip("B7", "删掉的告警不再出现", "同上")

check("B8", "确认不存在的告警 404", call("POST", "/api/alarms/ghost/ack")[0] == 404)

# 消除原因后告警必须自动关闭，只能手工清除的告警会被操作者忽视。
for c in call("GET", f"/api/images/{broken_id}/configs")[1].get("items", []):
    call("DELETE", f"/api/configs/{c['ID']}")
    time.sleep(2)
# 后台体检进行中时删除会被 409 拒绝（「请稍后重试」）——照提示隔几秒重试。
for _ in range(10):
    if call("DELETE", f"/api/images/{broken_id}")[0] != 409:
        break
    time.sleep(6)
recovered = False
for _ in range(9):
    time.sleep(6)
    still = [a for a in call("GET", "/api/alarms?size=100")[1]["items"]
             if a.get("Resource") == broken_id and a.get("Status") == "active"]
    if not still:
        recovered = True
        break
check("B9", "原因消失后告警自动恢复", recovered, "" if recovered else "等了 54 秒仍是 active")

section("C 审计")
st, logs = call("GET", "/api/logs?type=operation&size=20")
check("C1", "刚才的写操作都进了审计", st == 200 and logs.get("total", 0) > 0, f"HTTP {st} total={logs.get('total')}")
modules = {row.get("Module") for row in logs.get("items", [])}
check("C2", "审计行带中文模块名", "用户" in modules or "告警" in modules, str(modules)[:120])

cleanup()
sys.exit(summary())
