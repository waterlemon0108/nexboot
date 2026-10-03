// 服务器页：集群节点、选中节点的 systemd 服务管理与数据备份。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, TableState, Toggle, Tip } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation, usePolling } from '../../lib/hooks.js';
import { fmtDateTime } from '../../lib/format.js';
const { useState } = React;

const SVC_STATUS = {
  running:        { t: "运行中", c: "emerald", dot: "live" },
  stopped:        { t: "已停止", c: "",        dot: "idle" },
  failed:         { t: "失败",   c: "rose",    dot: "err" },
  "not-installed":{ t: "未安装", c: "amber",   dot: "warn" },
  activating:     { t: "启动中", c: "cyan",    dot: "cyan" },
  deactivating:   { t: "停止中", c: "cyan",    dot: "cyan" },
  reloading:      { t: "重载中", c: "cyan",    dot: "cyan" },
  unknown:        { t: "未知",   c: "",        dot: "idle" },
};
const fmtUptime = (sec) => {
  sec = Number(sec || 0);
  if (sec <= 0) return "—";
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  return d > 0 ? `${d}天 ${h}小时` : h > 0 ? `${h}小时 ${m}分` : `${m}分`;
};
const sinceNow = (iso) => {
  if (!iso) return "—";
  const t = new Date(iso);
  if (isNaN(t)) return "—";
  return fmtUptime((Date.now() - t.getTime()) / 1000);
};

// 与高可用卡片同一套说法：同一个状态在两个页面必须叫同一个名字。
const NODE_ROLE = {
  active: { chip: "emerald", text: "主机" },
  standby: { chip: "amber", text: "备机" },
};

function fmtSeen(iso) {
  if (!iso) return "—";
  const sec = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (!Number.isFinite(sec) || sec < 0) return "—";
  if (sec < 60) return `${sec} 秒前`;
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
  return `${Math.floor(sec / 3600)} 小时前`;
}

function PageServers() {
  const store = useStore();
  const conf = useConfirm();
  const [busyKey, setBusyKey] = useState(null);
  const [logs, setLogs] = useState(null); // { key, label, text }
  const [selectedId, setSelectedId] = useState(null);


  const nodesRes = useResource(() => api.listClusterNodes().catch(() => null), []);
  const matrixRes = useResource(() => api.listClusterServices().catch(() => null), []);
  const matrix = (matrixRes.data && matrixRes.data.nodes) || [];
  // 矩阵未返回时不能把节点画成「读取不到」：那和真故障长得一样。
  const matrixReady = matrixRes.data != null;
  // 花名册给心跳，矩阵给角色、主机信息和服务。取并集：花名册读不到时仍能显示矩阵里的节点。
  const roster = (nodesRes.data && nodesRes.data.items) || [];
  const nodes = mergeNodes(roster, matrix);
  const clustered = nodes.length > 1;
  const load = () => { nodesRes.reload(); matrixRes.reload(); };

  // 只在首次或选中节点消失时才默认选主机；切换时把选中拽走会让人以为看错了机器。
  const known = nodes.some(n => n.id === selectedId);
  const fallbackId = (nodes.find(n => n.ha_state === "active" && n.online)
    || nodes.find(n => n.online) || nodes[0] || {}).id || null;
  const activeId = known ? selectedId : fallbackId;
  const selectedNode = nodes.find(n => n.id === activeId) || null;
  const selectedView = matrix.find(m => m.node_id === activeId) || null;

  // 有服务正在启停时自动刷新，让状态自己走完而不是让人手点。
  const transitioning = (selectedView?.services || [])
    .some(s => ["activating", "deactivating", "reloading"].includes(s.status));
  usePolling(matrixRes.reload, { intervalMs: 2500, active: transitioning });

  // 确认框写明是哪台机器：集群里点错节点，影响的是另一批客户机。
  const actOnNode = async (node, svc, action) => {
    const where = node.ip || node.id;
    const ok = await conf.ask({
      title: `${action === "restart" ? "重启" : action === "stop" ? "停止" : "启动"}服务`,
      message: `将在 ${where} 上${action === "restart" ? "重启" : action === "stop" ? "停止" : "启动"} ${svc.capability || svc.label}（${svc.unit}）。`
        + (svc.critical || action === "stop" ? "这是无盘启动关键服务，期间该节点上的客户机会受影响。" : ""),
      danger: action === "stop", confirmText: action === "restart" ? "重启" : "确定",
    });
    if (!ok) return;
    setBusyKey(`${node.id}/${svc.key}`);
    try {
      await api.clusterServiceAction(node.id, svc.key, action);
      store.toast(`${where} · ${svc.capability || svc.label} 指令已下发`, "ok");
      setTimeout(() => matrixRes.reload(), 800);
    } catch (e) { store.toast(e.message || "操作失败", "err"); }
    finally { setBusyKey(null); }
  };
  // 移出不可逆，要求照地址输入确认；只对离线节点提供，移除在线节点几乎总是误操作。
  const forgetNode = async (node) => {
    const where = node.ip || node.id;
    const ok = await conf.ask({
      title: "把节点移出集群",
      message: `${where} 将从花名册中移除：它不再出现在集群视图里，也不再参与主机选举。\n`
        + `这不会动它本机上的任何数据。若它日后重新上线并指向本集群，会作为新节点重新加入。`,
      danger: true, confirmText: "移除", typeToConfirm: where,
    });
    if (!ok) return;
    try {
      await api.forgetClusterNode(node.id);
      store.toast(`${where} 已移出集群`, "ok");
      nodesRes.reload();
      matrixRes.reload();
    } catch (e) { store.toast(e.message || "移除失败", "err"); }
  };
  const openNodeLogs = async (node, svc) => {
    const label = `${node.ip || node.id} · ${svc.capability || svc.label}`;
    setLogs({ key: `${node.id}/${svc.key}`, label, text: "加载中…" });
    try { const r = await api.clusterServiceLogs(node.id, svc.key, 200); setLogs({ key: `${node.id}/${svc.key}`, label, text: r.logs || "(无日志)" }); }
    catch (e) { setLogs({ key: `${node.id}/${svc.key}`, label, text: e.message || "日志读取失败" }); }
  };
  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      {/* 「本机」只是此刻持有虚 IP 的机器，所以不单列本机信息：表里选一台，下面显示它。 */}
      <ClusterNodes nodes={nodes} matrix={matrix} matrixReady={matrixReady} selected={selectedId} onSelect={setSelectedId}
        onRefresh={load} onForget={clustered ? forgetNode : null}/>

      <NodeServices node={selectedNode} view={selectedView} matrixReady={matrixReady}
        onAct={actOnNode} onLogs={openNodeLogs} busyKey={busyKey}/>

      <BackupSection/>

      {logs && (
        <Modal open onClose={() => setLogs(null)} title={`日志 · ${logs.label}`} size="xl"
          footer={<button className="btn" onClick={() => setLogs(null)}>关闭</button>}>
          <pre style={{ margin: 0, maxHeight: "60vh", overflow: "auto", background: "var(--bg-0)", border: "1px solid var(--line)", borderRadius: 6, padding: 12, fontSize: 12, fontFamily: "var(--font-mono)", whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{logs.text}</pre>
        </Modal>
      )}
      {conf.node}
    </div>
  );
}

// 没有 error 只代表没有失败记录；有快照才算「正常」，否则从没备份过的机器也会满屏绿色。
function backupChip(item, cfg) {
  if (item.error) return <span className="chip rose">{item.error}</span>;
  if (item.last_snapshot) return <span className="chip emerald">正常</span>;
  if (!cfg?.backup_pool) return <span className="chip">未配置</span>;
  return <span className="chip amber">待备份</span>;
}

function BackupSection() {
  const store = useStore();
  const [form, setForm] = useState(null);

  const { data: status, reload: load } = useResource(async () => {
    try {
      const s = await api.getBackupStatus();
      setForm(formOf(s.config));
      return s;
    } catch { return null; }
  }, []);
  const nodesRes = useResource(() => api.listClusterBackups().catch(() => null), []);
  const { busy, run: runMut } = useMutation(store.toast);

  // 改动按保存才生效；与已存配置相同时按钮置灰。
  const save = () => runMut(() => api.saveBackupConfig({ enabled: form.enabled, schedule: scheduleOf(form) }),
    "备份计划已保存", { reload: () => { load(); nodesRes.reload(); }, errMsg: "保存失败" });
  const run = () => runMut(() => api.runBackup(), "已开始备份，各节点会各自更新自己的副本",
    { reload: () => { load(); nodesRes.reload(); }, errMsg: "发起失败" });

  const cfg = status?.config;
  // dirty 由表单与已存配置比较得出而非标志位，改回原样后按钮才能重新变灰。
  const dirty = !!form && !!cfg
    && (form.enabled !== !!cfg.enabled || (form.enabled && scheduleOf(form) !== (cfg.schedule || "")));
  const running = !!status?.task && (status.task.Status === "running" || status.task.Status === "pending");
  const rows = (nodesRes.data && nodesRes.data.nodes) || [];

  return (
    <div className="card" style={{ padding: 0 }}>
      <div className="card-h">
        <div className="card-title"><Icons.Database size={14}/> 数据备份</div>
        <div className="row" style={{ gap: 6 }}>
          {running && <span className="chip cyan"><span className="dot cyan"/> 备份中</span>}
          <button className="btn" disabled={busy || running} onClick={run}><Icons.Backup size={12}/> 立即备份</button>
        </div>
      </div>
      <div style={{ padding: 16, display: "flex", flexDirection: "column", gap: 14 }}>
        {/* 备份池不让手填：每台最多两个池，非数据池的那个就是备份池。计划只选时刻，不暴露 cron。 */}
        {/* 当前计划跟在保存按钮后面，它是这排控件的结果。 */}
        {form && (
          <div data-testid="backup-controls" className="row" style={{ gap: 10, alignItems: "center" }}>
            {/* 启用开关并进频率下拉：未保存时显示「已启用」会误导。
                下拉默认 width:100%，必须给宽度，否则一个就占满整行。 */}
            <Select value={form.enabled ? form.every : "off"} aria-label="备份计划" style={{ width: 104 }}
              onChange={v => setForm(v === "off" ? { ...form, enabled: false }
                : { ...form, enabled: true, every: v })}
              options={[{ value: "off", label: "禁用" }, { value: "daily", label: "每天" }, { value: "weekly", label: "每周" }]}/>
            {form.enabled && form.every === "weekly" && (
              <Select value={String(form.weekday)} aria-label="星期" style={{ width: 92 }}
                onChange={v => setForm({ ...form, weekday: Number(v) })} options={WEEKDAYS}/>
            )}
            {/* 禁用时隐藏时刻；值保留在表单里，重新启用仍是原时刻。 */}
            {form.enabled && (
              <input className="input mono" type="time" style={{ width: 110 }} aria-label="时刻"
                value={form.time} onChange={e => setForm({ ...form, time: e.target.value })}/>
            )}
            <button className="btn primary" disabled={busy || !dirty} onClick={save}>
              <Icons.Check size={12}/> {busy ? "保存中…" : "保存"}
            </button>
            {cfg?.enabled && cfg?.next_run_at && (
              <span className="meta">下次 <span className="mono">{cfg.next_run_text || fmtDateTime(cfg.next_run_at)}</span></span>
            )}
          </div>
        )}

        {/* 集群里每台各自把副本推到本机备份池，所以按节点列出，回答「几台手上有副本」。 */}
        <div data-testid="backup-nodes" style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 6, overflow: "hidden" }}>
          <table className="t" style={{ margin: 0 }}>
            <thead><tr><th>节点</th><th>角色</th><th>备份池</th><th>副本时刻</th><th>状态</th></tr></thead>
            <tbody>
              {rows.length === 0 && (
                <tr><td colSpan={5} className="meta" style={{ padding: 12 }}>还读不到各节点的备份状态</td></tr>
              )}
              {rows.map(n => (
                <tr key={n.node_id}>
                  <td className="mono">{n.ip || n.node_id}</td>
                  <td><span className={`chip ${NODE_ROLE[n.role]?.chip || ""}`}>{NODE_ROLE[n.role]?.text || n.role || "—"}</span></td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{n.backup_pool || "—"}</td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{n.last_backup_at ? fmtDateTime(n.last_backup_at) : "—"}</td>
                  <td>{nodeBackupChip(n)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {!status && <div style={{ fontSize: 13, color: "var(--fg-faint)" }}>未获取到备份状态</div>}
      </div>
    </div>
  );
}

const WEEKDAYS = [
  { value: "1", label: "周一" }, { value: "2", label: "周二" }, { value: "3", label: "周三" },
  { value: "4", label: "周四" }, { value: "5", label: "周五" }, { value: "6", label: "周六" },
  { value: "0", label: "周日" },
];

// formOf / scheduleOf 在存储形态（daily@03:00）和界面形态（频率 + 时刻）之间互转。
function formOf(cfg) {
  const raw = (cfg?.schedule || "").trim();
  const base = { enabled: !!cfg?.enabled, every: "daily", weekday: 0, time: "03:00" };
  const parts = raw.split("@");
  if (parts[0] === "daily" && parts[1]) return { ...base, every: "daily", time: parts[1] };
  if (parts[0] === "weekly" && parts[2]) return { ...base, every: "weekly", weekday: Number(parts[1]) || 0, time: parts[2] };
  return base;
}

function scheduleOf(form) {
  const time = (form.time || "03:00").slice(0, 5);
  return form.every === "weekly" ? `weekly@${form.weekday}@${time}` : `daily@${time}`;
}

// 状态回答「这台有没有可用于恢复的副本」；没有备份池单独标出，不算正常也不算故障。
function nodeBackupChip(n) {
  if (!n.reachable) return <span className="chip rose">{n.error || "读不到"}</span>;
  if (!n.backup_pool) return <span className="chip">没有备份池</span>;
  if (n.copying) return <span className="chip cyan"><span className="dot cyan"/>备份中</span>;
  if (!n.last_backup_at) return <span className="chip amber">待备份</span>;
  if (n.pending) return <span className="chip amber">待同步</span>;
  return <span className="chip emerald"><span className="dot live"/>正常</span>;
}

function mergeNodes(roster, matrix) {
  const out = roster.map(n => ({ ...n }));
  const byId = {};
  for (const n of out) byId[n.id] = n;
  for (const m of matrix) {
    const hit = byId[m.node_id];
    if (hit) {
      // 节点自己上报的角色优先：花名册是复制来的，会落后一拍。
      if (m.role) hit.ha_state = m.role;
      continue;
    }
    out.push({ id: m.node_id, ip: m.ip, ha_state: m.role, online: m.online });
  }
  return out.sort((a, b) => (b.ha_state === "active") - (a.ha_state === "active")
    || String(a.ip || "").localeCompare(String(b.ip || "")));
}

// 主机名和操作系统不参与决策，只放在悬停里。
function nodeHint(node, view) {
  const h = (view && view.host) || {};
  const parts = [node.ip || node.id];
  if (h.hostname) parts.push(`主机名 ${h.hostname}`);
  if (h.os) parts.push(h.os);
  return parts.join(" · ");
}

function ClusterNodes({ nodes, matrix, matrixReady, selected, onSelect, onRefresh, onForget }) {
  const [adopting, setAdopting] = useState(false);
  // 虚 IP 只有主机报得准：它是「此刻谁在服务」的那个地址。
  const vip = (nodes.find(n => n.ha_state === "active") || {}).portal_ip || "";
  const byId = {};
  for (const m of matrix) byId[m.node_id] = m;
  const ordered = [...nodes].sort((a, b) => (b.ha_state === "active") - (a.ha_state === "active"));
  const faults = matrix.reduce((n, m) => n + (m.reachable ? (m.services || []).filter(s => !s.ok).length : 0), 0);
  const offline = nodes.filter(n => !n.online).length;

  // 服务列的顺序与表头缩写，取自任意一台可达节点——受管单元是固定的一组。
  const sample = matrix.find(m => m.reachable && (m.services || []).length) || {};
  const columns = (sample.services || []).map(s => ({ key: s.key, label: s.capability || shortLabel(s.label) }));

  return (
    <div className="card" style={{ padding: 0 }} data-testid="cluster-nodes">
      <div className="card-h">
        <div className="card-title">
          <Icons.Server size={14}/> 集群节点
          {/* 虚 IP 是集群入口，放在卡片标题上，不挂在主机行下重复说明谁是主机。 */}
          {vip && <span className="mono meta" style={{ marginLeft: 10, fontWeight: 400 }}>虚 IP {vip}</span>}
        </div>
        <div className="row" style={{ gap: 6 }}>
          <span className="chip">{nodes.length} 节点</span>
          {offline > 0 && <span className="chip rose">{offline} 台离线</span>}
          <span className={`chip ${faults ? "amber" : "emerald"}`}>
            {faults ? `${faults} 处异常` : "全部就绪"}
          </span>
          <button className="btn" onClick={() => setAdopting(true)}>添加节点…</button>
          <button className="btn ghost icon" onClick={onRefresh} title="立即刷新" aria-label="立即刷新"><Icons.Refresh size={14}/></button>
        </div>
      </div>
      {adopting && <AdoptNodes onClose={() => { setAdopting(false); onRefresh && onRefresh(); }}/>}
      <div style={{ overflowX: "auto" }}>
        <table className="t">
          <thead>
            <tr>
              <th>节点</th>
              <th>角色</th>
              {/* 每个服务单独一列，不要挤进一个单元格靠固定宽度对齐：文案长度一变就错位。 */}
              {columns.map(c => <th key={c.key}>{c.label}</th>)}
              <th>状态</th>
              <th>心跳</th>
              {/* 滚动升级时各节点版本不同，复制和协议可能对不上，要能横向一眼看出。 */}
              <th>版本</th>
              {/* 已运行横着比才有意义：一台刚起来而别人没动过，说明它自己重启了。 */}
              <th>已运行</th>
              <th className="t-actions"></th>
            </tr>
          </thead>
          <tbody>
            {ordered.map(n => {
              const m = byId[n.id] || {};
              const services = m.services || [];
              const bad = services.filter(s => !s.ok).length;
              const primary = n.ha_state === "active";
              const picked = n.id === selected;
              return (
                <React.Fragment key={n.id}>
                <tr onClick={() => onSelect(n.id)} aria-selected={picked} title={nodeHint(n, m)}
                  style={{ cursor: "pointer", background: picked ? "var(--bg-0)" : undefined,
                    boxShadow: picked ? "inset 2px 0 0 var(--accent)" : undefined }}>
                  <td className="mono">{n.ip || n.id}</td>
                  <td>
                    <span className={`chip ${NODE_ROLE[n.ha_state]?.chip || ""}`}>
                      {NODE_ROLE[n.ha_state]?.text || n.ha_state || "—"}
                    </span>

                  </td>
                  {!n.online || !m.reachable
                    ? <td colSpan={Math.max(1, columns.length)}><span className="meta">
                        {!n.online ? "不可达" : !matrixReady ? "读取中…" : (m.error || "服务状态读取不到")}
                      </span></td>
                    : columns.map(c => {
                      const s = services.find(x => x.key === c.key);
                      if (!s) return <td key={c.key} className="meta">—</td>;
                      const verdict = capabilityState(s);
                      return (
                        <td key={c.key} title={cellHint(s, n)}>
                          <span className={`chip ${verdict.chip}`} style={{ padding: "1px 7px" }}>
                            {verdict.chip === "emerald" && <span className="dot live"/>}{verdict.text}
                          </span>
                        </td>
                      );
                    })}
                  <td>
                    {!n.online
                      ? <span className="chip rose">离线</span>
                      : bad
                        ? <span className="chip amber">{bad} 处异常</span>
                        : <span className="chip emerald"><span className="dot live"/>正常</span>}
                  </td>
                  <td className="meta">{fmtSeen(n.last_seen_at)}</td>
                  <td className="mono meta">{m.host?.version || "—"}</td>
                  <td className="meta">{m.host?.uptime_sec ? fmtUptime(m.host.uptime_sec) : "—"}</td>
                  <td className="t-actions"><div>
                    {onForget && !n.online && (
                      <button className="btn ghost icon danger" title="把这台移出集群"
                        aria-label={`移除节点 ${n.ip || n.id}`}
                        onClick={e => { e.stopPropagation(); onForget(n); }}><Icons.Trash size={12}/></button>
                    )}
                  </div></td>
                </tr>
                </React.Fragment>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// 只给两处易困惑的格子加说明：备机 DHCP 停着是对的（两个 authoritative 的 dnsmasq
// 会互相拒绝客户机）；iSCSI 单元加载完就退出，真正在跑的是内核态。
function cellHint(svc, node) {
  const name = svc.capability || shortLabel(svc.label);
  const base = `${name} · ${capabilityState(svc).text}（${svc.unit}）`;
  if (svc.expected === "stopped" && node.ha_state !== "active") {
    return `${base}\n备机不跑 DHCP 是对的：两个 DHCP 会互相拒绝客户机。`;
  }
  if (svc.expected === "ready") {
    return `${base}\n单元开机加载完就退出，真正在跑的是内核态。`;
  }
  return base;
}

// AdoptNodes 把同网段里尚未配置的机器拉进集群。不需要填任何字段：虚 IP 和集群令牌
// 集群自己知道。可列出的节点没有数据池，拉错也不会毁掉数据。
function AdoptNodes({ onClose }) {
  const store = useStore();
  const { busy, run } = useMutation(store.toast);
  const { data, loading, error, reload } = useResource(() => api.discoverNodes(), []);
  const items = (data && data.items) || [];

  const adopt = async (c) => {
    if (await run(() => api.adoptNode({ address: c.address }),
      `已受理：${c.address} 将重启为备机加入集群，随后到「存储池管理」给它建数据池`)) {
      reload();
    }
  };

  return (
    <Modal open onClose={onClose} title="添加节点" size="lg"
      footer={<>
        <button className="btn" onClick={reload} disabled={busy || loading}>重新扫描</button>
        <button className="btn primary" onClick={onClose}>完成</button>
      </>}>
      <div data-testid="adopt-nodes" style={{ display: "grid", gap: 12 }}>
        <div className="hint">
          这里列出客户机网段里<b>已装好、还没配置过</b>的机器，点「加入集群」即可，不用填任何东西。
          加入后到「存储池管理」给它建数据池。建过池的机器不会出现在这里，只能用命令行加入。
        </div>
        {loading && <div className="hint">正在扫描客户机网段…</div>}
        {error && <div style={{ fontSize: 13, color: "var(--rose)" }}>扫描失败：{error}</div>}
        {!loading && !error && items.length === 0 && (
          <div className="hint">
            没有发现可加入的机器。请确认它已安装并启动、在同一客户机网段、还没建过数据池；重装过的旧节点要先「移出集群」。
          </div>
        )}
        {items.length > 0 && (
          <table className="t">
            <thead><tr><th>地址</th><th>节点标识</th><th>版本</th><th></th></tr></thead>
            <tbody>
              {items.map(c => (
                <tr key={c.address}>
                  <td className="mono">{c.address}</td>
                  <td className="mono meta">{c.node_id}</td>
                  <td className="meta">{c.version || "—"}</td>
                  <td style={{ textAlign: "right" }}>
                    <button className="btn" disabled={busy} onClick={() => adopt(c)}>加入集群</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </Modal>
  );
}

// NodeServices 显示选中节点的服务；标题写明是哪台，原因同 actOnNode 的确认框。
function NodeServices({ node, view, matrixReady, onAct, onLogs, busyKey }) {
  const [expanded, setExpanded] = useState(null);
  if (!node) {
    return (
      <div className="card" style={{ padding: 16 }}>
        <span className="meta">还没有可管理的节点</span>
      </div>
    );
  }
  const where = node.ip || node.id;
  const role = NODE_ROLE[node.ha_state]?.text || node.ha_state || "";
  const services = (view && view.services) || [];
  const bad = services.filter(s => s.ok === false).length;
  const unreachable = !node.online || !view || !view.reachable;
  const pending = node.online && !matrixReady;
  return (
    <div className="card" style={{ padding: 0 }} data-testid="node-services">
      <div className="card-h">
        <div className="card-title">
          <Icons.Cog size={14}/> 无盘服务器 · <span className="mono">{where}</span>
          {role && <span className={`chip ${NODE_ROLE[node.ha_state]?.chip || ""}`} style={{ marginLeft: 8 }}>{role}</span>}
        </div>
        <span className={`chip ${pending ? "" : unreachable ? "rose" : bad ? "amber" : "emerald"}`}>
          {pending ? "读取中…" : unreachable ? (node.online ? "服务状态读取不到" : "不可达") : bad ? `${bad} 处异常` : "全部运行中"}
        </span>
      </div>
      {unreachable ? (
        <div style={{ padding: 16 }}>
          <span className="meta">
            {pending ? "正在读取服务状态…" : node.online ? "读不到这台的服务状态，无法在此操作" : "节点不可达，无法在此操作"}
          </span>
        </div>
      ) : (
      <>
      <table className="t" style={{ background: "transparent" }}>
        {/* 版面上只回答「能否提供这项能力」；systemd 单元、开机自启、PID 放进详情。 */}
        <thead><tr><th>服务</th><th>状态</th><th className="t-actions">操作</th></tr></thead>
        <tbody>
          {services.map(svc => {
            const verdict = capabilityState(svc);
            const busy = busyKey === `${node.id}/${svc.key}`;
            const open = expanded === svc.key;
            return (
              <React.Fragment key={svc.key}>
              <tr>
                <td>
                  <div className="row" style={{ gap: 8 }}>
                    <span style={{ fontWeight: 600 }}>{svc.label || svc.capability}</span>
                    {svc.critical && <span className="chip amber">关键</span>}
                    {/* 只对这一行成立的说明挂在这一行上，不放到表格底部。 */}
                    {svc.read_only && (
                      <Tip label={`${svc.label || svc.capability}说明`} text="控制服务自身只读：重启它会切断这条连接。请在该节点上执行 systemctl restart ndiskless。"/>
                    )}
                  </div>
                </td>
                <td>
                  <span className={`chip ${verdict.chip}`}>
                    {verdict.chip === "emerald" && <span className="dot live"/>}{verdict.text}
                  </span>
                </td>
                <td className="t-actions"><div>
                  {/* 应停着的服务不给「重启」：systemctl restart 会把它拉起来，备机上的 dnsmasq
                      会和主机互相拒绝客户机。只有它确实在跑时才给「停止」。 */}
                  {!svc.read_only && svc.installed !== false && (
                    svc.expected === "stopped"
                      ? svc.status === "running" && (
                        <button className="btn" disabled={busy} style={{ padding: "3px 8px" }}
                          onClick={() => onAct(node, svc, "stop")}>停止</button>
                      )
                      : <button className="btn" disabled={busy} style={{ padding: "3px 8px" }}
                        onClick={() => onAct(node, svc, "restart")}>重启</button>
                  )}
                  <button className="btn ghost" style={{ padding: "3px 8px" }}
                    onClick={() => onLogs(node, svc)}><Icons.Log size={11}/> 日志</button>
                  {/* 单元状态、运行时长、PID 放进详情，不常驻版面。 */}
                  <button className="btn ghost" aria-label="展开详情" style={{ padding: "3px 8px" }}
                    onClick={() => setExpanded(open ? null : svc.key)}>{open ? "收起" : "详情"}</button>
                </div></td>
              </tr>
              {open && (
                <tr>
                  <td colSpan={3} style={{ background: "var(--bg-0)" }}>
                    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))", gap: 10, padding: "4px 2px" }}>
                      <Info label="systemd 单元" value={svc.unit}/>
                      <Info label="开机自启" value={bootText(svc)}/>
                      <Info label="systemd 单元状态" value={svc.expected === "ready"
                        ? `${(SVC_STATUS[svc.status] || SVC_STATUS.unknown).t}（开机加载完即退出，服务由内核态提供）`
                        : (SVC_STATUS[svc.status] || SVC_STATUS.unknown).t}/>
                      <Info label="运行时长" value={sinceNow(svc.active_since)}/>
                      <Info label="PID" value={svc.main_pid > 0 ? String(svc.main_pid) : "—"}/>
                    </div>
                    {!svc.read_only && svc.installed !== false && svc.expected !== "stopped" && (
                      <div className="row" style={{ gap: 6, marginTop: 8 }}>
                        {svc.status !== "running" && (
                          <button className="btn" disabled={busy} style={{ padding: "3px 8px" }}
                            onClick={() => onAct(node, svc, "start")}>启动单元</button>
                        )}
                        {svc.status === "running" && (
                          <button className="btn ghost danger" disabled={busy} style={{ padding: "3px 8px" }}
                            onClick={() => onAct(node, svc, "stop")}>停止单元</button>
                        )}
                      </div>
                    )}
                  </td>
                </tr>
              )}
              </React.Fragment>
            );
          })}
        </tbody>
      </table>
      </>
      )}
    </div>
  );
}

// 表头已写服务名，格子里不再重复；副名（"· DHCP/TFTP/PXE"）进悬停。
function shortLabel(label) {
  return String(label || "").split(" · ")[0];
}

// capabilityState 只给三种结论：本机在提供、由主机提供、异常。
function capabilityState(svc) {
  if (svc.ok === false) return { chip: "rose", text: "异常" };
  if (svc.provided === false) return { chip: "", text: "由主机提供" };
  // iSCSI 内核态就绪与 dnsmasq 进程在跑功能上相同，统一叫「运行中」。
  return { chip: "emerald", text: "运行中" };
}

// oneshot 单元的开机自启无意义（内核模块由 modules-load.d 装载，导出按需创建），
// 显示「不需要」而不是让人误以为漏配的「已禁用」。
function bootText(svc) {
  if (!svc.installed) return "—";
  if (svc.expected === "ready") return "不需要（内核模块随系统加载）";
  return svc.enabled ? "已启用" : "已禁用";
}

// localVerdict 把结论（上版面，与集群列表同一说法）和 systemd 实况（进悬停）分开。
// ok 缺席时（旧后端）按原状态着色。
function localVerdict(svc, st) {
  if (svc.ok === undefined) return { c: st.c, dot: st.dot, t: st.t, title: svc.unit };
  return {
    c: svc.ok ? "emerald" : "rose",
    dot: svc.ok ? "live" : "",
    t: stateText(svc),
    title: svc.detail ? `${svc.detail}（systemd 单元：${st.t}）` : `systemd 单元：${st.t}`,
  };
}

// stateText 给结论而不是 systemd 术语。
function stateText(s) {
  if (s.expected === "ready") return s.ok ? "就绪" : "缺失";
  if (s.expected === "stopped") return s.ok ? "已停止" : "不应运行";
  if (s.ok) return "运行中";
  return s.status === "not-installed" ? "未安装" : "已停止";
}

function Info({ label, value }) {
  return (
    <div>
      <div className="lbl" style={{ marginBottom: 4 }}>{label}</div>
      <div className="mono ellip" style={{ fontSize: 14, fontWeight: 500 }}>{value}</div>
    </div>
  );
}

window.PageServers = PageServers;

export { PageServers };
