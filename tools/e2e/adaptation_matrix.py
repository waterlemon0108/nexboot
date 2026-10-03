#!/usr/bin/env python3
"""超管机离线适配：把驱动集与适配脚本写进超管机的持久克隆。

服务端这一半（编排 + 落盘布局）只有真机能验证——单测能覆盖写了哪些文件，
覆盖不了「克隆真的建出来了、文件真的落在 NTFS 上、终端记录跟着变了」。
Windows 内 adapt.ps1 是否真的装上驱动仍需人工，见 docs/手测/超管机在线适配-手测.md。

需要一个能通过体检的真实 Windows 镜像（ND_WINDOWS_SOURCE），会创建并删除
自己的镜像/分组/终端——不要对生产池运行。
"""
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import (IMPORT_DIR, POOL, SSH_HOST, call, check, configs, mount_probe,  # noqa: E402
                   reductions, section, ssh, summary, task_of)

SOURCE = os.environ.get("ND_WINDOWS_SOURCE", f"{IMPORT_DIR}/isharedisk.gzip")
IMG = "e2e-adapt"
CFG = f"{IMG}_default"
MAC = "AA:BB:CC:22:33:44"
TERMINAL = "terminal-AABBCC223344"
CLONE = f"{POOL}/run/SCLIENT-AABBCC223344"


def cleanup():
    call("DELETE", f"/api/terminals/{TERMINAL}")
    for g in call("GET", "/api/groups")[1]["items"]:
        if g["Name"].startswith("e2e-"):
            call("DELETE", f"/api/groups/{g['ID']}")
    ssh(f"zfs destroy -r {CLONE} 2>/dev/null")
    if call("GET", f"/api/images/{IMG}")[0] == 200:
        for c in configs(IMG):
            call("DELETE", f"/api/configs/{c['ID']}")
            time.sleep(2)
        call("DELETE", f"/api/images/{IMG}")
        time.sleep(3)


cleanup()

section("A 准备超管机")
call("POST", "/api/images/import", {"name": IMG, "source_path": SOURCE, "os_type": "windows"})
task = task_of("task-import_image", timeout=900)
check("A1", "镜像导入完成", task.get("Status") == "success", (task.get("Error") or "")[:160])
time.sleep(10)  # 先等体检的临时克隆释放

# 网段按 ND_SSH_HOST 所在网段推：产品会硬拒不在客户机网卡网段内的分组。
SEG = ".".join(SSH_HOST.split(".")[:3])
st, group = call("POST", "/api/groups", {
    "name": "e2e-适配班", "start_ip": f"{SEG}.200", "client_max": 10,
    "gateway": "", "netmask": "255.255.255.0",
    "system_image_id": IMG, "system_config_id": CFG,
    "system_reduction_id": reductions(CFG)[0]["ID"]})
check("A2", "建分组", st == 201, str(group)[:120])
gid = group["ID"] if st == 201 else ""
st, _ = call("POST", "/api/terminals", {"mac": MAC, "name": "适配机", "group_id": gid})
check("A3", "建终端", st in (200, 201))

# 非超管机没有持久克隆可写。
st, body = call("POST", f"/api/terminals/{TERMINAL}/inject-driver", {"bundle_id": ""})
check("A4", "普通终端不能注入（先要设为超管机）", st in (400, 409), f"HTTP {st} {str(body)[:100]}")

st, _ = call("POST", f"/api/terminals/{TERMINAL}/super", {})
check("A5", "设为超管机", st in (200, 201, 204))

section("B 注入：克隆与落盘布局")
st, result = call("POST", f"/api/terminals/{TERMINAL}/inject-driver", {"bundle_id": ""})
check("B1", "无驱动集也可注入（仅盘符脚本）", st in (200, 201), f"HTTP {st} {str(result)[:160]}")
check("B2", "超管机持久克隆已建出", CLONE in ssh(f"zfs list -H -o name | grep SCLIENT || true"),
      ssh("zfs list -H -o name | grep SCLIENT || true"))

# 像产品一样挂载克隆的 Windows 分区，检查实际写入的内容；这一半没有单元测试覆盖。
probe = mount_probe(CLONE,
                    'find Windows/System32/GroupPolicy -maxdepth 4 2>/dev/null; '
                    'echo "--scripts.ini--"; cat Windows/System32/GroupPolicy/Machine/Scripts/scripts.ini 2>/dev/null; '
                    'ls ndadapt 2>/dev/null | sed "s/^/--ndadapt--/"')
check("B3", "适配脚本落在本地 GPO 启动目录", "Startup/adapt.ps1" in probe, probe.replace("\n", " | ")[:220])
check("B4", "启动脚本由 ndadapt.cmd 注册", "0CmdLine=ndadapt.cmd" in probe, probe.replace("\n", " | ")[:160])
check("B5", "gpsvc 能处理这份本地策略（gpt.ini 已写）", "GroupPolicy/gpt.ini" in probe)

st, terminal = call("GET", f"/api/terminals/{TERMINAL}")
pending = (terminal or {}).get("PendingBundleAt")
check("B6", "终端记下了这次待适配", bool(pending), str(terminal)[:160])

st, view = call("GET", f"/api/terminals/{TERMINAL}/inject-result")
check("B7", "机器还没跑过时结果为未完成",
      st == 200 and view.get("done") is False and view.get("injected_at"),
      f"HTTP {st} {str(view)[:120]}")

section("C 重复注入")
st, body = call("POST", f"/api/terminals/{TERMINAL}/inject-driver", {"bundle_id": ""})
# 拒绝必须告诉操作者下一步怎么做。目前返回 400（其它状态冲突用 409），界面都会显示消息。
check("C1", "已有待适配时拒绝覆盖，并说明怎么继续",
      st in (400, 409) and "覆盖" in str(body), f"HTTP {st} {str(body)[:120]}")
st, body = call("POST", f"/api/terminals/{TERMINAL}/inject-driver", {"bundle_id": "", "overwrite": True})
check("C2", "显式 overwrite 才重置克隆重来", st in (200, 201), f"HTTP {st} {str(body)[:120]}")

section("D 收尾")
st, _ = call("DELETE", f"/api/terminals/{TERMINAL}/super")
check("D1", "取消超管", st in (200, 204))
cleanup()
check("D2", "超管克隆随之回收", "SCLIENT-AABBCC223344" not in ssh("zfs list -H -o name || true"),
      ssh("zfs list -H -o name | grep SCLIENT || true"))

sys.exit(summary())
