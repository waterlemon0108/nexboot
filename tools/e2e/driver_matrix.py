#!/usr/bin/env python3
"""驱动中心：上传驱动包 → 组合驱动集 → 注入超管机 → 下载归档。

单测覆盖了 .inf 解析与各条拒绝规则，覆盖不了「zip 真的传上去了、包真的落在
磁盘上、归档真的能下下来、驱动集真的进了超管机克隆」。这条链路此前既无单测
也无 E2E，只有手测文档。

会创建并删除自己的驱动包/驱动集/镜像/分组/终端——不要对生产池运行。
"""
import io
import json
import os
import sys
import time
import urllib.request
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ndapi  # noqa: E402
from ndapi import (BASE, IMPORT_DIR, call, check, configs, get_bytes,  # noqa: E402
                   mount_probe, reductions, section, ssh, summary, task_of)

SOURCE = os.environ.get("ND_WINDOWS_SOURCE", f"{IMPORT_DIR}/isharedisk.gzip")
IMG = "e2e-driver"
CFG = f"{IMG}_default"
MAC = "AA:BB:CC:33:44:55"
TERMINAL = "terminal-AABBCC334455"

# 解析器能接受的最小驱动：一个 .inf，写一个 PCI id。
SAMPLE_INF = """; e2e sample
[Version]
Signature   = "$WINDOWS NT$"
Class       = Net
Provider    = %V_E2E%
DriverVer   = 01/01/2026,1.0.0.1

[Manufacturer]
%V_E2E% = E2E, NTamd64.10.0

[E2E.NTamd64.10.0]
%E2E_DESC% = E2E.ndi, PCI\\VEN_8086&DEV_{dev}

[Strings]
V_E2E = "E2E"
E2E_DESC = "E2E Test NIC"
"""


def pack_zip(dev="15B7"):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("e2enic.inf", SAMPLE_INF.format(dev=dev))
        z.writestr("e2enic.sys", b"\x00" * 64)
    return buf.getvalue()


def upload_pack(name, data, category="boot_critical_nic", os_type="windows", arch="x64"):
    """Multipart upload, hand-built: the endpoint takes a form, not JSON."""
    boundary = "----e2e-driver-boundary"
    parts = []
    for field, value in (("name", name), ("category", category), ("os_type", os_type), ("arch", arch)):
        parts.append(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{field}\"\r\n\r\n{value}\r\n".encode())
    parts.append(
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}.zip\"\r\n"
        f"Content-Type: application/zip\r\n\r\n".encode() + data + b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    req = urllib.request.Request(BASE + "/api/driver-packs", data=b"".join(parts), method="POST")
    req.add_header("Authorization", "Bearer " + (ndapi._token or ndapi.login()))
    req.add_header("Content-Type", f"multipart/form-data; boundary={boundary}")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode().strip()


def cleanup():
    call("DELETE", f"/api/terminals/{TERMINAL}")
    for g in call("GET", "/api/groups")[1]["items"]:
        if g["Name"].startswith("e2e-"):
            call("DELETE", f"/api/groups/{g['ID']}")
    for b in call("GET", "/api/driver-bundles")[1]["items"]:
        if b["bundle"]["Name"].startswith("e2e-"):
            call("DELETE", f"/api/driver-bundles/{b['bundle']['ID']}")
    for p in call("GET", "/api/driver-packs")[1]["items"]:
        if p["pack"]["Name"].startswith("e2e-"):
            call("DELETE", f"/api/driver-packs/{p['pack']['ID']}")
    ssh("zfs destroy -r $(zfs list -H -o name | grep SCLIENT-AABBCC334455) 2>/dev/null")
    if call("GET", f"/api/images/{IMG}")[0] == 200:
        for c in configs(IMG):
            call("DELETE", f"/api/configs/{c['ID']}")
            time.sleep(2)
        call("DELETE", f"/api/images/{IMG}")
        time.sleep(3)


ndapi.login()
cleanup()

section("A 上传驱动包")
st, body = upload_pack("e2e-nic-a", pack_zip("15B7"))
check("A1", "上传含 .inf 的 zip", st in (200, 201), f"HTTP {st} {str(body)[:140]}")
pack_a = body["pack"]["ID"] if st in (200, 201) else ""
hwids = body.get("pack", {}).get("HWIDs") if st in (200, 201) else None
check("A2", "从 .inf 解析出硬件 ID", bool(hwids) and "VEN_8086&DEV_15B7" in str(hwids), str(hwids))
check("A2b", "同时解析出版本与厂商",
      body.get("pack", {}).get("Version") == "1.0.0.1" and body.get("pack", {}).get("Vendor") == "E2E",
      str(body.get("pack", {}))[:120])

st, body = upload_pack("e2e-nic-b", pack_zip("15B8"))
pack_b = body["pack"]["ID"] if st in (200, 201) else ""
check("A3", "第二个驱动包", bool(pack_b), f"HTTP {st}")

# 不是驱动包的 zip 必须在上传时拒绝，而不是等到客户机开机注入时才失败。
empty = io.BytesIO()
with zipfile.ZipFile(empty, "w") as z:
    z.writestr("readme.txt", "no drivers here")
st, body = upload_pack("e2e-not-a-driver", empty.getvalue())
check("A4", "没有 .inf 的 zip 当场拒绝", st == 400 and "inf" in str(body), f"HTTP {st} {str(body)[:100]}")

section("B 组合驱动集")
st, body = call("POST", "/api/driver-bundles", {"name": "e2e-集合", "pack_ids": [pack_a, pack_b]})
bundle = body["bundle"]["ID"] if st == 201 else ""
check("B1", "用两个包组一个驱动集", st == 201, f"HTTP {st} {str(body)[:140]}")
check("B2", "重名驱动集 409", call("POST", "/api/driver-bundles", {"name": "e2e-集合", "pack_ids": [pack_a]})[0] == 409)
check("B3", "空包列表 400", call("POST", "/api/driver-bundles", {"name": "e2e-空", "pack_ids": []})[0] == 400)

# 已禁用的驱动包不能进入打包，由服务端把关而不是靠页面置灰。
call("POST", f"/api/driver-packs/{pack_b}/status", {"status": "disabled"})
st, body = call("POST", "/api/driver-bundles", {"name": "e2e-含禁用", "pack_ids": [pack_a, pack_b]})
check("B4", "含已禁用驱动包的驱动集被拒", st == 400 and "禁用" in str(body), f"HTTP {st} {str(body)[:100]}")
call("POST", f"/api/driver-packs/{pack_b}/status", {"status": "enabled"})

check("B5", "被驱动集引用的包不能删", call("DELETE", f"/api/driver-packs/{pack_a}")[0] == 409)

section("C 归档下载")
st, raw, headers = get_bytes(f"/api/driver-bundles/{bundle}/archive")
check("C1", "驱动集可下载为 zip", st == 200 and raw[:2] == b"PK", f"HTTP {st} {raw[:40]!r}")
check("C2", "带上浏览器保存所需的响应头",
      headers.get("Content-Type") == "application/zip" and ".zip" in headers.get("Content-Disposition", ""),
      f"{headers.get('Content-Type')} / {headers.get('Content-Disposition')}")
# 超管机解开的就是这个压缩包，两个驱动包都必须在里面。
with zipfile.ZipFile(io.BytesIO(raw)) as z:
    names = z.namelist()
check("C3", "归档内含两个驱动包的 inf", sum(1 for n in names if n.lower().endswith(".inf")) >= 2, " ".join(names)[:160])

section("D 注入超管机")
call("POST", "/api/images/import", {"name": IMG, "source_path": SOURCE, "os_type": "windows"})
task = task_of("task-import_image", timeout=900)
check("D1", "镜像导入完成", task.get("Status") == "success", (task.get("Error") or "")[:120])
time.sleep(10)

# 网段按 ND_SSH_HOST 所在网段推：产品会硬拒不在客户机网卡网段内的分组。
SEG = ".".join(ndapi.SSH_HOST.split(".")[:3])
st, group = call("POST", "/api/groups", {
    "name": "e2e-驱动班", "start_ip": f"{SEG}.210", "client_max": 10,
    "gateway": "", "netmask": "255.255.255.0",
    "system_image_id": IMG, "system_config_id": CFG,
    "system_reduction_id": reductions(CFG)[0]["ID"]})
gid = group["ID"] if st == 201 else ""
call("POST", "/api/terminals", {"mac": MAC, "name": "驱动机", "group_id": gid})
call("POST", f"/api/terminals/{TERMINAL}/super", {})
st, body = call("POST", f"/api/terminals/{TERMINAL}/inject-driver", {"bundle_id": bundle})
check("D2", "把驱动集注入超管机", st in (200, 201), f"HTTP {st} {str(body)[:140]}")
check("D3", "回执点名注入的是哪个驱动集", "e2e-集合" in str(body), str(body)[:140])

clone = ssh("zfs list -H -o name | grep SCLIENT-AABBCC334455 || true").strip()
probe = mount_probe(clone, 'ls -l ndadapt 2>/dev/null') if clone else ""
check("D4", "驱动集 zip 落在克隆的 ndadapt 目录", "bundle.zip" in probe, probe.replace("\n", " | ")[:200])

section("E 收尾")
cleanup()
check("E1", "驱动包与驱动集都已删除",
      not [p for p in call("GET", "/api/driver-packs")[1]["items"] if p["pack"]["Name"].startswith("e2e-")])

sys.exit(summary())
