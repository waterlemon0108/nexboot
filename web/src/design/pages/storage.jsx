// 存储池管理页。后端不提供的指标（吞吐、L2ARC 命中率等）直接不显示，不编造。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, TableState, StatCard, SegFilter } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation } from '../../lib/hooks.js';
import { fmtCap } from '../../lib/format.js';
const { useState, useEffect } = React;

const ROLE_LABEL = { data: "存储盘", read_cache: "读缓存", write_cache: "写缓存", special: "元数据盘", spare: "热备盘" };
// ZFS 后台正在对池做的操作。
const OP_LABEL = { resilver: "重建中", remove: "迁移中" };

// 池的用途：数据池存放全部镜像、配置和客户机克隆；备份池是本地备份的目标；
// 两者都不是的池等待在备份设置里指定。
const POOL_ROLE = {
  data: { cls: "cyan", text: "数据池", hint: "镜像、配置和客户机克隆都存放在这个池" },
  backup: { cls: "violet", text: "备份池", hint: "本地备份的目标池（在备份设置中选择）" },
};
const roleRank = (p) => (p.Role === "data" ? 0 : p.Role === "backup" ? 1 : 2);

// 布局建池后无法更改（除非重建），所以每个选项在选之前就说明代价。
const LAYOUTS = {
  mirror: { label: "镜像（RAID1）", desc: "每 2（或 3）块盘互为完整副本，再在组间条带。读性能与条带相当、写减半；每组可坏 1 块。推荐用于数据池。" },
  stripe: { label: "条带", desc: "容量最大、读写最快，但没有冗余：任一块盘损坏，池里全部数据丢失。只适合测试环境。" },
  raidz1: { label: "raidz1", desc: "一块校验盘，可坏 1 块。随机 IO 只有一块盘的水平，不能移盘；重建慢。" },
  raidz2: { label: "raidz2", desc: "两块校验盘，可坏 2 块。容量优先、随机 IO 弱、不能移盘，适合备份池。" },
  raidz3: { label: "raidz3", desc: "三块校验盘，可坏 3 块。同 raidz2，冗余更高、容量更少。" },
};
const LAYOUT_ORDER = ["mirror", "stripe", "raidz1", "raidz2", "raidz3"];
const RAIDZ_PARITY = { raidz1: 1, raidz2: 2, raidz3: 3 };

// 提交前按 zpool 的命名规则检查：字母开头，之后只能是字母、数字、_ - . :；
// 不能是 vdev 关键字（如 mirrors 会被 ZFS 以 name is reserved 拒绝），也不能是 c<数字> 形式的设备名。
function poolNameProblem(name) {
  if (!/^[A-Za-z]/.test(name)) return "池名要以字母开头，例如 tank、data-pool";
  if (!/^[A-Za-z0-9_.:-]+$/.test(name)) return "池名只能包含字母、数字和 _ - . :";
  const lower = name.toLowerCase();
  if (/^(mirror|raidz|draid|spare)/.test(lower) || lower === "log") return `${name} 是 ZFS 保留名（mirror / raidz / draid / spare 开头及 log 都不能作池名），请换一个`;
  if (/^c[0-9]/.test(lower)) return `${name} 不能作池名：ZFS 不允许 c 加数字开头的名字`;
  return "";
}

// layoutLabel 给出表格和详情里的布局说法：「镜像（RAID1）×3」「raidz2」「条带」。读到池后组数取自池本身。
// 本产品的「镜像」指系统镜像，磁盘镜像都带上 RAID1，免得混淆。
function layoutLabel(pool) {
  const layout = pool.Layout || "stripe";
  if (layout === "mirror") {
    const groups = (pool.Groups || []).filter(g => g.Role === "data" && g.Kind === "mirror").length;
    return groups ? `镜像（RAID1）×${groups}` : `镜像（RAID1）${pool.GroupWidth || 2} 路`;
  }
  return (LAYOUTS[layout] || {}).label || layout;
}

// poolPlan 按后端的规则校验所选磁盘与布局，写明还差几块，并预估容量
//（镜像：按选择顺序每组最小盘之和；raidz：数据盘数 × 最小盘 × 0.9，扣除填充与预留）。
// 只是预估，建成后以池上报的 used + available 为准。
function poolPlan(layout, width, disks) {
  const n = disks.length;
  if (!n) return { ok: false, message: "" };
  const sizes = disks.map(d => Number(d.Size || 0));
  const min = (arr) => arr.reduce((m, v) => (m == null || v < m ? v : m), null) || 0;
  if (layout === "stripe") {
    return { ok: true, message: `条带 ${n} 块 ≈ ${fmtCap(sizes.reduce((a, b) => a + b, 0))} 可用 · 无冗余` };
  }
  if (layout === "mirror") {
    if (n < width || n % width !== 0) {
      return { ok: false, message: `镜像池按 ${width} 块一组，请再选 ${width - (n % width)} 块（已选 ${n} 块）` };
    }
    let total = 0;
    for (let i = 0; i < n; i += width) total += min(sizes.slice(i, i + width));
    return { ok: true, message: `镜像 ${n / width} 组 ≈ ${fmtCap(total)} 可用 · 每组可坏 ${width - 1} 块` };
  }
  const parity = RAIDZ_PARITY[layout] || 0;
  const need = parity + 2;
  if (n < need) return { ok: false, message: `${layout} 至少 ${need} 块盘，请再选 ${need - n} 块（已选 ${n} 块）` };
  return { ok: true, message: `${layout} ≈ ${fmtCap((n - parity) * min(sizes) * 0.9)} 可用 · 可坏 ${parity} 块` };
}

function healthMeta(h) {
  const up = String(h || "").toUpperCase();
  if (up === "ONLINE") return { cls: "emerald", dot: "live", text: "在线" };
  if (up === "DEGRADED") return { cls: "amber", dot: "warn", text: "降级" };
  if (up === "AVAIL") return { cls: "emerald", dot: "live", text: "待命" }; // 热备盘，待命
  if (up === "INUSE") return { cls: "amber", dot: "warn", text: "顶替中" }; // 热备盘，正在顶替故障盘
  if (up === "" || up === "PENDING") return { cls: "", dot: "cyan", text: "处理中" };
  if (up === "MISSING") return { cls: "rose", dot: "err", text: "未找到", title: "本机上已找不到该池" };
  // 其余都是故障；ZFS 原词留在悬停里，便于搜索。
  if (up === "FAULTED") return { cls: "rose", dot: "err", text: "故障", title: h };
  if (up === "OFFLINE") return { cls: "rose", dot: "err", text: "已离线", title: h };
  if (up === "UNAVAIL") return { cls: "rose", dot: "err", text: "不可用", title: h };
  if (up === "REMOVED") return { cls: "rose", dot: "err", text: "已拔出", title: h };
  if (up === "SUSPENDED") return { cls: "rose", dot: "err", text: "已暂停", title: h };
  return { cls: "rose", dot: "err", text: h };
}

// 节点回答「没有这个池」：只剩这条记录。
const isMissing = (p) => String(p?.Health || "").toUpperCase() === "MISSING";

// 按角色取池的磁盘，优先用带状态的 DiskItems，没有状态时退回扁平路径数组。
function disksByRole(pool, role, fallbackKey) {
  const items = (pool.DiskItems || []).filter(d => d.Role === role);
  if (items.length) return items;
  return (pool[fallbackKey] || []).map(path => ({ Path: path, Role: role, Status: "" }));
}

// 池的写操作必须在盘所在的节点上执行；本机的池仍走本地调用，别的节点的池按名字路由过去。
// 返回的对象与 api 同形，调用点不用改写法。
function poolWriter(node) {
  if (!node) return api;
  const at = (suffix) => (id, body) => api.clusterPoolAction(node, id, suffix, body);
  return {
    ...api,
    deletePool: (id) => api.clusterDestroyPool(node, id),
    addPoolDisk: at("/disks"),
    removePoolDisk: at("/disks/remove"),
    replacePoolDisk: at("/disks/replace"),
    mirrorUpgradePool: at("/mirror-upgrade"),
    addSpecial: at("/special"),
    removeSpecial: at("/special/remove"),
    addSpare: at("/spares"),
    removeSpare: at("/spares/remove"),
    addReadCache: at("/read-cache"),
    removeReadCache: at("/read-cache/remove"),
    addWriteCache: at("/write-cache"),
    removeWriteCache: at("/write-cache/remove"),
    flushWriteCache: (id) => api.clusterPoolAction(node, id, "/write-cache/flush"),
  };
}

function PageStorage() {
  const store = useStore();
  const conf = useConfirm();
  const [selectedId, setSelectedId] = useState(null);
  const [filterHealth, setFilterHealth] = useState("all");
  const [filterRole, setFilterRole] = useState("all");
  const [modal, setModal] = useState(null);
  const [form, setForm] = useState({ name: "", disks: [], layout: "mirror", group_width: 2 });

  // 各请求独立结算：磁盘探测失败（如 lsblk 不可用）不能让池列表变空，反之亦然。
  const { data, loading, error, reload: load } = useResource(async () => {
    const [pr, dr, nr, cr] = await Promise.allSettled([
      api.listPools(), api.listDisks(), api.listClusterNodes(), api.listClusterPools(),
    ]);
    if (pr.status === "rejected") throw pr.reason;
    return {
      localPools: (pr.value && pr.value.items) || [],
      disks: dr.status === "fulfilled" ? (dr.value && dr.value.items) || [] : [],
      nodes: nr.status === "fulfilled" ? (nr.value && nr.value.items) || [] : [],
      cluster: cr.status === "fulfilled" ? cr.value : null,
    };
  }, []);
  const localPools = (data && data.localPools) || [];
  const disks = (data && data.disks) || [];
  const nodes = (data && data.nodes) || [];
  const cluster = (data && data.cluster) || null;
  const clustered = nodes.length > 1;
  // 池记录会复制，本地列表包含各节点的池，但只有本机的有容量数字；
  // 其余节点的容量和健康只能取自按节点汇总的结果。
  const nodeOf = (id) => nodes.find(n => n.id === id) || null;
  const selfNodeID = ((cluster && cluster.nodes) || []).find(n => n.is_self)?.node_id || "";
  const nodeLabel = (id) => (nodeOf(id)?.ip) || id || "—";
  const pools = clustered && cluster
    ? (cluster.nodes || []).flatMap(n => (n.pools || []).map(p => ({ ...p, ServerID: p.ServerID || n.node_id, __node: n })))
    : localPools;
  // 没有应答的节点要单独点出，不能直接略去：少一行读起来像「那台没有存储」。
  const unreachableNodes = clustered && cluster ? (cluster.nodes || []).filter(n => !n.reachable) : [];
  // 本机的池返回 null，走本地调用。
  const remoteNodeOf = (p) => (clustered && p && p.__node && !p.__node.is_self ? p.__node.node_id : null);
  const { busy, run } = useMutation(store.toast);

  const selected = pools.find(p => p.ID === selectedId) || null;

  const filtered = pools.filter(p => {
    if (filterRole !== "all" && (p.Role || "") !== (filterRole === "none" ? "" : filterRole)) return false;
    if (filterHealth === "all") return true;
    const up = String(p.Health || "").toUpperCase();
    if (filterHealth === "online") return up === "ONLINE";
    if (filterHealth === "abnormal") return up !== "ONLINE" && up !== "PENDING" && up !== "";
    return true;
  }).sort((a, b) => roleRank(a) - roleRank(b)); // 数据池在前；同类保持接口给的节点顺序

  const totalCap = clustered && cluster ? Number(cluster.total_capacity || 0) : pools.reduce((s, p) => s + Number(p.Capacity || 0), 0);
  const totalUsed = clustered && cluster ? Number(cluster.total_used || 0) : pools.reduce((s, p) => s + Number(p.Used || 0), 0);
  const onlineN = pools.filter(p => String(p.Health).toUpperCase() === "ONLINE").length;
  // 每台最多两个池（数据 + 备份）。只有所有已列出节点都满了才锁按钮；
  // 部分节点满的情况交给后端拒绝。
  const poolsByServer = pools.reduce((m, p) => { const k = p.ServerID || ""; m[k] = (m[k] || 0) + 1; return m; }, {});
  const atPoolLimit = pools.length > 0 && Object.values(poolsByServer).every(n => n >= 2);
  const present = pools.filter(p => !isMissing(p));
  const dataDiskN = present.reduce((s, p) => s + ((p.Disks || []).length), 0);
  const rcN = present.reduce((s, p) => s + ((p.ReadCacheDisks || []).length), 0);
  const wcN = present.reduce((s, p) => s + ((p.WriteCacheDisks || []).length), 0);
  const usePct = totalCap ? Math.round(totalUsed / totalCap * 100) : 0;

  const openCreate = () => { setForm({ name: "", disks: [], layout: "mirror", group_width: 2, node: "", role: "" }); setModal("create"); };

  // 每台一个数据池、一个备份池：按所选节点已有的，算出还能建哪种
  const createNodeID = form.node || selfNodeID;
  const nodePools = clustered ? pools.filter(p => (p.ServerID || "") === createNodeID) : pools;
  const hasData = nodePools.find(p => p.Role === "data");
  const hasBackup = nodePools.find(p => p.Role !== "data");
  const roleChoices = [
    !hasData && { value: "data", label: POOL_ROLE.data.text },
    !hasBackup && { value: "backup", label: POOL_ROLE.backup.text },
  ].filter(Boolean);
  const createRole = roleChoices.length === 1 ? roleChoices[0].value
    : roleChoices.some(o => o.value === form.role) ? form.role : "";
  const roleHint = hasData && hasBackup ? `这台已有数据池 ${hasData.Name} 和备份池 ${hasBackup.Name}，不能再创建`
    : hasData ? `这台已有数据池 ${hasData.Name}，只能建备份池`
    : hasBackup ? `这台已有备份池 ${hasBackup.Name}，只能建数据池`
    : "数据池存放镜像、配置和客户机克隆；备份池是本地备份的目标";

  const pickedDisks = form.disks.map(path => disks.find(d => d.Path === path) || { Path: path, Size: 0 });
  const groupWidth = form.layout === "mirror" ? (form.group_width || 2) : 1;
  const plan = poolPlan(form.layout, groupWidth, pickedDisks);

  const doCreate = async () => {
    const name = form.name.trim();
    if (!createRole) { store.toast("请选择存储池类型", "err"); return; }
    if (!name) { store.toast("请输入池名称", "err"); return; }
    if (/\s/.test(name)) { store.toast("池名不能包含空格", "err"); return; }
    const nameProblem = poolNameProblem(name);
    if (nameProblem) { store.toast(nameProblem, "err"); return; }
    if (!form.disks.length) { store.toast("请至少选择一块磁盘", "err"); return; }
    if (!plan.ok) { store.toast(plan.message, "err"); return; }
    const body = { name, disks: form.disks, layout: form.layout, group_width: groupWidth, role: createRole };
    const target = clustered && form.node && form.node !== selfNodeID ? form.node : "";
    // 最后确认一次：条带没有冗余；建在别的机器上不容易察觉。
    const stripe = form.layout === "stripe";
    if (stripe || target) {
      const where = target ? `将在 ${nodeLabel(target)} 上创建存储池 ${name}，使用磁盘：${form.disks.join("、")}。` : "";
      const risk = stripe ? `条带布局无冗余：任一磁盘故障，${name} 中的全部镜像、配置、还原点及客户机克隆将丢失且无法恢复。确认以条带布局创建？` : "";
      const sure = await conf.ask({
        title: stripe ? "条带布局没有冗余" : "在其他服务器上创建",
        message: [where, risk].filter(Boolean).join("\n"),
        danger: stripe, confirmText: stripe ? "仍要创建" : "创建",
      });
      if (!sure) return;
    }
    const ok = await run(
      () => (target ? api.clusterCreatePool(target, body) : api.createPool(body)),
      (target ? `存储池 ${name} 已提交到 ${nodeLabel(target)} 执行，结果见任务列表` : `存储池 ${name} 创建任务已提交（后台执行）`)
        + (createRole === "data" ? "；建成后将自动重启服务以启用该数据池" : ""),
      { reload: load, errMsg: "创建失败" });
    if (ok) setModal(null);
  };

  const doDestroy = async (pool) => {
    const node = remoteNodeOf(pool);
    const where = node ? nodeLabel(node) : "";
    // 集群里池名不唯一（几台都叫 tank），确认时输入的是目标节点地址而不是池名。
    const ok = isMissing(pool) ? await conf.ask({
      title: "删除存储池记录",
      message: `${where || "本机"}上已找不到 ${pool.Name}，将删除这条记录，不会改动任何磁盘。删除后可在该服务器上重新创建同名的池。`,
      confirmText: "删除",
    }) : await conf.ask({
      title: "销毁存储池",
      message: node
        ? `销毁 ${where} 上的 ${pool.Name}，将删除该池及其全部数据（镜像、配置、还原点、客户机克隆），且无法恢复。\n这台不是你正在使用的服务器，请核对地址后再继续。`
        : `销毁 ${pool.Name} 将删除该池及其全部数据（镜像、配置、还原点、客户机克隆），且无法恢复。`,
      danger: true, confirmText: "销毁", typeToConfirm: node ? where : pool.Name,
    });
    if (!ok) return;
    if (await run(() => poolWriter(node).deletePool(pool.ID), `存储池 ${pool.Name} 销毁任务已提交`, { reload: load, errMsg: "销毁失败" })) {
      setSelectedId(null);
    }
  };

  // 建池要列目标节点自己的盘：本机的 /dev/sdb 在那台上可能是系统盘、不存在或已属于别的池。
  const createNode = (form && form.node) || "";
  const { data: nodeDiskData, loading: nodeDisksLoading } = useResource(
    async () => (clustered && createNode ? api.listClusterDisks(createNode) : null),
    [clustered, createNode],
  );
  const createDisks = clustered && createNode
    ? ((nodeDiskData && nodeDiskData.items) || [])
    : disks;
  // 读不到的节点的池容量无从得知，总量不完整；这条注解跟在总量数字后面，不单开横幅。
  const missing = unreachableNodes.length;
  const partial = missing ? ` · 不含读不到的 ${missing} 台` : "";
  // 别的节点的池，加盘、换盘、加缓存都发往那台，选盘也要列那台自己的盘。
  const detailNode = remoteNodeOf(selected);
  const { data: detailDiskData, reload: reloadDetailDisks } = useResource(
    async () => (detailNode ? api.listClusterDisks(detailNode) : null),
    [detailNode],
  );
  const detailDisks = detailNode ? ((detailDiskData && detailDiskData.items) || []) : disks;
  const freeDisks = detailDisks.filter(d => !d.InUse);
  const onDetailChanged = () => { if (detailNode) reloadDetailDisks(); return load(); };
  const createFreeDisks = createDisks.filter(d => !d.InUse);

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      {/* 汇总统计，全部由池数据算出 */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))", gap: 12 }}>
        <StatCard label="存储池" value={pools.length} unit="个" accent="cyan"
          sub={clustered
            ? `${onlineN} 在线 · ${nodes.length} 台服务器${missing ? ` · ${missing} 台读不到` : ""}`
            : `${onlineN} 在线`}/>
        <StatCard label="总容量" value={fmtCap(totalCap).split(" ")[0]} unit={fmtCap(totalCap).split(" ")[1] || ""} accent="violet"
          sub={`${dataDiskN} 块存储盘${partial}`}/>
        <StatCard label="已使用" value={fmtCap(totalUsed).split(" ")[0]} unit={fmtCap(totalUsed).split(" ")[1] || ""} accent="amber"
          sub={`可用 ${fmtCap(totalCap - totalUsed)} · ${usePct}% 使用率${partial}`} pct={usePct}/>
        <StatCard label="缓存盘" value={rcN + wcN} unit="块" accent="emerald" sub={`读 ${rcN} · 写 ${wcN}`}/>
      </div>

      {/* 筛选栏 */}
      <div className="card" style={{ padding: "10px 14px", display: "flex", alignItems: "center", gap: 12 }}>
        <span className="lbl">状态</span>
        <Select value={filterHealth} onChange={setFilterHealth} aria-label="按状态筛选" options={[
          { value: "all", label: "全部" },
          { value: "online", label: "在线" },
          { value: "abnormal", label: "异常" },
        ]} style={{ width: 120 }}/>
        <span className="lbl">类型</span>
        <Select value={filterRole} onChange={setFilterRole} aria-label="按类型筛选" options={[
          { value: "all", label: "全部" },
          { value: "data", label: "数据池" },
          { value: "backup", label: "备份池" },
          { value: "none", label: "未指定" },
        ]} style={{ width: 120 }}/>
        <div style={{ flex: 1 }}/>
        <button className="btn ghost icon" onClick={() => load()} title="立即刷新" aria-label="立即刷新"><Icons.Refresh size={13}/></button>
        {atPoolLimit && <span className="hint">每个节点最多两个存储池（数据池 + 备份池）；扩容请给数据池加盘</span>}
        <button className="btn primary" onClick={openCreate} disabled={atPoolLimit}
          title={atPoolLimit ? "每个节点最多两个存储池（一个数据池、一个备份池）" : undefined}>
          <Icons.Plus size={12}/> 创建存储池
        </button>
      </div>

      {/* 池列表 */}
      <div className="card" style={{ padding: 0 }}>
        <table className="t">
          <thead><tr>
            <th>池名</th><th>服务器</th><th>布局</th><th>容量</th><th>存储盘</th>
            <th>读 / 写缓存</th><th>状态</th><th className="t-actions"></th>
          </tr></thead>
          <tbody>
            {filtered.map((p) => {
              const cap = Number(p.Capacity || 0), used = Number(p.Used || 0);
              const pct = cap ? Math.round(used / cap * 100) : 0;
              const hm = healthMeta(p.Health);
              return (
                <tr key={p.ID} style={{ cursor: "pointer" }} onClick={() => setSelectedId(p.ID)}>
                  <td><div className="row"><Icons.Database size={14} style={{ color: "var(--cyan)" }}/><span className="mono" style={{ fontWeight: 600 }}>{p.Name}</span>{(() => {
                    const role = POOL_ROLE[p.Role];
                    return role
                      ? <span className={`chip ${role.cls}`} title={role.hint}>{role.text}</span>
                      : <span className="chip" title="尚未指定用途；可在备份设置中选作备份池">未指定</span>;
                  })()}</div></td>
                  {/* 节点 ID 是 machine-id 的哈希，运维认不出；显示 IP。 */}
                  <td className="muted mono">{clustered ? nodeLabel(p.ServerID) : (p.ServerID || "—")}</td>
                  <td><span className={`chip ${(p.Layout || "stripe") === "stripe" ? "" : "cyan"}`}>{layoutLabel(p)}</span></td>
                  <td style={{ minWidth: 250 }}>
                    {isMissing(p) ? <span className="muted">本机上已找不到该池</span> : <div className="row" style={{ gap: 8 }}>
                      {/* 限制最大宽度：自动表格布局会把窄列让出的空间都给这一列，不限宽时条会拉到约 450px。 */}
                      <div className={`bar ${pct > 85 ? "rose" : pct > 70 ? "amber" : ""}`} style={{ flex: 1, minWidth: 60, maxWidth: 180 }}><span style={{ width: `${pct}%` }}/></div>
                      {/* 按内容定宽且不换行，条占剩余空间。可用量单独写出：ZFS 按它判定 `out of space`。 */}
                      <span className="mono" style={{ fontSize: 12, whiteSpace: "nowrap", flexShrink: 0 }}>已用 {fmtCap(used)} · 可用 {fmtCap(cap - used)}</span>
                    </div>}
                  </td>
                  <td className="mono">{isMissing(p) ? "—" : (p.Disks || []).length}</td>
                  <td className="mono" style={{ fontSize: 12 }}>
                    <span style={{ color: "var(--emerald)" }}>{(p.ReadCacheDisks || []).length}</span> / <span style={{ color: "var(--violet)" }}>{(p.WriteCacheDisks || []).length}</span>
                  </td>
                  <td>
                    <span className={`chip ${hm.cls}`}>{hm.dot && <span className={`dot ${hm.dot}`}/>}{hm.text}</span>
                    {p.Operation && <span className="mono meta" style={{ marginLeft: 6 }}>{OP_LABEL[p.Operation] || p.Operation} {p.Progress}</span>}
                  </td>
                  <td className="t-actions"><div><button className="btn ghost icon" title="查看池详情" aria-label={`查看 ${p.Name} 详情`}><Icons.Chevron size={12}/></button></div></td>
                </tr>
              );
            })}
            <TableState colSpan={8} loading={loading} error={error} empty={!filtered.length} hint={pools.length ? "没有匹配的存储池" : "暂无存储池 · 点右上角创建"}/>
          </tbody>
        </table>
      </div>

      {selected && <PoolDetail pool={selected} freeDisks={freeDisks} onClose={() => setSelectedId(null)} onChanged={onDetailChanged}
        onDestroy={() => doDestroy(selected)} remoteNode={detailNode} remoteLabel={nodeLabel(detailNode)}
        serverLabel={clustered ? nodeLabel(selected.ServerID) : (selected.ServerID || "—")}/>}

      <Modal open={modal === "create"} onClose={() => !busy && setModal(null)} title="创建存储池" size="md"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button>
          <button className="btn primary" disabled={busy || !createRole || (form.disks.length > 0 && !plan.ok)} onClick={doCreate}>{busy ? "创建中…" : "创建"}</button></>}>
        {clustered && (
          <Field label="建在哪台" required hint="池建在哪台服务器上，就用那台自己的盘；选错机器，选出来的盘不是你以为的那块">
            <Select value={form.node || selfNodeID} aria-label="建在哪台"
              onChange={v => setForm({ ...form, node: v, disks: [] })}
              options={nodes.map(n => ({
                value: n.id,
                label: `${n.ip}${n.ha_state === "active" ? "（主机）" : ""}${n.online ? "" : " · 离线"}`,
              }))} style={{ width: "100%" }}/>
          </Field>
        )}
        <Field label="存储池类型" required hint={roleHint}>
          <Select value={createRole} aria-label="存储池类型" disabled={!roleChoices.length}
            onChange={v => setForm({ ...form, role: v })}
            options={!roleChoices.length ? [{ value: "", label: "不能再创建" }]
              : roleChoices.length > 1 ? [{ value: "", label: "请选择" }, ...roleChoices] : roleChoices}/>
        </Field>
        <Field label="池名称" required hint="ZFS 池名，字母/数字/下划线，不含空格">
          <input className="input mono" style={{ width: "100%" }} value={form.name} onChange={e => setForm({ ...form, name: e.target.value })} placeholder="tank2"/>
        </Field>
        <Field label="布局" required hint={LAYOUTS[form.layout].desc}>
          <div className="row" style={{ gap: 10, flexWrap: "wrap" }}>
            <SegFilter value={form.layout} onChange={v => setForm({ ...form, layout: v })}
              options={LAYOUT_ORDER.map(id => ({ id, label: LAYOUTS[id].label }))}/>
            {form.layout === "mirror" && (
              <div className="row" style={{ gap: 6 }}>
                <span className="lbl">每组</span>
                <Select value={String(form.group_width || 2)} onChange={v => setForm({ ...form, group_width: Number(v) })}
                  options={[{ value: "2", label: "2 路" }, { value: "3", label: "3 路" }]} style={{ width: 80 }}/>
              </div>
            )}
          </div>
        </Field>
        <Field label={`选择磁盘 · 已选 ${form.disks.length}`} required
          hint={plan.message ? <span className={plan.ok ? "mono" : ""} style={{ color: plan.ok ? "var(--fg-mute)" : "var(--amber)" }}>{plan.message}</span> : "镜像按选择顺序两两成组；raidz 全部盘为一组"}>
          <DiskPicker disks={createDisks} selected={form.disks} onToggle={(path, on) => {
            const set = new Set(form.disks);
            on ? set.add(path) : set.delete(path);
            setForm({ ...form, disks: [...set] });
          }}/>
        </Field>
        <div style={{ background: "var(--amber-soft)", border: "1px solid oklch(0.55 0.10 80 / 0.3)", borderRadius: 6, padding: "10px 12px", fontSize: 12, color: "var(--amber)", display: "flex", alignItems: "center", gap: 8 }}>
          <Icons.Shield size={14} style={{ flexShrink: 0 }}/> 选中的磁盘将被 ZFS 接管并格式化，原有数据将丢失
        </div>
      </Modal>
      {conf.node}
    </div>
  );
}

// DiskPicker 列出物理磁盘，已占用的禁选。
function DiskPicker({ disks, selected, onToggle }) {
  if (!disks.length) {
    return <div style={{ background: "var(--bg-0)", border: "1px solid var(--line)", borderRadius: 6, padding: 14, fontSize: 12, color: "var(--fg-faint)", textAlign: "center" }}>未探测到可用磁盘</div>;
  }
  return (
    <div style={{ background: "var(--bg-0)", borderRadius: 6, border: "1px solid var(--line)", padding: 10, display: "flex", flexDirection: "column", gap: 6, maxHeight: 220, overflowY: "auto" }}>
      {disks.map(d => {
        const checked = selected.includes(d.Path);
        const disabled = d.InUse;
        return (
          <label key={d.Path} style={{ display: "flex", alignItems: "center", gap: 10, padding: "5px 6px", borderRadius: 4, cursor: disabled ? "not-allowed" : "pointer", opacity: disabled ? 0.45 : 1 }}>
            <input type="checkbox" disabled={disabled} checked={!!checked} onChange={e => onToggle(d.Path, e.target.checked)}/>
            <span className="mono" style={{ fontSize: 12, fontWeight: 600 }}>{d.Path}</span>
            {d.Type && <span className="chip">{d.Type}</span>}
            <span className="mono muted meta">{fmtCap(d.Size)}</span>
            {d.Model && <span className="muted meta ellip" style={{ maxWidth: 140 }}>{d.Model}</span>}
            <span style={{ marginLeft: "auto", fontSize: 12, color: disabled ? "var(--amber)" : "var(--emerald)" }}>{disabled ? `占用${d.UsedBy ? " · " + d.UsedBy : ""}` : "可用"}</span>
          </label>
        );
      })}
    </div>
  );
}

// DiskPickModal 为加盘、加缓存挑空闲盘。validate(picked) 返回 { ok, message }，
// 所选不符合池布局时（镜像须整组、raidz 须整组、镜像附加须恰好一块）禁用确认按钮。
function DiskPickModal({ open, title, freeDisks, busy, onClose, onConfirm, validate, hint }) {
  const [picked, setPicked] = useState([]);
  useEffect(() => { if (open) setPicked([]); }, [open]);
  const check = picked.length && validate ? validate(picked) : { ok: picked.length > 0, message: "" };
  return (
    <Modal open={open} onClose={() => !busy && onClose()} title={title} size="md"
      footer={<><button className="btn" disabled={busy} onClick={onClose}>取消</button>
        <button className="btn primary" disabled={busy || !picked.length || !check.ok} onClick={() => onConfirm(picked)}>{busy ? "提交中…" : "确认"}</button></>}>
      {hint && <div className="hint" style={{ marginBottom: 8 }}>{hint}</div>}
      <DiskPicker disks={freeDisks} selected={picked} onToggle={(path, on) => {
        setPicked(prev => on ? [...new Set([...prev, path])] : prev.filter(p => p !== path));
      }}/>
      {check.message && <div className={`hint${check.ok ? " mono" : ""}`} style={{ marginTop: 6, color: check.ok ? "var(--fg-mute)" : "var(--amber)" }}>{check.message}</div>}
    </Modal>
  );
}

// 详情用居中弹窗，与产品里其它详情视图一致。
function PoolDetail({ pool, freeDisks, onClose, onChanged, onDestroy, remoteNode = null, remoteLabel = "", serverLabel = "" }) {
  // 这一屏的写操作都发往池所在的节点。
  const api = poolWriter(remoteNode);
  const store = useStore();
  const conf = useConfirm();
  const [picker, setPicker] = useState(null); // "data" | "read" | "write" | "special" | "spare" | { attach: "/dev/sdX" } | { replace: "/dev/sdX" } | { expand: "raidz2-0" }
  const [upgrade, setUpgrade] = useState(null); // 升级 RAID1 向导打开期间为 { [dataDiskPath]: freeDiskPath }
  const { busy, run: runMut } = useMutation(store.toast);
  const run = (fn, okMsg) => runMut(fn, okMsg, { reload: onChanged });

  const cap = Number(pool.Capacity || 0), used = Number(pool.Used || 0);
  const usePct = cap ? Math.round(used / cap * 100) : 0;
  const hm = healthMeta(pool.Health);
  const missing = isMissing(pool);
  const dataDisks = disksByRole(pool, "data", "Disks");
  const readCache = disksByRole(pool, "read_cache", "ReadCacheDisks");
  const writeCache = disksByRole(pool, "write_cache", "WriteCacheDisks");
  // vdev 树取自池本身。有数据组时按组渲染；条带的「组」就是单盘，保持扁平。
  const groups = pool.Groups || [];
  const dataGroups = groups.filter(g => g.Role === "data");
  const specialGroups = groups.filter(g => g.Role === "special");
  const spareDisks = groups.filter(g => g.Role === "spare").flatMap(g => g.Disks || []);

  const layout = pool.Layout || "stripe";
  const width = pool.GroupWidth || 1;
  const parity = RAIDZ_PARITY[layout] || 0;
  // 按后端的规则校验「加数据盘」在当前布局下是否成立。
  const validateExpand = (picked) => {
    const n = picked.length;
    if (layout === "mirror") {
      if (n < width || n % width !== 0) return { ok: false, message: `镜像池按 ${width} 块一组加盘，请再选 ${width - (n % width)} 块（已选 ${n} 块）` };
      return { ok: true, message: `将新增 ${n / width} 组镜像` };
    }
    if (parity) {
      if (n !== width) return { ok: false, message: n < width ? `${layout} 池按整组加盘，每组 ${width} 块，请再选 ${width - n} 块（已选 ${n} 块）` : `${layout} 池按整组加盘，每组 ${width} 块，请去掉 ${n - width} 块` };
      return { ok: true, message: `将新增 1 组 ${layout}` };
    }
    return { ok: true, message: `将新增 ${n} 块条带成员，无冗余` };
  };
  const validateAttach = (picked) => picked.length === 1
    ? { ok: true, message: `${picked[0]} 将与 ${picker?.attach} 组成 RAID1，后台复制已用块，池不停机` }
    : { ok: false, message: "一次只能给一块盘加一块 RAID1 盘" };
  const validateReplace = (picked) => picked.length === 1
    ? { ok: true, message: `${picked[0]} 将顶替 ${picker?.replace}，后台重建数据，池不停机` }
    : { ok: false, message: "换盘一次只能选一块新盘" };
  const attaching = picker && typeof picker === "object" && picker.attach;
  const replacing = picker && typeof picker === "object" && picker.replace;
  const expanding = picker && typeof picker === "object" && picker.expand;
  const validateSpecial = (picked) => picked.length === 2 || picked.length === 3
    ? { ok: true, message: `将新增一组 ${picked.length} 路镜像的元数据盘，元数据与小块随后迁移过去` }
    : { ok: false, message: `元数据盘必须镜像，请选 2 或 3 块（已选 ${picked.length} 块）：它丢了整个池就丢` };
  const validateRaidzExpand = (picked) => picked.length === 1
    ? { ok: true, message: `${picked[0]} 将加入 ${picker?.expand}，全组数据重排，机械盘可能要数小时到数十小时，期间池可用` }
    : { ok: false, message: "raidz 扩容一次只能加一块盘" };

  const addFor = async (paths) => {
    const kind = picker;
    setPicker(null);
    if (attaching) await run(() => api.addPoolDisk(pool.ID, { disks: paths, mode: "attach", target: kind.attach }), `给 ${kind.attach} 加 RAID1 盘任务已提交`);
    else if (expanding) await run(() => api.addPoolDisk(pool.ID, { disks: paths, mode: "attach", target: kind.expand }), `给 ${kind.expand} 扩容任务已提交`);
    else if (replacing) await run(() => api.replacePoolDisk(pool.ID, { old_disk: kind.replace, new_disk: paths[0] }), `用 ${paths[0]} 换掉 ${kind.replace} 的任务已提交`);
    else if (kind === "special") await run(() => api.addSpecial(pool.ID, { disks: paths }), "添加元数据盘任务已提交");
    else if (kind === "spare") await run(() => api.addSpare(pool.ID, { disks: paths }), "添加热备盘任务已提交");
    else if (kind === "data") await run(() => api.addPoolDisk(pool.ID, { disks: paths }), "添加存储盘任务已提交");
    else if (kind === "read") await run(() => api.addReadCache(pool.ID, { disks: paths }), "添加读缓存任务已提交");
    else if (kind === "write") await run(() => api.addWriteCache(pool.ID, { disks: paths }), "添加写缓存任务已提交");
  };

  // 移除方式取决于池的结构：镜像成员是 detach（组变窄，不复制数据）；
  // 条带成员或整组是 remove，先把数据迁走。
  const removeDataDisk = async (path) => {
    const group = dataGroups.find(g => g.Kind === "mirror" && (g.Disks || []).some(d => d.Path === path));
    if (group) {
      const left = (group.Disks || []).length - 1;
      const ok = await conf.ask({
        title: "摘除 RAID1 盘",
        message: `摘除 ${path}？${group.Name} 将只剩 ${left} 路${left === 1 ? "，暂无冗余" : ""}。摘除是即时的，不复制数据。`,
        danger: true, confirmText: "摘除",
      });
      if (ok) await run(() => api.removePoolDisk(pool.ID, { disk: path }), "摘除 RAID1 盘任务已提交");
      return;
    }
    const ok = await conf.ask({ title: "移除磁盘", message: `从 ${pool.Name} 移除 ${path}？数据将重建到剩余磁盘。`, danger: true });
    if (ok) await run(() => api.removePoolDisk(pool.ID, { disk: path }), "移除磁盘任务已提交");
  };
  // 每块数据盘都配好空闲盘才能开始：只镜像一半的池，风险上仍等同条带。
  const upgradePairs = dataDisks.map(d => ({ target: d.Path, disk: (upgrade || {})[d.Path] || "" }));
  const upgradePlan = (() => {
    if (!upgrade) return { ok: false, message: "" };
    const missing = upgradePairs.filter(p => !p.disk).length;
    if (freeDisks.length < dataDisks.length) return { ok: false, message: `空闲盘不够：需要 ${dataDisks.length} 块，只有 ${freeDisks.length} 块` };
    if (missing) return { ok: false, message: `还有 ${missing} 块存储盘没有选 RAID1 盘` };
    return { ok: true, message: `将建 ${upgradePairs.length} 组 RAID1，完成后布局变为镜像（RAID1）` };
  })();
  const doUpgrade = async () => {
    if (!upgradePlan.ok) return;
    const ok = await run(() => api.mirrorUpgradePool(pool.ID, { pairs: upgradePairs }), `升级为 RAID1 任务已提交（${upgradePairs.length} 对）`);
    if (ok) setUpgrade(null);
  };
  const removeSpecialGroup = async (group) => {
    const ok = await conf.ask({
      title: "移除元数据盘",
      message: `移除元数据盘组 ${group.Name}（${(group.Disks || []).map(d => d.Path).join("、")}）？元数据将迁回存储盘，迁移期间池可用。`,
      danger: true, confirmText: "移除",
    });
    if (ok) await run(() => api.removeSpecial(pool.ID, { disk: group.Name }), `移除 ${group.Name} 任务已提交`);
  };
  const removeSpareDisk = async (path) => {
    const ok = await conf.ask({ title: "移除热备盘", message: `把 ${path} 从 ${pool.Name} 的热备盘里移除？坏盘将不再自动顶替。`, danger: true, confirmText: "移除" });
    if (ok) await run(() => api.removeSpare(pool.ID, { disk: path }), "移除热备盘任务已提交");
  };
  const removeGroup = async (group) => {
    const ok = await conf.ask({
      title: "移除整组",
      message: `移除整组 ${group.Name}（${(group.Disks || []).map(d => d.Path).join("、")}）？其数据将迁移到其他组，迁移期间池可用。`,
      danger: true, confirmText: "移除整组",
    });
    if (ok) await run(() => api.removePoolDisk(pool.ID, { disk: group.Name }), `移除 ${group.Name} 任务已提交`);
  };
  const removeReadCache = async (path) => run(() => api.removeReadCache(pool.ID, { disk: path }), "已移除读缓存");
  const removeWriteCache = async (path) => run(() => api.removeWriteCache(pool.ID, { disk: path }), "已移除写缓存");
  const flushWrite = async () => run(() => api.flushWriteCache(pool.ID), "写缓存刷新任务已提交");

  return (
    <Modal open onClose={onClose} size="xl"
      title={<span className="row" style={{ gap: 8 }}>
        <Icons.Database size={15} style={{ color: "var(--cyan)" }}/>
        存储池详情 · <span className="mono">{pool.Name}</span>
      </span>}
      /* 底部只放操作按钮、靠右，与客户机和镜像详情一致；关闭用标题栏的 ×。 */
      footer={<button className="btn danger" disabled={busy} onClick={onDestroy}><Icons.Trash size={12}/> {missing ? "删除记录" : "销毁存储池"}</button>}>
        <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
          <KVGrid items={[
            ["池名", pool.Name, true],
            ["服务器", serverLabel || pool.ServerID || "—", true],
            ["布局", layoutLabel(pool), false],
            // 正在进行的重建或迁移跟在状态后面显示；没有就不占一格。
            ["状态", <span className="row" style={{ gap: 6 }}><span className={`chip ${hm.cls}`}>{hm.dot && <span className={`dot ${hm.dot}`}/>}{hm.text}</span>{pool.Operation && <span className="mono meta">{OP_LABEL[pool.Operation] || pool.Operation} {pool.Progress}</span>}</span>, false],
          ]}/>

          {missing ? <div className="hint">
            本机上已找不到该池，这里只剩它的记录{(pool.Disks || []).length ? `（原成员 ${pool.Disks.join("、")}）` : ""}。删除这条记录不会改动任何磁盘，删除后可在该服务器上重新创建同名的池。
          </div> : <>
          <Section title="容量">
            <div className="row" style={{ gap: 12, marginBottom: 8 }}>
              <span className="mono" style={{ fontSize: 18, fontWeight: 600 }}>{fmtCap(used)}</span>
              <span className="mono" style={{ color: "var(--fg-faint)", fontSize: 13 }}>/ 共 {fmtCap(cap)} · 可用 {fmtCap(cap - used)} · 已使用 {usePct}%</span>
            </div>
            <div className={`bar ${usePct > 85 ? "rose" : usePct > 70 ? "amber" : ""}`}><span style={{ width: `${usePct}%` }}/></div>
          </Section>

          <DiskSection title="成员磁盘" disks={dataDisks} groups={dataGroups} busy={busy}
            onAdd={() => setPicker("data")} onRemove={removeDataDisk}
            removable={!parity && dataDisks.length > 1}
            note={parity
              ? "raidz 不能移盘（含 raidz 的池也不能做设备移除）；坏盘请用「换盘」" + (pool.RaidzExpandable ? "。本机支持 raidz 单盘扩容" : pool.RaidzExpandNote ? "。" + pool.RaidzExpandNote : "")
              : null}
            extraAction={layout === "stripe" && dataDisks.length > 1 // 单盘时和这块盘的「加 RAID1 盘」是同一件事
              ? <button className="btn" disabled={busy} style={{ padding: "3px 10px" }} onClick={() => setUpgrade({})}><Icons.Shield size={11}/> 升级为 RAID1</button>
              : null}
            groupAction={parity
              ? (pool.RaidzExpandable ? (g) => (
                <button className="btn ghost icon" disabled={busy} onClick={() => setPicker({ expand: g.Name })} title="扩一块盘" aria-label={`给 ${g.Name} 扩一块盘`}><Icons.Plus size={12}/></button>
              ) : null)
              : dataGroups.filter(g => g.Kind === "mirror").length > 1 ? (g) => (
                <button className="btn ghost icon danger" disabled={busy} onClick={() => removeGroup(g)} title="移除整组" aria-label={`移除整组 ${g.Name}`}><Icons.Trash size={12}/></button>
              ) : null}
            rowAction={(d) => (<>
              {/* 条带成员可变镜像、镜像可加一路；raidz 没有可附加的对象。换盘适用于所有布局。
                  按钮带文字，只有图标时运维找不到「换盘」。 */}
              {!parity && <button className="btn ghost" disabled={busy} style={{ padding: "3px 8px" }} onClick={() => setPicker({ attach: d.Path })} title="给这块盘配一块盘组成 RAID1" aria-label={`给 ${d.Path} 加 RAID1 盘`}><Icons.Plus size={11}/> 加 RAID1 盘</button>}
              <button className="btn ghost" disabled={busy} style={{ padding: "3px 8px" }} onClick={() => setPicker({ replace: d.Path })} title="用一块空闲盘顶替这块盘" aria-label={`换掉 ${d.Path}`}><Icons.Refresh size={11}/> 换盘</button>
            </>)}/>

          <DiskSection title="元数据盘 special" groups={specialGroups} disks={[]} busy={busy}
            onAdd={() => setPicker("special")}
            groupAction={(g) => <button className="btn ghost icon danger" disabled={busy} onClick={() => removeSpecialGroup(g)} title="移除元数据盘组" aria-label={`移除元数据盘组 ${g.Name}`}><Icons.Trash size={12}/></button>}
            note="把元数据和小块放到 SSD/NVMe 上，机械盘数据池受益最大；必须镜像，丢了整个池就丢"/>
          <DiskSection title="热备盘" disks={spareDisks} busy={busy}
            onAdd={() => setPicker("spare")} onRemove={removeSpareDisk} removable removeLabel="移除热备盘"
            note="坏盘时自动顶替（autoreplace 已随添加开启）"/>

          <DiskSection title="读缓存 L2ARC" disks={readCache} busy={busy}
            onAdd={() => setPicker("read")} onRemove={removeReadCache} removable
            note="内存装不下热点数据时再加（机械盘池 + 内存偏小的机器）；全 SSD 池不需要。坏了不影响数据"/>

          <DiskSection title="写缓存 ZIL" disks={writeCache} busy={busy}
            onAdd={() => setPicker("write")} onRemove={removeWriteCache} removable
            note="只加速同步写，无盘客户机的日常写入用不上，一般不需要；确需时用带断电保护的企业级 SSD 并镜像"
            extraAction={writeCache.length ? <button className="btn" disabled={busy} style={{ padding: "3px 10px" }} onClick={flushWrite}><Icons.Refresh size={11}/> 刷新写缓存</button> : null}/>
          </>}
        </div>

        {/* 条带升级为 RAID1：每块数据盘配一块空闲盘，一个任务完成。在线进行，ZFS 只在后台复制已用块。 */}
        <Modal open={!!upgrade} onClose={() => !busy && setUpgrade(null)} title={`升级为 RAID1 · ${pool.Name}`} size="md"
          footer={<><button className="btn" disabled={busy} onClick={() => setUpgrade(null)}>取消</button>
            <button className="btn primary" disabled={busy || !upgradePlan.ok} onClick={doUpgrade}>{busy ? "提交中…" : "开始升级"}</button></>}>
          <div className="hint" style={{ marginBottom: 10 }}>为每块存储盘选一块不小于它的空闲盘，两两组成 RAID1。升级在线进行，池不停机；每对完成后该盘即有冗余。</div>
          <div style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 6, overflow: "hidden" }}>
            <table className="t" style={{ margin: 0 }}>
              <thead><tr><th>存储盘</th><th>RAID1 盘</th></tr></thead>
              <tbody>
                {dataDisks.map(d => {
                  const takenElsewhere = Object.entries(upgrade || {}).filter(([k]) => k !== d.Path).map(([, v]) => v);
                  const options = freeDisks.filter(f => !takenElsewhere.includes(f.Path)).map(f => ({ value: f.Path, label: `${f.Path} · ${fmtCap(f.Size)}` }));
                  return (
                    <tr key={d.Path}>
                      <td className="mono" style={{ fontWeight: 600 }}>{d.Path}</td>
                      <td>
                        <Select value={(upgrade || {})[d.Path] || ""} onChange={v => setUpgrade(prev => ({ ...(prev || {}), [d.Path]: v }))}
                          aria-label={`${d.Path} 的 RAID1 盘`}
                          options={[{ value: "", label: "选择空闲盘…" }, ...options]}/>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          {upgradePlan.message && <div className="hint" style={{ marginTop: 6, color: upgradePlan.ok ? "var(--fg-mute)" : "var(--amber)" }}>{upgradePlan.message}</div>}
        </Modal>

        <DiskPickModal open={!!picker} busy={busy} freeDisks={freeDisks}
          title={attaching ? `给 ${picker.attach} 加 RAID1 盘` : expanding ? `给 ${picker.expand} 扩一块盘` : replacing ? `换掉 ${picker.replace}` : picker === "data" ? "添加存储盘" : picker === "special" ? "添加元数据盘" : picker === "spare" ? "添加热备盘" : picker === "read" ? "添加读缓存盘" : "添加写缓存盘"}
          hint={picker === "data" ? (layout === "mirror" ? `镜像池按 ${width} 块一组加盘` : parity ? `${layout} 池按整组加盘，每组 ${width} 块` : null) : replacing ? "选一块不小于原盘的空闲盘顶替它" : picker === "special" ? "选 2 或 3 块 SSD/NVMe，将组成一组镜像" : null}
          validate={attaching ? validateAttach : expanding ? validateRaidzExpand : replacing ? validateReplace : picker === "data" ? validateExpand : picker === "special" ? validateSpecial : null}
          onClose={() => setPicker(null)} onConfirm={addFor}/>
        {conf.node}
    </Modal>
  );
}

// DiskSection 是池成员的一个分区。传入 groups（该角色的 vdev 树）时，镜像或 raidz 组
// 有一行组标题，降级的组和掉线的成员放在一起看；单盘组（条带）保持扁平。
// readOnly 分区没有加减操作。
function DiskSection({ title, disks, groups, onAdd, onRemove, removable, busy, extraAction, readOnly, rowAction, groupAction, note, removeLabel = "移除" }) {
  const grouped = (groups || []).some(g => g.Kind !== "disk");
  const rows = grouped
    ? groups.flatMap(g => g.Kind === "disk"
      ? (g.Disks || []).map(d => ({ type: "disk", d }))
      : [{ type: "group", g }, ...(g.Disks || []).map(d => ({ type: "disk", d, inGroup: true }))])
    : disks.map(d => ({ type: "disk", d }));
  const memberRow = (d, inGroup) => {
    const sm = healthMeta(d.Status);
    return (
      <tr key={d.Path}>
        <td className="mono" style={{ fontWeight: 600, paddingLeft: inGroup ? 28 : undefined }}>{d.Path}</td>
        <td className="muted meta">{ROLE_LABEL[d.Role] || d.Role}</td>
        <td>{d.Status ? <span className={`chip ${sm.cls}`}>{sm.text}</span> : <span className="muted meta">—</span>}</td>
        <td className="t-actions"><div>
          {rowAction && !readOnly && rowAction(d)}
          {removable && !readOnly && <button className="btn ghost icon danger" disabled={busy} onClick={() => onRemove(d.Path)} title={removeLabel} aria-label={`${removeLabel} ${d.Path}`}><Icons.Trash size={12}/></button>}
        </div></td>
      </tr>
    );
  };
  return (
    <Section title={title} actions={readOnly ? null : <div className="row" style={{ gap: 6 }}>
      {extraAction}
      <button className="btn" disabled={busy} style={{ padding: "3px 10px" }} onClick={onAdd}><Icons.Plus size={11}/> 添加</button>
    </div>}>
      <div style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 6, overflow: "hidden" }}>
        {rows.length ? (
          <table className="t" style={{ margin: 0 }}>
            <thead><tr><th>设备</th><th>角色</th><th>状态</th><th className="t-actions"></th></tr></thead>
            <tbody>
              {rows.map(r => {
                if (r.type === "disk") return memberRow(r.d, r.inGroup);
                const gm = healthMeta(r.g.Status);
                return (
                  <tr key={r.g.Name} style={{ background: "var(--bg-1)" }}>
                    <td className="mono" style={{ fontWeight: 600 }}><span>{r.g.Name}</span> <span className="muted meta">· {LAYOUTS[r.g.Kind]?.label || r.g.Kind} · {(r.g.Disks || []).length} 块</span></td>
                    <td className="muted meta">{ROLE_LABEL[r.g.Role] || r.g.Role}</td>
                    <td>{r.g.Status ? <span className={`chip ${gm.cls}`}>{gm.text}</span> : <span className="muted meta">—</span>}</td>
                    <td className="t-actions"><div>{groupAction && !readOnly && groupAction(r.g)}</div></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        ) : (
          <div className="hint" style={{ padding: 12, textAlign: "center" }}>无</div>
        )}
      </div>
      {note && <div className="hint" style={{ marginTop: 6 }}>{note}</div>}
    </Section>
  );
}

function Section({ title, children, actions }) {
  return (
    <div>
      <div className="row" style={{ marginBottom: 8 }}>
        <div className="lbl">{title}</div>
        <div style={{ flex: 1 }}/>
        {actions}
      </div>
      {children}
    </div>
  );
}

function KVGrid({ items }) {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))", gap: 1, background: "var(--line-soft)", border: "1px solid var(--line-soft)", borderRadius: 6, overflow: "hidden" }}>
      {items.map(([k, v, mono], i) => (
        <div key={i} style={{ background: "var(--bg-1)", padding: "10px 12px" }}>
          <div className="meta" style={{ marginBottom: 3 }}>{k}</div>
          <div className={mono ? "mono" : ""} style={{ fontSize: 13, fontWeight: 500 }}>{v}</div>
        </div>
      ))}
    </div>
  );
}

window.PageStorage = PageStorage;

export { PageStorage };
