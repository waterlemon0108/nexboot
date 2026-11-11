#!/usr/bin/env python3
"""一台备机停服时删镜像：不能留下「池无库有」，其他备机复制不能停。

修之前是先销毁再清各节点的旧副本，清理失败就返回。库行还在、数据集没了，目录判缺数据集并随复制
清单下发，其他备机拒绝复制，全集群停摆，节点恢复后也不自愈。

    A  备机刚停（心跳还新鲜）：拒绝并点名，什么都没动
    B  被拒期间另一台备机照常追平
    C  心跳过期后能删，另一台备机跟着删，复制不停
    D  停掉的备机回来：增量追平，不触发整份重建

破坏性：会停一台备机的服务几分钟，有定时器兜底，最多 15 分钟后自动拉起。只动自己建的空白数据盘。
环境同 ndcluster；ND_RUNNER_IP 是跑脚本的那台，不会停它。
"""
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndcluster import (NODES, VIP_BASE, call, check, host, node_call, pool_of, roster, section,  # noqa: E402
                       ssh_out, summary, wait)

NAME = f"e2e-down-{time.strftime('%H%M%S')}"
RUNNER = os.environ.get("ND_RUNNER_IP", "")
REVIVE = "nd-e2e-revive"
# 复制每分钟一轮，放宽到三轮多。
FOLLOW_WAIT = 240


def status(base):
    st, body = node_call(base, "GET", "/internal/ha/status")
    return body if st == 200 and isinstance(body, dict) else {}


def mark_of(base):
    mark = status(base).get("mark") or ""
    return int(mark.rsplit("-", 1)[-1]) if mark.startswith("rep-") else 0


def image_by_name():
    st, imgs = call(VIP_BASE, "GET", "/api/images")
    return next((i for i in (imgs or {}).get("items", []) if i.get("Name") == NAME), None)


def has_dataset(base, image_id):
    """True / False；读不到时 None，不能把「没读到」当成「没有」。"""
    h = host(base)
    got, out = ssh_out(h, f"zfs list -H -o name {pool_of(h)}/nd/{image_id} 2>/dev/null; true")
    if not got:
        return None
    return bool(out.strip())


def residues(base):
    h = host(base)
    got, out = ssh_out(h, f"zfs list -H -o name -d 1 {pool_of(h)} | grep -E '/nd-(rebuilding|diverged|held)-' ; true")
    return out.strip() if got else f"读不到：{out}"


def nd_creation(base):
    h = host(base)
    got, out = ssh_out(h, f"zfs get -Hp -o value creation {pool_of(h)}/nd")
    return out.strip() if got else ""


def active_alarms_about(*names):
    st, res = call(VIP_BASE, "GET", "/api/alarms?page_size=100")
    items = (res or {}).get("items", []) if isinstance(res, dict) else []
    return [a.get("Message") for a in items
            if (a.get("status") or a.get("Status")) == "active" and any(n in str(a.get("Message", "")) for n in names)]


def sync_of(node_id, base):
    st, res = call(VIP_BASE, "GET", f"/api/volumes?node={node_id}&pool={pool_of(host(base))}")
    return res if st == 200 and isinstance(res, dict) else {}


def stop_service(h):
    # 兜底：这个脚本中途死掉，这台也不会一直停着。
    return ssh_out(h, f"systemctl stop {REVIVE}.timer 2>/dev/null; systemctl reset-failed {REVIVE}.service 2>/dev/null; "
                      f"systemd-run --quiet --unit={REVIVE} --on-active=900 /bin/systemctl start ndiskless; "
                      "systemctl stop ndiskless; systemctl is-active ndiskless; true")


def start_service(h):
    return ssh_out(h, f"systemctl stop {REVIVE}.timer 2>/dev/null; systemctl start ndiskless; sleep 2; systemctl is-active ndiskless; true")


def main():
    states = {b: status(b) for b in NODES}
    writer = next((b for b, s in states.items() if s.get("role") == "active"), "")
    standbys = [b for b, s in states.items() if s.get("role") == "standby"]
    if not writer or len(standbys) < 2:
        check("0", "要有一台主机和两台备机", False, str(states)[:200])
        return summary()
    down = next(b for b in standbys if host(b) != RUNNER)
    keep = next(b for b in standbys if b != down)
    down_id, keep_id = states[down]["node_id"], states[keep]["node_id"]
    print(f"主机 {host(writer)}，要停 {host(down)}，留着 {host(keep)}")

    section("准备：建一个自己的空白数据盘，等它复制到每台")
    st, body = call(VIP_BASE, "POST", "/api/images/blank", {"name": NAME, "size_bytes": 1 << 30, "filesystem": "ntfs"})
    made = wait(lambda: (lambda i: i if i and i.get("State") == "normal" else None)(image_by_name()), 240)
    if not check("P1", "空白数据盘建好了", bool(made), f"{st} {str(body)[:160]}"):
        return summary()
    img = made["ID"]
    for b in standbys:
        check(f"P2-{host(b)}", f"{host(b)} 收到了", bool(wait(lambda n=b: has_dataset(n, img) is True, FOLLOW_WAIT, 10)))
    creation = nd_creation(down)

    try:
        section("A 备机刚停：拒绝并点名，什么都没动")
        got, out = stop_service(host(down))
        if not check("A0", f"{host(down)} 已停服", got and "inactive" in out, out[:160]):
            return summary()
        stopped_at = time.time()
        mark_before = mark_of(writer)
        st, body = call(VIP_BASE, "DELETE", f"/api/images/{img}", timeout=60)
        text = str(body)
        check("A1", "被拒绝，点名停服的节点，说明镜像没有动", st == 409 and host(down) in text and "还没有动" in text, f"{st} {text[:240]}")
        check("A1b", "报错是操作者能执行的话，不是连接错误原文", "dial tcp" not in text and text.find("请先") >= 0, text[:240])
        check("A2", "库行还在", image_by_name() is not None)
        check("A3", "主机的数据还在", has_dataset(writer, img) is True)
        check("A4", "另一台备机的数据还在", has_dataset(keep, img) is True)
        view = sync_of(down_id, down)
        check("A5", "存储池详情说明这台读不到，不显示成空", view.get("reachable") is False and host(down) in str(view.get("error")), str(view)[:200])

        section("B 被拒期间，另一台备机照常追平")
        moved = wait(lambda: mark_of(writer) > mark_before and mark_of(keep) >= mark_of(writer), FOLLOW_WAIT, 10)
        check("B1", "主机打了新标记，另一台追平", bool(moved), f"主机 {mark_of(writer)} 另一台 {mark_of(keep)} 之前 {mark_before}")
        check("B2", "另一台没有复制错误", sync_of(keep_id, keep).get("sync", {}).get("level") != "error", str(sync_of(keep_id, keep).get("sync"))[:200])
        check("B3", "没有报目录缺数据集", not active_alarms_about(img, NAME), str(active_alarms_about(img, NAME))[:200])

        section("C 心跳过期后能删，复制不停")
        offline = wait(lambda: not roster(VIP_BASE).get(down_id, {}).get("online", True), 240, 5)
        check("C0", "花名册标记它离线", bool(offline))
        # 判活窗口是 90 秒，花名册的窗口可能不一样，多留一点。
        time.sleep(max(0, 100 - (time.time() - stopped_at)))
        started = time.time()
        st, body = call(VIP_BASE, "DELETE", f"/api/images/{img}", timeout=90)
        took = time.time() - started
        if not check("C1", "这次能删了", st == 204, f"{st} {str(body)[:240]}"):
            return summary()
        check("C2", "不被停服的节点拖住", took < 30, f"{took:.1f}s")
        check("C3", "库行没了", image_by_name() is None)
        check("C4", "主机的数据没了", has_dataset(writer, img) is False)
        check("C5", "另一台备机跟着删了", bool(wait(lambda: has_dataset(keep, img) is False, FOLLOW_WAIT, 10)))
        check("C6", "另一台追平", bool(wait(lambda: mark_of(keep) >= mark_of(writer), FOLLOW_WAIT, 10)), f"主机 {mark_of(writer)} 另一台 {mark_of(keep)}")
        check("C7", "另一台没有复制错误", sync_of(keep_id, keep).get("sync", {}).get("level") != "error", str(sync_of(keep_id, keep).get("sync"))[:200])
        check("C8", "另一台没有留下残留", not residues(keep), residues(keep)[:200])
        check("C9", "没有报目录缺数据集", not active_alarms_about(img, NAME), str(active_alarms_about(img, NAME))[:200])

        section("D 停掉的备机回来：增量追平")
        got, out = start_service(host(down))
        check("D0", "服务起来了", got and "active" in out, out[:160])
        back = wait(lambda: status(down).get("role") == "standby" and status(down).get("healthy"), 180, 5)
        check("D1", "回到健康的备机", bool(back), str(status(down))[:200])
        check("D2", "追平主机", bool(wait(lambda: mark_of(down) >= mark_of(writer), 300, 10)), f"主机 {mark_of(writer)} 这台 {mark_of(down)}")
        check("D3", "它的这份镜像也删了", has_dataset(down, img) is False)
        check("D4", "没有触发整份重建", nd_creation(down) == creation, f"之前 {creation} 现在 {nd_creation(down)}")
        check("D5", "没有留下残留", not residues(down), residues(down)[:200])
        check("D6", "花名册恢复在线", bool(wait(lambda: roster(VIP_BASE).get(down_id, {}).get("online"), 180, 5)))
    finally:
        start_service(host(down))
        if image_by_name():
            call(VIP_BASE, "DELETE", f"/api/images/{img}", timeout=90)
    return summary()


if __name__ == "__main__":
    sys.exit(main())
