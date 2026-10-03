#!/usr/bin/env python3
"""配置与还原点的功能矩阵：正向流程与异常流程各一遍，对着真实 ZFS 跑。

前置：服务已部署且可登录，SSH 能读到池，池里有导入目录。见 README.md。
本脚本会删除并重建名为 probe 的镜像及其派生对象——不要对生产池运行。
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ndapi import (BASE, IMPORT_DIR, POOL, call, cfg_by_name, check, configs,  # noqa: E402
                   reductions, section, ssh, summary, task_of)

IMG = "probe"

# 每次都从新导入的镜像开始，否则上一轮留下的配置和还原点会让「初始状态」断言失效。
section("0 准备干净镜像")
call("DELETE", f"/api/images/{IMG}")
time.sleep(3)
for c in configs(IMG) if call("GET", f"/api/images/{IMG}")[0] == 200 else []:
    call("DELETE", f"/api/configs/{c['ID']}")
    time.sleep(2)
call("DELETE", f"/api/images/{IMG}")
time.sleep(2)
ssh(f"truncate -s 64M {IMPORT_DIR}/probe.raw")
st, _ = call("POST", "/api/images/import",
             {"name": IMG, "source_path": f"{IMPORT_DIR}/probe.raw", "os_type": "linux"})
t = task_of("task-import_image")
check("0", "测试镜像导入完成", t.get("Status") == "success", t.get("Error", "")[:120])
time.sleep(12)  # 等体检释放临时克隆

section("A 导入后的初始状态")
cfgs = configs(IMG)
check("A1", "默认配置存在", len(cfgs) == 1 and cfgs[0]["Name"] == "default", cfgs[0]["ID"] if cfgs else "")
d = cfgs[0]["DefaultReductionID"] if cfgs else None
st, red = call("GET", f"/api/reductions/{d}") if d else (404, "")
check("A2", "默认还原点可解析（非快照名）", d == f"{IMG}_0" and len(reductions(f"{IMG}_default")) == 1, str(d))
# 只看数据池的目录：备份池上留有历史副本（含 @ndbackup-* 标记），不属于这里的拓扑
layout = [l.split()[0] for l in ssh(f"zfs list -H -t all -o name | grep '^{POOL}/nd/{IMG}'").splitlines() if '@rep-' not in l and '@ndbackup-' not in l]
check("A3", "ZFS 拓扑：镜像为根 + 配置克隆",
      layout == [f"{POOL}/nd/{IMG}", f"{POOL}/nd/{IMG}@0", f"{POOL}/nd/{IMG}_default", f"{POOL}/nd/{IMG}_default@0"],
      " ".join(layout))

section("B 新建配置")
call("POST", f"/api/images/{IMG}/configs", {"name": "work"}); time.sleep(3)
w = cfg_by_name(IMG, "work")
check("B1", "新配置有自己的基线还原点", bool(w) and len(reductions(w["ID"])) == 1)
check("B2", "默认还原点指向真实还原点行", bool(w) and w["DefaultReductionID"] == w["ID"] + "_0", w["DefaultReductionID"] if w else "")
call("POST", f"/api/images/{IMG}/configs", {"name": "美术教室"}); time.sleep(3)
cn = cfg_by_name(IMG, "美术教室")
check("B3", "中文名配置可创建且显示名保留", bool(cn), cn["ID"] if cn else "")
check("B4", "中文名配置的数据集 ID 为 ASCII", bool(cn) and all(ord(ch) < 128 for ch in cn["ID"]))
check("B5", "重名 409", call("POST", f"/api/images/{IMG}/configs", {"name": "work"})[0] == 409)
check("B6", "空名 400", call("POST", f"/api/images/{IMG}/configs", {"name": "  "})[0] == 400)
check("B7", "镜像不存在 404", call("POST", "/api/images/ghost/configs", {"name": "x"})[0] == 404)

section("C 派生（fork）配置")
call("POST", f"/api/configs/{IMG}_default/fork", {"name": "forked", "reduction_id": f"{IMG}_0"}); time.sleep(3)
f = cfg_by_name(IMG, "forked")
fr = reductions(f["ID"]) if f else []
check("C1", "fork 的基线还原点归属新配置", len(fr) == 1 and fr[0]["ConfigID"] == f["ID"])
check("C2", "还原点不属于源配置 400",
      call("POST", f"/api/configs/{IMG}_default/fork", {"name": "bad", "reduction_id": w["ID"] + "_0"})[0] == 400)
check("C3", "reduction_id 为空 400",
      call("POST", f"/api/configs/{IMG}_default/fork", {"name": "bad", "reduction_id": ""})[0] == 400)
check("C4", "源配置不存在 404",
      call("POST", "/api/configs/ghost/fork", {"name": "bad", "reduction_id": f"{IMG}_0"})[0] == 404)

section("D 新建还原点")
CFG = w["ID"]
call("POST", f"/api/configs/{CFG}/reductions", {"name": "r1"}); time.sleep(3)
check("D1", "正向创建并归一化为 @r1", any(r["Name"] == "@r1" for r in reductions(CFG)))
check("D2", "重名 409", call("POST", f"/api/configs/{CFG}/reductions", {"name": "r1"})[0] == 409)
check("D3", "大小写重名 409", call("POST", f"/api/configs/{CFG}/reductions", {"name": "R1"})[0] == 409)
check("D4", "空名 400", call("POST", f"/api/configs/{CFG}/reductions", {"name": " "})[0] == 400)
# 操作者输入的名字原样保存显示，只有底层快照名转成 ZFS 接受的形式。
typed = {}
for n in ("2026.07.29", "v1-0", "a:b", "装完office", "2026Q3-驱动更新"):
    typed[n] = call("POST", f"/api/configs/{CFG}/reductions", {"name": n})[0]
    time.sleep(2)
check("D5", "任何名字都能建（含中文与空格）", set(typed.values()) == {202}, str(typed))
rows = {r.get("DisplayName"): r["Name"] for r in reductions(CFG)}
check("D6", "显示名按原样保留，快照名折成 ASCII",
      rows.get("装完office") == "@office" and rows.get("2026Q3-驱动更新") == "@2026q3", str(rows))
check("D6b", "中文重名仍然 409（忽略大小写）",
      call("POST", f"/api/configs/{CFG}/reductions", {"name": "装完Office"})[0] == 409)
check("D6c", "超长名 400", call("POST", f"/api/configs/{CFG}/reductions", {"name": "装" * 61})[0] == 400)
check("D7", "配置不存在 404", call("POST", "/api/configs/ghost/reductions", {"name": "x"})[0] == 404)



section("E 合并还原点")
check("E1", "keep 为空 400", call("POST", f"/api/configs/{CFG}/reductions/merge", {"keep_reduction_id": ""})[0] == 400)
check("E2", "keep 不存在 404", call("POST", f"/api/configs/{CFG}/reductions/merge", {"keep_reduction_id": "ghost"})[0] == 404)
check("E3", "keep 跨配置 400",
      call("POST", f"/api/configs/{CFG}/reductions/merge", {"keep_reduction_id": f"{IMG}_0"})[0] == 400)
before = reductions(CFG)
default_before = cfg_by_name(IMG, "work")["DefaultReductionID"]
victim = next(r for r in before if r["Name"] == "@a:b")
call("POST", f"/api/configs/{CFG}/reductions/merge",
     {"keep_reduction_id": CFG + "_v1-0", "delete_reduction_ids": [victim["ID"]]})
time.sleep(4)
after = reductions(CFG)
check("E4", "部分合并只删指定的", len(after) == len(before) - 1 and all(r["Name"] != "@a:b" for r in after))
check("E5", "部分合并不改动仍然存在的默认还原点",
      cfg_by_name(IMG, "work")["DefaultReductionID"] == default_before, default_before)
snaps = [l for l in ssh(f"zfs list -H -t snapshot -o name | grep '^{POOL}/nd/{CFG}@'").splitlines() if "@ndbackup-" not in l and "@rep-" not in l]
check("E6", "ZFS 快照与库一致", sorted(s.split("@")[1] for s in snaps) == sorted(r["Name"][1:] for r in after))
call("POST", f"/api/configs/{CFG}/reductions/merge", {"keep_reduction_id": CFG + "_0"}); time.sleep(4)
check("E7", "全量合并只剩 keep", [r["Name"] for r in reductions(CFG)] == ["@0"])
st, body = call("POST", f"/api/configs/{IMG}_default/reductions/merge", {"keep_reduction_id": f"{IMG}_0"})
check("E8", "只剩一个还原点时合并是空操作", st == 202)

section("F 删除还原点 / 依赖预检")
st, body = call("DELETE", f"/api/reductions/{IMG}_0")
check("F1", "被 fork 依赖的还原点：409 且中文点名", st == 409 and "派生出的配置" in str(body), str(body))
check("F2", "不存在的还原点 404", call("DELETE", "/api/reductions/ghost")[0] == 404)
for n in ("d1", "d2"):
    call("POST", f"/api/configs/{CFG}/reductions", {"name": n}); time.sleep(2)
reds = reductions(CFG)
call("DELETE", f"/api/reductions/{CFG}_d1"); time.sleep(3)
check("F3", "正向删除", len(reductions(CFG)) == len(reds) - 1)
cur = cfg_by_name(IMG, "work")["DefaultReductionID"]
st, body = call("DELETE", f"/api/reductions/{cur}")
check("F4", "当前应用的还原点：409 且提示先应用别的", st == 409 and "先应用" in str(body)
      and any(r["ID"] == cur for r in reductions(CFG)), f"HTTP {st} {str(body)[:100]}")
for r in reductions(CFG):
    if r["ID"] != cur:
        call("DELETE", f"/api/reductions/{r['ID']}"); time.sleep(2)
st, body = call("DELETE", f"/api/reductions/{cur}")
check("F5", "最后一个还原点：409 且说明是唯一的", st == 409 and "唯一" in str(body)
      and [r["ID"] for r in reductions(CFG)] == [cur], f"HTTP {st} {str(body)[:100]}")

section("G 删除配置 / 镜像")
st, body = call("DELETE", f"/api/configs/{IMG}_default")
check("G1", "被 fork 依赖的配置：409 且中文点名", st == 409 and "派生出的配置" in str(body), str(body))
# 双机时每 15s 一轮整库 send，destroy 可能撞上「正被占用」，隔轮重试即可。
for _ in range(5):
    call("DELETE", f"/api/configs/{CFG}"); time.sleep(6)
    if cfg_by_name(IMG, "work") is None:
        break
check("G2", "正向删除配置", cfg_by_name(IMG, "work") is None)
check("G3", "配置删除后 ZFS 数据集也没了", CFG not in ssh(f"zfs list -H -o name | grep '^{POOL}/nd/' || true"))

# 以下三节都对 probe 的默认配置操作：新建两个还原点 ra、rb，应用 rb。
DEF = f"{IMG}_default"
for n in ("ra", "rb"):
    call("POST", f"/api/configs/{DEF}/reductions", {"name": n}); time.sleep(3)
RA = next((r["ID"] for r in reductions(DEF) if r["Name"] == "@ra"), "")
RB = next((r["ID"] for r in reductions(DEF) if r["Name"] == "@rb"), "")
call("POST", f"/api/reductions/{RB}/apply")


def snaps_of(dataset):
    out = ssh(f"zfs list -H -t snapshot -o name -d 1 {POOL}/nd/{dataset} 2>/dev/null || true")
    return sorted(l.split("@", 1)[1] for l in out.splitlines() if "@" in l and "@rep-" not in l and "@ndbackup-" not in l)


def delete_image(name):
    for c in configs(name) if call("GET", f"/api/images/{name}")[0] == 200 else []:
        call("DELETE", f"/api/configs/{c['ID']}"); time.sleep(2)
    call("DELETE", f"/api/images/{name}"); time.sleep(3)


section("H 导出为镜像文件（按还原点）")
st, ticket = call("POST", f"/api/reductions/{RA}/export-ticket")
check("H1", "拿到还原点的下载票据，文件名带还原点名", st == 200 and "ra" in str(ticket.get("file_name", "")), str(ticket)[:120])
EXP = f"{IMPORT_DIR}/probe-ra.zfs"
ssh(f"rm -f {EXP}; curl -s -o {EXP} '{BASE}{ticket.get('url', '')}'; stat -c %s {EXP}")
size = ssh(f"stat -c %s {EXP} 2>/dev/null || echo 0").strip()
check("H2", "下载到非空的镜像文件", size.isdigit() and int(size) > 0, size)
check("H3", "导出用的临时克隆已清理", "EXPORT-" not in ssh(f"zfs list -H -o name | grep '^{POOL}/run/' || true"))
delete_image("probe-exp")
call("POST", "/api/images/import", {"name": "probe-exp", "source_path": EXP, "os_type": "linux"})
t = task_of("task-import_image")
check("H4", "导出的文件能重新导入", t.get("Status") == "success", t.get("Error", "")[:120])
check("H5", "导入后的镜像只有 @0，没有残留快照", snaps_of("probe-exp") == ["0"], str(snaps_of("probe-exp")))
time.sleep(10)
delete_image("probe-exp")
ssh(f"rm -f {EXP}")

section("I 另存为新镜像（完整复制）")
delete_image("probe-copy")
check("I1", "空名 400", call("POST", f"/api/reductions/{RA}/save-as-image", {"name": " "})[0] == 400)
check("I2", "重名 409", call("POST", f"/api/reductions/{RA}/save-as-image", {"name": IMG})[0] == 409)
check("I3", "还原点不存在 404", call("POST", "/api/reductions/ghost/save-as-image", {"name": "x"})[0] == 404)
st, body = call("POST", f"/api/reductions/{RA}/save-as-image", {"name": "probe-copy"})
t = task_of("task-copy_image")
check("I4", "另存任务成功", st == 202 and t.get("Status") == "success", f"HTTP {st} " + t.get("Error", "")[:120])
origin = ssh(f"zfs get -H -o value origin {POOL}/nd/probe-copy 2>/dev/null").strip()
check("I5", "新镜像是完整副本，不依赖原镜像", origin == "-", origin)
check("I6", "新镜像只有 @0，自带默认配置", snaps_of("probe-copy") == ["0"] and cfg_by_name("probe-copy", "default") is not None,
      str(snaps_of("probe-copy")))
check("I7", "临时克隆已清理", "EXPORT-" not in ssh(f"zfs list -H -o name | grep '^{POOL}/run/' || true"))
time.sleep(10)
delete_image("probe-copy")

section("J 覆盖原镜像（按还原点）")
call("POST", f"/api/reductions/{RA}/apply")
st, body = call("POST", f"/api/configs/{DEF}/merge")
check("J1", "应用的不是最新还原点时合并配置被拒并说明", st == 409 and "覆盖原镜像" in str(body), str(body)[:160])
check("J2", "还原点不存在 404", call("POST", "/api/reductions/ghost/overwrite-image")[0] == 404)
st, body = call("POST", f"/api/reductions/{RA}/overwrite-image")
t = task_of("task-merge_config")
check("J3", "从较早的还原点覆盖原镜像成功", st == 202 and t.get("Status") == "success", f"HTTP {st} " + t.get("Error", "")[:160])
check("J4", "只剩一个配置、一个还原点", len(configs(IMG)) == 1 and [r["Name"] for r in reductions(DEF)] == ["@0"],
      f"{[c['Name'] for c in configs(IMG)]} {[r['Name'] for r in reductions(DEF)]}")
check("J5", "池上镜像和配置都只剩 @0", snaps_of(IMG) == ["0"] and snaps_of(DEF) == ["0"], f"{snaps_of(IMG)} {snaps_of(DEF)}")

sys.exit(summary())
