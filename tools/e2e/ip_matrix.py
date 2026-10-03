#!/usr/bin/env python3
"""IP 分配与 DHCP：分组区间、终端地址、跨组搬迁，每一步都让真 dnsmasq 判卷。

单测覆盖了分配算法本身，但覆盖不了这一条：产品拼出来的那行 dhcp-range 到底
能不能被 dnsmasq 加载。单测读的是我们自己生成的字符串，它和 dnsmasq 的语法
之间没有任何联系——曾经就把 `static` 加在 end-addr 之后（那里 static 是
*替代* end-addr 的），单测全绿，部署上去服务起不来，因为初始同步失败即退出。
所以这份矩阵每改一次分组/终端，都把落盘的配置交给 `dnsmasq --test`。

会创建并删除自己的分组和终端，前缀 e2e-。不要对生产环境运行。
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ndapi  # noqa: E402
from ndapi import call, check, section, ssh, summary  # noqa: E402

CONF = "/etc/dnsmasq.d/ndiskless.conf"


def groups():
    return call("GET", "/api/groups")[1]["items"]


def terminals():
    return call("GET", "/api/terminals")[1]["items"]


def cleanup():
    for t in terminals():
        if t.get("MAC", "").startswith("E2E") or t.get("Name", "").startswith("e2e-"):
            call("DELETE", f"/api/terminals/{t['ID']}")
    for g in groups():
        if g.get("Name", "").startswith("e2e-"):
            call("DELETE", f"/api/groups/{g['ID']}")


def dnsmasq_ok():
    """dnsmasq 对落盘配置的判决——这份矩阵存在的理由。"""
    out = ssh(f"dnsmasq --test -C {CONF} 2>&1")
    return "syntax check OK" in out, out.replace("\n", " ")[:200]


def dnsmasq_loaded_current_config():
    """运行中的 dnsmasq 是否读过磁盘上这份配置。

    只验文件是不够的：systemctl reload 发的是 SIGHUP，而 dnsmasq 明确不在
    SIGHUP 时重读配置文件。产品曾经就这样「同步成功」了十一天，运行中的进程
    一个新分组都不知道，而每一层看起来都正常。判据是进程的启动时刻不早于配置
    的写入时刻。
    """
    # 产品刚 restart 时服务可能处于 activating，systemctl 的时间戳为空，要等它落定。
    started = ""
    for _ in range(20):
        started = ssh("systemctl show dnsmasq -p ActiveEnterTimestampMonotonic --value")
        if started and started != "0":
            break
        time.sleep(0.5)
    written = ssh(f"stat -c %Y {CONF}")
    now_mono = ssh("awk '{printf \"%d\", $1*1000000}' /proc/uptime")
    now_wall = ssh("date +%s")
    try:
        # 都换算成「距今多少秒」，跨时钟源才可比。
        started_ago = (int(now_mono) - int(started)) / 1e6
        written_ago = int(now_wall) - int(written)
    except (ValueError, TypeError):
        return False, f"读不到时间戳：started={started!r} written={written!r}"
    ok = started_ago <= written_ago + 2  # 进程比配置新（或同时），留 2 秒余量
    return ok, f"dnsmasq 启动于 {started_ago:.0f} 秒前，配置写于 {written_ago} 秒前"


def base_group(name, start, gateway, netmask="255.255.255.0", client_max=10):
    src = groups()[0]  # 复用已有分组的镜像/配置/还原点三元组
    return {
        "name": name, "start_ip": start, "client_max": client_max,
        "gateway": gateway, "netmask": netmask, "dns1": "223.5.5.5",
        "system_image_id": src["SystemImageID"],
        "system_config_id": src["SystemConfigID"],
        "system_reduction_id": src["SystemReductionID"],
    }


ndapi.login()
cleanup()

section("0 起点")
ok, detail = dnsmasq_ok()
check("0a", "改动前配置本身是好的", ok, detail)
existing = groups()
check("0b", "有可复用的分组三元组", bool(existing) and bool(existing[0].get("SystemReductionID")),
      str([g["Name"] for g in existing]))
if not ok or not existing:
    sys.exit(summary())

# 本矩阵的分组分布在 66~71 多个网段，只有跨网段（DHCP 中继）模式下合法；跑完恢复原值。
NET = call("GET", "/api/network")[1]
WAS_CROSS = bool(NET.get("allow_cross_subnet"))
if not WAS_CROSS:
    st, body = call("PUT", "/api/network", {"client_iface": NET.get("client_iface_setting", ""), "allow_cross_subnet": True})
    check("0c", "打开跨网段分组", st in (200, 204), f"HTTP {st} {str(body)[:120]}")

section("A 建组的边界")
# 每一条建成功的都要让 dnsmasq 复核；被拒的只看状态码。
st, body = call("POST", "/api/groups", base_group("e2e-一班", "192.168.66.10", "192.168.66.1"))
check("A1", "建正常分组", st in (200, 201), f"HTTP {st} {str(body)[:120]}")
g1 = body.get("ID") if st in (200, 201) else ""
ok, detail = dnsmasq_ok()
check("A2", "建组后 dnsmasq 认这份配置", ok, detail)
ok, detail = dnsmasq_loaded_current_config()
check("A2b", "建组后运行中的 dnsmasq 真的重新加载了", ok, detail)

for cid, label, req, want in [
    ("A3", "网关落在客户机区间内被拒",
     base_group("e2e-网关撞车", "192.168.67.10", "192.168.67.15"), 400),
    ("A4", "区间盖住广播地址被拒",
     base_group("e2e-广播", "192.168.68.250", "192.168.68.1", client_max=6), 400),
    ("A5", "区间盖住网络地址被拒",
     base_group("e2e-网络地址", "192.168.69.0", "192.168.69.254", client_max=10), 400),
    ("A6", "区间跨出子网被拒",
     base_group("e2e-跨段", "192.168.70.250", "192.168.70.1", client_max=20), 400),
    ("A7", "与已有分组网段重叠被拒",
     base_group("e2e-重叠", "192.168.66.15", "192.168.66.1", client_max=5), 409),
]:
    st, body = call("POST", "/api/groups", req)
    check(cid, label, st == want, f"HTTP {st} {str(body)[:120]}")

# 一班的网关 192.168.66.1 落在新分组的区间里，某台机器拿到 .1 时一班就会失去出口。
st, body = call("POST", "/api/groups",
                base_group("e2e-抢网关", "192.168.66.1", "192.168.66.254", client_max=8))
check("A8", "区间盖住其它分组的网关被拒", st == 409, f"HTTP {st} {str(body)[:120]}")

# 区间不能包含服务器自己的地址：dnsmasq 会拒发（"in use by the server or relay"），轮到它的终端会起不来。
own = ssh("ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1").split()
if own:
    # 只圈本机地址附近一小段：整段 1-254 会先撞上「与其它分组重叠」（409），测不到这条要验的拒绝。
    a, b, c, d = own[0].split(".")
    st, body = call("POST", "/api/groups",
                    base_group("e2e-盖住本机", f"{a}.{b}.{c}.{d}", f"{a}.{b}.{c}.254", client_max=2))
    check("A9", "区间盖住服务器自身地址被拒", st == 400, f"本机 {own[0]}：HTTP {st} {str(body)[:100]}")
else:
    check("A9", "区间盖住服务器自身地址被拒", False, "读不到本机地址")

# 被拒的建组不能在配置里留下痕迹。
ok, detail = dnsmasq_ok()
check("A10", "一串被拒建组后配置仍然可加载", ok, detail)
check("A11", "被拒的分组没有落进配置", "192.168.67." not in ssh(f"cat {CONF}"),
      ssh(f"grep -c dhcp-range {CONF}"))

section("B 终端地址的端点")
st, body = call("POST", "/api/terminals",
                {"mac": "E2:E0:00:00:00:01", "ip": "192.168.66.10", "group_id": g1})
check("B1", "首址可用", st in (200, 201), f"HTTP {st} {str(body)[:120]}")
t1 = body.get("ID") if st in (200, 201) else ""
st, body = call("POST", "/api/terminals",
                {"mac": "E2:E0:00:00:00:02", "ip": "192.168.66.19", "group_id": g1})
check("B2", "末址可用", st in (200, 201), f"HTTP {st} {str(body)[:120]}")
t2 = body.get("ID") if st in (200, 201) else ""
check("B3", "首址前一位被拒",
      call("POST", "/api/terminals",
           {"mac": "E2:E0:00:00:00:03", "ip": "192.168.66.9", "group_id": g1})[0] == 400)
check("B4", "末址后一位被拒",
      call("POST", "/api/terminals",
           {"mac": "E2:E0:00:00:00:04", "ip": "192.168.66.20", "group_id": g1})[0] == 400)
check("B5", "重复 IP 被拒",
      call("POST", "/api/terminals",
           {"mac": "E2:E0:00:00:00:05", "ip": "192.168.66.10", "group_id": g1})[0] == 409)
check("B6", "同一 MAC 换写法仍算重复",
      call("POST", "/api/terminals",
           {"mac": "e2-e0-00-00-00-01", "ip": "192.168.66.12", "group_id": g1})[0] == 409)

st, body = call("POST", "/api/terminals", {"mac": "E2:E0:00:00:00:06", "group_id": g1})
auto = body.get("IP") if st in (200, 201) else ""
check("B7", "自动分配取最低空位", auto == "192.168.66.11", f"HTTP {st} 得到 {auto}")

conf = ssh(f"cat {CONF}")
check("B8", "端点上的终端都渲染成了保留项",
      "192.168.66.10" in conf and "192.168.66.19" in conf, "")
ok, detail = dnsmasq_ok()
check("B9", "登记终端后 dnsmasq 认这份配置", ok, detail)
ok, detail = dnsmasq_loaded_current_config()
check("B10", "登记终端后运行中的 dnsmasq 真的重新加载了", ok, detail)

section("C 只服务已登记的 MAC")
# 区间与保留项重叠是设计使然，未登记机器必须拿不到地址，否则它租走的地址会在 12 小时内被分给已登记终端。
check("C1", "配置里有 dhcp-ignore=tag:!known", "dhcp-ignore=tag:!known" in conf,
      "\n".join(l for l in conf.split("\n") if "ignore" in l))
check("C2", "分组区间是完整的 start,end 写法",
      f"dhcp-range=set:" in conf and "192.168.66.10,192.168.66.19" in conf,
      "\n".join(l for l in conf.split("\n") if l.startswith("dhcp-range")))

section("D 跨组搬迁")
st, body = call("POST", "/api/groups", base_group("e2e-二班", "192.168.71.10", "192.168.71.1"))
check("D1", "建第二个分组", st in (200, 201), f"HTTP {st} {str(body)[:120]}")
g2 = body.get("ID") if st in (200, 201) else ""
st, body = call("POST", "/api/terminals/move", {"terminal_ids": [t1, t2], "group_id": g2})
check("D2", "跨网段批量搬迁被接受", st == 200, f"HTTP {st} {str(body)[:160]}")
moved = [t for t in terminals() if t["ID"] in (t1, t2)]
check("D3", "搬过去的终端拿到了目标网段的地址",
      all(t["IP"].startswith("192.168.71.") for t in moved),
      str([(t["MAC"], t["IP"], t["GroupID"]) for t in moved]))
check("D4", "两台没拿到同一个地址", len({t["IP"] for t in moved}) == len(moved),
      str([t["IP"] for t in moved]))
ok, detail = dnsmasq_ok()
check("D5", "搬迁后 dnsmasq 认这份配置", ok, detail)
ok, detail = dnsmasq_loaded_current_config()
check("D6", "搬迁后运行中的 dnsmasq 真的重新加载了", ok, detail)

section("E 改分组区间")
# 区间缩到装不下已有终端必须被拒，否则下次渲染配置报错，全机房 DHCP 停摆。
shrink = base_group("e2e-二班", "192.168.71.10", "192.168.71.1", client_max=1)
st, body = call("PUT", f"/api/groups/{g2}", shrink)
check("E1", "缩到装不下已有终端被拒", st >= 400, f"HTTP {st} {str(body)[:120]}")
grow = base_group("e2e-二班", "192.168.71.10", "192.168.71.1", client_max=20)
st, _ = call("PUT", f"/api/groups/{g2}", grow)
check("E2", "扩大区间被接受", st in (200, 204), f"HTTP {st}")
ok, detail = dnsmasq_ok()
check("E3", "改区间后 dnsmasq 认这份配置", ok, detail)

section("F 清理")
cleanup()
if not WAS_CROSS:
    call("PUT", "/api/network", {"client_iface": NET.get("client_iface_setting", ""), "allow_cross_subnet": False})
ok, detail = dnsmasq_ok()
check("F1", "删干净后配置仍然可加载", ok, detail)
check("F2", "e2e 的分组和终端都不在了",
      not [g for g in groups() if g["Name"].startswith("e2e-")]
      and not [t for t in terminals() if t.get("MAC", "").startswith("E2E0")], "")

sys.exit(summary())
