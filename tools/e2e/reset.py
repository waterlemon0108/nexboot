#!/usr/bin/env python3
"""把集群带回一个已知的干净起点；达不到就失败退出，不带着脏环境往下跑。

存在的理由：这套矩阵此前没有「干净起点」这个概念，全靠每个矩阵自己收尾。而破坏
性用例经常跑一半就断（断电、断网、超时），收尾没执行，残留就永久留在那里——下一
轮在一个和它毫无关系的地方红，于是「每轮都有新问题」。真机上已经反复付过代价：
上一轮的 e2e-super 没删掉，下一轮超管保存 409；一个失去快照的还原点被设成当前应
用点，整轮每次 /boot 都 500；孤儿数据集把 47.2G 的池吃到只剩 782M，复制随之停摆。

只清套件自己建的东西。判据是命名：测试资产一律带 e2e 字样，MAC 一律 0e:2e:c1:
前缀，挪开的副本叫 nd-rebuilding-*/nd-diverged-*。**认不出来的一律不碰**——宁可
报告让人来看，也不猜。销毁不可逆，而这套脚本跑在别人的集群上。

用法：
    python3 reset.py            复位
    python3 reset.py --check    只检查不动手
"""
import shlex
import sys
import time

sys.path.insert(0, ".")
from ndcluster import (NODES, VIP, call, entry_base, host, pool_of, ssh, ssh_out)  # noqa: E402

if not VIP:
    raise SystemExit("请先设置 ND_VIP 为测试集群的虚 IP，再运行本脚本。")

CHECK_ONLY = "--check" in sys.argv
VIP_BASE = entry_base()

# 测试资产的判据。改套件时同步改这里，否则复位会把新资产留下来。
TEST_MAC_PREFIX = "0E2EC1"
TEST_NAME_MARK = "e2e"

problems = []
actions = []



def note(msg):
    actions.append(msg)
    print("  ·", msg, flush=True)


def fail(msg):
    problems.append(msg)
    print("  !", msg, flush=True)


def step(title):
    print("\n== %s ==" % title, flush=True)


# ---------------------------------------------------------------- 1 可达与角色
step("1 节点可达、恰好一个写入者")
alive, writer = [], None
for base in NODES:
    h = host(base)
    got, out = ssh_out(h, "curl -s -m 5 http://127.0.0.1:8080/healthz")
    if not got:
        fail("%s 连不上（SSH 失败）" % h)
        continue
    alive.append(h)
    if ssh_out(h, f"ip -4 -o addr show | awk -v vip={shlex.quote(VIP)} "
                  "'{split($4, ip, \"/\"); if (ip[1] == vip) found=1} END {print found+0}'")[1] == "1":
        writer = h
    print("   %-16s %s" % (h, out[:70]), flush=True)
if len(alive) != len(NODES):
    fail("只有 %d/%d 台可达" % (len(alive), len(NODES)))
if writer is None:
    fail("没有任何节点持有虚 IP")
else:
    note("写入者 = %s" % writer)

# ------------------------------------------------------------------ 2 测试资产
step("2 清理测试残留（终端、分组、还原点）")
st, terms = call(VIP_BASE, "GET", "/api/terminals")
if st == 200 and isinstance(terms, dict):
    for t in terms.get("items") or []:
        mac = (t.get("MAC") or "").upper().replace(":", "")
        name = t.get("Name") or ""
        if mac.startswith(TEST_MAC_PREFIX) or TEST_NAME_MARK in name:
            note("删终端 %s（%s）" % (name or t.get("ID"), mac))
            if not CHECK_ONLY:
                call(VIP_BASE, "DELETE", "/api/terminals/%s" % t.get("ID"), timeout=40)
else:
    fail("读不到终端清单（HTTP %s）" % st)

# 删还原点前先把当前应用点挪开：当前点拒绝删除，DELETE 虽返回 202，行却一直在，下一轮建同名会 409。
st, imgs = call(VIP_BASE, "GET", "/api/images")
for img in (imgs.get("items") or []) if st == 200 and isinstance(imgs, dict) else []:
    st2, cfgs = call(VIP_BASE, "GET", "/api/images/%s/configs" % img.get("ID"))
    for cfg in (cfgs.get("items") or []) if st2 == 200 and isinstance(cfgs, dict) else []:
        st3, reds = call(VIP_BASE, "GET", "/api/configs/%s/reductions" % cfg.get("ID"))
        items = (reds.get("items") or []) if st3 == 200 and isinstance(reds, dict) else []
        doomed = [r for r in items
                  if TEST_NAME_MARK in (r.get("DisplayName") or "")
                  or TEST_NAME_MARK in (r.get("Name") or "").lstrip("@")]
        if not doomed:
            continue
        # 只在当前点正是要删的测试点时才挪到最新的非测试点；当前点是现场客户机开机用的，其余情况不碰。
        doomed_ids = {r.get("ID") for r in doomed}
        if cfg.get("DefaultReductionID") in doomed_ids:
            keep = next((r.get("ID") for r in reversed(items) if r.get("ID") not in doomed_ids), "")
            if not keep:
                fail("配置 %s 只剩测试还原点，删光会让它没有可用的应用点——留给人处理"
                     % cfg.get("ID"))
                continue
            note("配置 %s 的当前点是测试还原点，挪到 %s" % (cfg.get("ID"), keep))
            if not CHECK_ONLY:
                call(VIP_BASE, "POST", "/api/reductions/%s/apply" % keep, timeout=40)
        for r in doomed:
            note("删还原点 %s（配置 %s）" % (r.get("DisplayName") or r.get("ID"), cfg.get("ID")))
            if not CHECK_ONLY:
                call(VIP_BASE, "DELETE", "/api/reductions/%s" % r.get("ID"), timeout=40)

# 超管保存中断会在库里留下 <配置>_before_super 的记录，数据集删了记录还在会触发 missing_dataset，
# 备机据此拒绝跟随、复制停摆。所以池和库要一起清。
for img in (imgs.get("items") or []) if st == 200 and isinstance(imgs, dict) else []:
    st2, cfgs = call(VIP_BASE, "GET", "/api/images/%s/configs" % img.get("ID"))
    for cfg in (cfgs.get("items") or []) if st2 == 200 and isinstance(cfgs, dict) else []:
        cid = cfg.get("ID") or ""
        if not cid.endswith("_before_super"):
            continue
        origin = cid[: -len("_before_super")]
        if not any((c.get("ID") == origin) for c in (cfgs.get("items") or [])):
            fail("库里有 %s 而原配置 %s 不在——保存可能停在半路，留给人处理" % (cid, origin))
            continue
        note("删库里的保存暂存配置 %s（原配置仍在）" % cid)
        if not CHECK_ONLY:
            call(VIP_BASE, "DELETE", "/api/configs/%s" % cid, timeout=40)

st, groups = call(VIP_BASE, "GET", "/api/groups")
for g in (groups.get("items") or []) if st == 200 and isinstance(groups, dict) else []:
    if TEST_NAME_MARK in (g.get("Name") or "") and not g.get("IsDefault"):
        note("删分组 %s" % g.get("Name"))
        if not CHECK_ONLY:
            call(VIP_BASE, "DELETE", "/api/groups/%s" % g.get("ID"), timeout=40)

# -------------------------------------------------------------- 3 池上的残留
step("3 清理池上不依赖追平的残留")
# 这两类无需等追平即可判断，且必须在一致性检查之前清：残留 _before_super 会让体检报错，整轮 /boot 失败。
for base in NODES:
    h = host(base)
    p = pool_of(h)
    # <配置>_before_super 是超管保存的暂存名。原配置还在说明交换已完成，可以清；
    # 原配置不在说明保存停在半路，这份可能是唯一副本，绝不能碰。
    got, raw = ssh_out(h, "zfs list -H -o name -r %s/nd 2>/dev/null | grep '_before_super$' || true" % p)
    for ds in (raw.split() if got else []):
        origin = ds[: -len("_before_super")]
        if not ssh_out(h, "zfs list -H -o name %s 2>/dev/null || true" % origin)[1].strip():
            fail("%s 上 %s 的原配置 %s 不在——保存可能停在半路，这份别动"
                 % (h, ds.split("/")[-1], origin.split("/")[-1]))
            continue
        note("%s 删保存暂存 %s（原配置仍在，换已完成）" % (h, ds.split("/")[-1]))
        if not CHECK_ONLY:
            ssh(h, "zfs destroy -r %s" % ds, timeout=300)
    # run/ 下属于测试 MAC 的克隆：终端已经删了，克隆不该留着
    got, raw = ssh_out(h, "zfs list -H -o name -r %s/run 2>/dev/null | grep -E '(S)?CLIENT-%s' || true"
                       % (p, TEST_MAC_PREFIX))
    for ds in (raw.split() if got else []):
        note("%s 删测试克隆 %s" % (h, ds.split("/")[-1]))
        if not CHECK_ONLY:
            ssh(h, "zfs destroy -r %s" % ds, timeout=300)


step("4 目录一致、三台追平")
st, rep = call(VIP_BASE, "GET", "/api/system/consistency", timeout=60)
if st != 200 or not isinstance(rep, dict):
    fail("一致性体检读不到（HTTP %s）" % st)
elif not rep.get("ok"):
    fail("目录与数据库对不上：%s —— 这会让整轮的 /boot 全部失败，先处理它"
         % [i.get("ref") for i in (rep.get("issues") or [])])
else:
    note("一致性 ok")


def latest_marks():
    return {host(b): ssh_out(host(b), "zfs list -H -o name -t snapshot -r %s/nd 2>/dev/null "
                                      "| grep @rep- | tail -1" % pool_of(host(b)))[1].split("@")[-1]
            for b in NODES}


marks = latest_marks()
if len(set(marks.values())) == 1 and all(marks.values()):
    note("三台已追平 %s" % list(marks.values())[0])
else:
    print("   等待追平…", flush=True)
    for _ in range(30):
        time.sleep(20)
        marks = latest_marks()
        if len(set(marks.values())) == 1 and all(marks.values()):
            note("三台追平 %s" % list(marks.values())[0])
            break
    else:
        fail("600 秒内三台没追平：%s" % marks)

step("5 回收挪开的冗余副本")
# 必须在追平之后：判断挪开的副本是否冗余要和 live 目录比内容，追平前 live 不完整，
# 连 @0 都会被算成「独有」。
for base in NODES:
    h = host(base)
    p = pool_of(h)
    live = {x.split("@")[-1] for x in ssh_out(
        h, "zfs list -H -o name -t snapshot -r %s/nd 2>/dev/null | grep -v @rep- || true" % p)[1].split()}
    got, raw = ssh_out(
        h, "zfs list -H -o name,used -d 1 %s 2>/dev/null | grep -E 'nd-(rebuilding|diverged)' || true" % p)
    for line in (raw.splitlines() if got else []):
        name, used = (line.split() + [""])[:2]
        uniq = {x.split("@")[-1] for x in ssh_out(
            h, "zfs list -H -o name -t snapshot -r %s 2>/dev/null | grep -v @rep- || true" % name)[1].split()}
        extra = uniq - live
        # 独有内容也是测试资产时照删；认不出来的停下来报告，可能是失联期间写下的真数据，删错不可逆。
        unknown = {x for x in extra if TEST_NAME_MARK not in x}
        if unknown:
            fail("%s 上 %s 有认不出来的内容 %s —— 不自动删，请人工确认"
                 % (h, name.split("/")[-1], sorted(unknown)))
            continue
        if extra:
            note("%s 的独有内容 %s 也是测试资产，一并回收" % (name.split("/")[-1], sorted(extra)))
        note("%s 删冗余副本 %s（%s）" % (h, name.split("/")[-1], used))
        if not CHECK_ONLY:
            ssh(h, "zfs destroy -r %s" % name, timeout=600)


# ------------------------------------------------------------------- 结论
step("起点")
for base in NODES:
    h = host(base)
    p = pool_of(h)
    print("   %-16s 池=%-6s 可用=%-8s %s" % (
        h, p, ssh_out(h, "zfs list -H -o avail %s" % p)[1],
        ssh_out(h, "curl -s -m 5 http://127.0.0.1:8080/healthz")[1][:50]), flush=True)

print("\n动作 %d 项，问题 %d 项" % (len(actions), len(problems)), flush=True)
if problems:
    print("复位未达成：", flush=True)
    for p_ in problems:
        print("  -", p_, flush=True)
    print("\n带着脏环境跑下去，红出来的多半不是产品的问题。先处理上面这些。", flush=True)
    sys.exit(1)
print("干净起点已达成" if not CHECK_ONLY else "检查通过（未动手）", flush=True)
