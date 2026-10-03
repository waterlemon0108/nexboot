// 总览按集群而不是本机统计，否则运维会按单节点余量估算镜像大小。
// 集群面板只在多节点时出现。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { api } from '../../lib/api.js';
import { useResource } from '../../lib/hooks.js';
import { TableState, StatCard } from '../primitives.jsx';
import { fmtCap, TASK_LABELS } from '../../lib/format.js';

const TASK_STATUS = { pending: { t: "等待", c: "" }, running: { t: "执行中", c: "cyan" }, success: { t: "成功", c: "emerald" }, failed: { t: "失败", c: "rose" } };

// 还原点类任务一天几十条，总览只留最近几条，免得把容量和镜像挤出屏幕。
const RECENT_TASKS = 5;

function poolRow(p, node, key) {
  const cap = Number(p.Capacity || 0), used = Number(p.Used || 0);
  const pct = cap ? (used / cap) * 100 : 0;
  return {
    key, node, name: p.Name, reachable: true, cap, used, pct,
    disks: (p.Disks || []).length,
    color: pct > 85 ? "amber" : pct > 70 ? "cyan" : "emerald",
  };
}

function PageOverview() {
  const store = useStore();

  const { data, loading } = useResource(async () => {
    const [tr, gr, ir, pr, kr, nr, cr] = await Promise.allSettled([
      api.listTerminals(), api.listGroups(), api.listImages(), api.listPools(), api.listTaskHistory({ page: 1, size: RECENT_TASKS }),
      api.listClusterNodes(), api.listClusterPools(),
    ]);
    const items = (r) => (r.status === "fulfilled" ? (r.value?.items || []) : []);
    return {
      terminals: items(tr), groups: items(gr), images: items(ir), pools: items(pr), tasks: items(kr),
      nodes: items(nr), cluster: cr.status === "fulfilled" ? cr.value : null,
    };
  }, []);
  const { terminals = [], groups = [], images = [], pools = [], tasks = [], nodes = [], cluster = null } = data || {};
  const online = terminals.filter(t => t.State === "online").length;
  // 多于一个节点才算集群；单节点时集群接口照样有响应，但不画集群面板。
  const clustered = nodes.length > 1;
  // 有集群数据用集群合计，否则用本机池。只计有响应的节点，不给失联节点记 0，免得低估集群容量。
  const poolCount = clustered && cluster ? cluster.pool_count : pools.length;
  const totalCap = clustered && cluster ? Number(cluster.total_capacity || 0) : pools.reduce((s, p) => s + Number(p.Capacity || 0), 0);
  const usedCap = clustered && cluster ? Number(cluster.total_used || 0) : pools.reduce((s, p) => s + Number(p.Used || 0), 0);
  const offlineNodes = nodes.filter(n => !n.online).length;
  // 放置功能之前开机的客户机没有节点字段，算在本机。
  const selfNodeID = (cluster?.nodes || []).find(n => n.is_self)?.node_id || "";
  const activeNode = nodes.find(n => n.ha_state === "active");
  // 每个池一行，多节点时标出所在节点。失联节点也占一行并注明，否则会被读成「该节点没有存储」。
  const poolRows = clustered && cluster
    ? (cluster.nodes || []).flatMap(n => (
      n.reachable
        ? (n.pools || []).map(p => poolRow(p, n.ip, `${n.node_id}/${p.Name}`))
        : [{ key: n.node_id, node: n.ip, name: "—", reachable: false, error: n.error || "读不到该节点的池" }]
    ))
    : pools.map(p => poolRow(p, "", p.ID));
  // 各节点负载用来看分布是否均衡（指定节点的分组会偏向一边）。只计在线客户机，离线的不占节点。
  const loadByNode = terminals.reduce((m, t) => {
    if (t.State !== "online") return m;
    const id = t.StorageServerID || selfNodeID;
    if (id) m[id] = (m[id] || 0) + 1;
    return m;
  }, {});
  const peakLoad = Math.max(1, ...nodes.map(n => loadByNode[n.id] || 0));
  const capacity = groups.reduce((s, g) => s + (g.ClientMax || 0), 0);
  const onlineOf = (gid) => terminals.filter(t => t.GroupID === gid && t.State === "online").length;

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 18 }}>
      {/* 汇总数据 */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(190px, 1fr))", gap: 14 }}>
        {clustered && (
          <StatCard label="集群" value={nodes.length} unit="节点"
            accent={offlineNodes ? "amber" : "emerald"}
            sub={`${offlineNodes ? `${offlineNodes} 台离线` : "全部在线"}${activeNode ? ` · 主机 ${activeNode.ip}` : ""}`}/>
        )}
        <StatCard label="在线终端" value={online} unit={`/ ${terminals.length}`} accent="emerald" sub={`${terminals.length} 台注册客户机`}/>
        <StatCard label="终端分组" value={groups.length} unit="组" accent="cyan" sub={`容量 ${capacity} 台`}/>
        <StatCard label="存储池" value={poolCount} unit="个" accent="violet" sub={`已用 ${fmtCap(usedCap)} · 可用 ${fmtCap(totalCap - usedCap)}`}/>
        <StatCard label="镜像" value={images.length} unit="个" accent="amber" sub={`${images.filter(i => i.OSType === "windows").length} Win · ${images.filter(i => i.OSType === "linux").length} Linux`}/>
      </div>

      {!loading && terminals.length === 0 && (
        <SetupGuide pools={pools} images={images} groups={groups} goto={store.goto}/>
      )}

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(360px, 1fr))", gap: 14 }}>
        {/* 分组占用 */}
        <div className="card" style={{ padding: 0 }}>
          <div className="card-h">
            <div className="card-title"><Icons.Group size={14}/> 分组在线率</div>
            <span className="chip">{groups.length} 分组</span>
          </div>
          <div style={{ padding: "16px 18px" }}>
            {groups.map(g => {
              const on = onlineOf(g.ID), cap = g.ClientMax || 0;
              const pct = cap ? (on / cap) * 100 : 0;
              const color = pct >= 90 ? "emerald" : pct >= 50 ? "cyan" : "violet";
              return (
                <div key={g.ID} style={{ marginBottom: 10, cursor: "pointer" }} onClick={() => store.goto("terminals", { groupId: g.ID })}>
                  <div style={{ display: "flex", justifyContent: "space-between", fontSize: 13, marginBottom: 4 }}>
                    <span>{g.Name}</span>
                    <span className="mono" style={{ color: "var(--fg-mute)" }}><span style={{ color: `var(--${color})` }}>{on}</span> / {cap}</span>
                  </div>
                  <div className={`bar ${color}`}><span style={{ width: `${pct}%` }}/></div>
                </div>
              );
            })}
            {!groups.length && <div style={{ fontSize: 13, color: "var(--fg-faint)", textAlign: "center", padding: 16 }}>{loading ? "加载中…" : "暂无分组"}</div>}
          </div>
        </div>

        {/* 近期任务 */}
        <div className="card" style={{ padding: 0 }}>
          <div className="card-h">
            <div className="card-title"><Icons.Tasks size={14}/> 近期任务</div>
            <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={() => store.goto("tasks")}>全部<Icons.Chevron size={11}/></button>
          </div>
          <div style={{ padding: 14 }}>
            {tasks.slice(0, RECENT_TASKS).map(t => {
              const st = TASK_STATUS[t.Status] || { t: t.Status, c: "" };
              return (
                <div key={t.ID} style={{ marginBottom: 12 }}>
                  <div style={{ display: "flex", justifyContent: "space-between", fontSize: 13, marginBottom: 5, gap: 8 }}>
                    <span className="ellip">{TASK_LABELS[t.Type] || t.Type} · <span className="muted" title={t.TargetRef || ""}>{t.TargetName || t.TargetRef || "—"}</span></span>
                    <span className={`chip ${st.c}`} style={{ flexShrink: 0 }}>{st.t}</span>
                  </div>
                  <div className={`bar ${t.Status === "success" ? "emerald" : t.Status === "failed" ? "rose" : ""}`}>
                    <span style={{ width: `${t.Status === "success" ? 100 : t.Progress || 0}%` }}/>
                  </div>
                </div>
              );
            })}
            {!tasks.length && <div style={{ fontSize: 13, color: "var(--fg-faint)", textAlign: "center", padding: 16 }}>{loading ? "加载中…" : "暂无任务"}</div>}
          </div>
        </div>
      </div>

      <div className={`overview-trio${clustered ? "" : " duo"}`}>
        {/* 节点负载 */}
        {clustered && (
          <div className="card" style={{ padding: 0 }}>
            <div className="card-h">
              <div className="card-title"><Icons.Server size={14}/> 节点负载</div>
              <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={() => store.goto("servers")}>详情<Icons.Chevron size={11}/></button>
            </div>
            <div style={{ padding: 14, display: "flex", flexDirection: "column", gap: 12 }}>
              {nodes.map(n => {
                const load = loadByNode[n.id] || 0;
                const pct = (load / peakLoad) * 100;
                return (
                  <div key={n.id}>
                    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", fontSize: 13, marginBottom: 5 }}>
                      <span style={{ display: "flex", alignItems: "baseline", gap: 8 }}>
                        <span className="mono" style={{ color: "var(--fg-mute)" }}>{n.ip}</span>
                        <span className="meta">{n.ha_state === "active" ? "主机" : "备机"}</span>
                        {!n.online && <span className="tag amber">离线</span>}
                      </span>
                      <span className="mono" style={{ color: "var(--fg-mute)" }}>{load} 台终端</span>
                    </div>
                    <div className={`bar ${n.online ? "cyan" : ""}`}><span style={{ width: `${n.online ? pct : 0}%` }}/></div>
                  </div>
                );
              })}
            </div>
          </div>
        )}
        {/* 存储容量 */}
        <div className="card" style={{ padding: 0 }}>
          <div className="card-h">
            <div className="card-title"><Icons.Database size={14}/> 存储池容量</div>
            <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={() => store.goto("storage")}>详情<Icons.Chevron size={11}/></button>
          </div>
          <div style={{ padding: 14, display: "flex", flexDirection: "column", gap: 12 }}>
            {poolRows.map(row => (
              <div key={row.key}>
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: 10, fontSize: 13, marginBottom: 5 }}>
                  <span className="mono" style={{ color: "var(--fg-mute)", display: "flex", flexWrap: "wrap", alignItems: "baseline", columnGap: 10, minWidth: 0 }}>
                    <span>{row.node && <span style={{ color: "var(--fg-faint)" }}>{row.node} </span>}{row.name}</span>
                    {row.reachable && <span className="meta">已用 {fmtCap(row.used)} · 可用 {fmtCap(row.cap - row.used)} · {row.disks} 盘</span>}
                  </span>
                  {row.reachable
                    ? <span className="mono" style={{ color: `var(--${row.color})` }}>{row.pct.toFixed(0)}%</span>
                    : <span className="mono" style={{ color: "var(--fg-faint)" }}>读不到</span>}
                </div>
                {row.reachable ? (
                  <>
                    <div className={`bar ${row.color}`}><span style={{ width: `${row.pct}%` }}/></div>
                  </>
                ) : (
                  <div className="mono meta" style={{ marginTop: 4 }}>{row.error}</div>
                )}
              </div>
            ))}
            {!poolRows.length && <div style={{ fontSize: 13, color: "var(--fg-faint)", textAlign: "center", padding: 16 }}>{loading ? "加载中…" : "暂无存储池"}</div>}
          </div>
        </div>

        {/* 镜像概览 */}
        <div className="card" style={{ padding: 0 }}>
          <div className="card-h">
            <div className="card-title"><Icons.Layers size={14}/> 镜像</div>
            <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={() => store.goto("images")}>详情<Icons.Chevron size={11}/></button>
          </div>
          {/* 四列表格在三分之一宽卡片里放不下，让它在卡内横滚，不撑出整页横向滚动条。 */}
          <div style={{ padding: 0, overflowX: "auto" }}>
            <table className="t" style={{ margin: 0 }}>
              <tbody>
                {images.map(im => (
                  <tr key={im.ID} style={{ cursor: "pointer" }} onClick={() => store.goto("images", { imageId: im.ID })}>
                    <td><span className="mono" style={{ fontWeight: 600 }}>{im.Name}</span></td>
                    <td><span className="chip">{im.OSType === "linux" ? "Linux" : im.OSType === "windows" ? "Windows" : im.OSType}</span></td>
                    <td className="mono muted" style={{ fontSize: 12 }}>逻辑 {fmtCap(im.Size)} · 实占 {im.Used == null ? "—" : fmtCap(im.Used)}</td>
                    <td><span className={`chip ${im.State === "normal" ? "emerald" : im.State === "error" ? "rose" : "cyan"}`} >{im.State === "normal" ? "就绪" : im.State === "importing" ? "导入中" : "错误"}</span></td>
                  </tr>
                ))}
                {/* colSpan 要等于列数，否则占位文字只占第一列。 */}
                <TableState colSpan={4} loading={loading} empty={!images.length} hint="暂无镜像"/>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  );
}

// 首次使用清单：注册第一台客户机前显示，各步完成状态由真实数据推出，不另存引导状态。
function SetupGuide({ pools, images, groups, goto }) {
  const steps = [
    { label: "创建存储池", desc: "把服务器磁盘组成 ZFS 池，镜像与客户机克隆都放在池里", done: pools.length > 0, page: "storage", action: "去创建" },
    { label: "导入系统镜像", desc: "把 Windows/Linux 镜像文件（vmdk / vhd / zfs 流等）导入为可启动镜像", done: images.length > 0, page: "images", action: "去导入" },
    { label: "新建终端分组", desc: "配置网段、网关与系统镜像，组内终端共享这套启动配置", done: groups.length > 0, page: "groups", action: "去新建" },
    { label: "添加客户机终端", desc: "登记终端 MAC 地址，之后 PXE 开机即可进入系统", done: false, page: "terminals", action: "去添加" },
  ];
  const next = steps.findIndex(s => !s.done);
  return (
    <div className="card" style={{ padding: 0 }}>
      <div className="card-h">
        <div className="card-title"><Icons.Tasks size={14}/> 初始化向导 · 四步完成无盘部署</div>
        <span className="chip cyan">{steps.filter(s => s.done).length} / {steps.length}</span>
      </div>
      <div style={{ padding: 16, display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(190px, 1fr))", gap: 12 }}>
        {steps.map((s, i) => (
          <div key={s.page} style={{
            border: `1px solid ${i === next ? "var(--cyan)" : "var(--line-soft)"}`, borderRadius: 8, padding: 12,
            background: s.done ? "var(--emerald-soft)" : i === next ? "var(--cyan-soft)" : "var(--bg-0)",
            opacity: !s.done && i !== next ? 0.6 : 1,
          }}>
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
              {/* 用 Icons.Check 而非 ✓ 字符，字符的粗细和基线随平台回退字体变化。 */}
              <span className={`chip ${s.done ? "emerald" : i === next ? "cyan" : ""}`} style={{ fontSize: 11 }}>{s.done ? <Icons.Check size={10}/> : i + 1}</span>
              <span style={{ fontSize: 13, fontWeight: 600 }}>{s.label}</span>
            </div>
            <div className="hint" style={{ minHeight: 48 }}>{s.desc}</div>
            {!s.done && (
              <button className={`btn ${i === next ? "primary" : ""}`} style={{ marginTop: 8, padding: "3px 10px" }} onClick={() => goto(s.page)}>{s.action}<Icons.Chevron size={11}/></button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

window.PageOverview = PageOverview;

export { PageOverview };
