// 后端客户机状态只有在线、离线（旧数据的 unknown 按离线），没有电源和负载字段，页面不做这些显示。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, TableState, SearchBox, SegFilter, Pager } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { redName, fmtCap } from '../../lib/format.js';
import { useResource, useMutation, usePolling, usePaged } from '../../lib/hooks.js';
const { useState, useEffect, useRef } = React;

const STATE = {
  online:  { t: "在线", c: "emerald", dot: "live" },
  offline: { t: "离线", c: "",        dot: "idle" },
};
// 只有在线、离线两种：没连过的（旧数据里的 unknown）就是离线。
const stateOf = (t) => (t?.State === "online" ? "online" : "offline");
const terminalName = (t) => String(t?.Name || t?.name || "").trim();
const terminalTitle = (t) => terminalName(t) || t?.ID || "—";
const terminalForm = (t) => ({
  id: t.ID,
  name: terminalName(t),
  // 和别处一样按冒号分隔显示，保存时由后端规范化。
  mac: fmtMac(t.MAC),
  ip: t.IP,
  group_id: t.GroupID,
  is_super: t.IsSuper,
  state: t.State || "offline",
});
// 分组地址区间是 [StartIP, StartIP + ClientMax - 1]，与后端（assets/addresspool.go）一样按 uint32 算，
// 要跨字节进位，不能只加最后一段。
const ipToInt = (value) => {
  const parts = String(value || "").trim().split(".");
  if (parts.length !== 4) return null;
  let n = 0;
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null;
    const v = Number(part);
    if (v > 255) return null;
    n = n * 256 + v;
  }
  return n;
};
const intToIp = (n) => [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join(".");
const ipRangeEnd = (startIP, max) => {
  const start = ipToInt(startIP);
  if (start === null || !max) return startIP || "—";
  return intToIp(start + Math.max(0, max - 1));
};
// 解析不了的输入不提示：合法性以后端为准，输入一半就报警只是噪音。
const ipInGroupPool = (ip, group) => {
  const n = ipToInt(ip);
  const start = ipToInt(group && group.StartIP);
  if (n === null || start === null || !(group && group.ClientMax)) return true;
  return n >= start && n <= start + group.ClientMax - 1;
};
// 后端存规范化 MAC（无分隔符、大写，如 525400AABBCC）；显示时插回冒号，搜索时去掉。
const normMac = (s) => String(s || "").replace(/[:\-.\s]/g, "").toUpperCase();
const fmtMac = (s) => normMac(s).match(/.{1,2}/g)?.join(":") || (s || "");
const pad2 = (n) => String(n).padStart(2, "0");
const fmtTime = (iso) => {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d)) return "—";
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
};

async function downloadRaw(res, filename) {
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url; a.download = filename;
  document.body.appendChild(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
}

function PageTerminals() {
  const store = useStore();
  const conf = useConfirm();
  const { route, goto } = store;

  const [view, setView] = useState("table");
  const [groupFilter, setGroupFilter] = useState(route.params.groupId || "all");
  const [stateFilter, setStateFilter] = useState("all");
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState([]);
  const [detailId, setDetailId] = useState(null);
  const [editId, setEditId] = useState(null);
  const [modal, setModal] = useState(null); // add | bulk-group | stopSuper | injectDriver
  const [form, setForm] = useState({});
  const fileRef = useRef(null);

  // 分开 settle：分组、镜像信息获取失败不能清空客户机列表。
  const { data, loading, error, reload: load } = useResource(async () => {
    const [tr, gr, ir, br, nr] = await Promise.allSettled([api.listTerminals(), api.listGroups(), api.listImages(), api.listDriverBundles(), api.listClusterNodes()]);
    if (tr.status === "rejected") throw tr.reason;
    return {
      terminals: (tr.value && tr.value.items) || [],
      groups: gr.status === "fulfilled" ? (gr.value && gr.value.items) || [] : [],
      images: ir.status === "fulfilled" ? (ir.value && ir.value.items) || [] : [],
      bundles: br.status === "fulfilled" ? (br.value && br.value.items) || [] : [],
      nodes: nr.status === "fulfilled" ? (nr.value && nr.value.items) || [] : [],
    };
  }, []);
  const terminals = (data && data.terminals) || [];
  const groups = (data && data.groups) || [];
  const images = (data && data.images) || [];
  const bundles = (data && data.bundles) || [];
  const nodes = (data && data.nodes) || [];
  // 机器慢时先看盘在哪台；放置自动分摊后猜不出来，所以多节点才显示这一列。
  const clustered = nodes.length > 1;
  const nodeLabel = (t) => {
    const id = t.StorageServerID;
    // 没记落点的是放置功能之前开过机的客户机；运维面对的是 VIP，不能显示成「本机」。
    if (!id) return "未记录";
    return (nodes.find(n => n.id === id) || {}).ip || id;
  };
  const { busy, run } = useMutation(store.toast);

  // 在线状态由服务端按 iSCSI 会话推出，需定时刷新；详情和编辑表单以 [t.ID, mode] 为 key，
  // 后台刷新不会冲掉正在编辑的内容。
  usePolling(load, { intervalMs: 15000 });

  useEffect(() => { if (route.params.groupId) setGroupFilter(route.params.groupId); }, [route.params.groupId]);

  const groupById = Object.fromEntries(groups.map(g => [g.ID, g]));
  const imageById = Object.fromEntries(images.map(i => [i.ID, i]));
  const bundleNameById = Object.fromEntries(bundles.map(b => [b.bundle.ID, b.bundle.Name]));
  const imageNameOf = (groupId) => {
    const g = groupById[groupId];
    return (g && imageById[g.SystemImageID] && imageById[g.SystemImageID].Name) || "—";
  };

  const filtered = terminals.filter(t =>
    (groupFilter === "all" || t.GroupID === groupFilter) &&
    (stateFilter === "all" || stateOf(t) === stateFilter) &&
    (!search ||
      terminalTitle(t).toLowerCase().includes(search.toLowerCase()) ||
      t.ID.toLowerCase().includes(search.toLowerCase()) ||
      normMac(t.MAC).includes(normMac(search)) ||
      (t.IP || "").includes(search))
  );
  const stateCount = terminals.reduce((m, t) => ({ ...m, [stateOf(t)]: (m[stateOf(t)] || 0) + 1 }), {});
  const detail = terminals.find(t => t.ID === detailId) || null;
  const editing = terminals.find(t => t.ID === editId) || null;

  // 「全选」只选当前页，批量操作不会碰到用户没看见的行。
  const paged = usePaged(filtered);
  const visible = view === "table" ? paged.rows : filtered;

  const toggleSel = (id) => setSelectedIds(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  const selectAll = () => setSelectedIds(visible.map(t => t.ID));
  const clearSel = () => setSelectedIds([]);
  // 筛选、翻页、换条数或换视图后清空勾选，否则批量操作会碰到看不见的行。
  useEffect(() => { setSelectedIds([]); }, [groupFilter, stateFilter, search, paged.page, paged.size, view]);

  // ── 写操作 ────────────────────────────────────────────────────────────────
  const openDetail = (t) => setDetailId(t.ID);
  const openEdit = (t) => setEditId(t.ID);
  const openAdd = () => { setForm({ name: "", mac: "", ip: "", group_id: groups[0]?.ID || "" }); setModal("add"); };
  const saveAdd = async () => {
    if (!form.mac?.trim()) { store.toast("请输入 MAC 地址", "err"); return; }
    if (!form.group_id) { store.toast("请选择所属分组", "err"); return; }
    const ok = await run(() => api.createTerminal({
      name: form.name?.trim() || "",
      mac: form.mac.trim(),
      ip: form.ip?.trim() || "",
      group_id: form.group_id,
      is_super: false,
      state: "offline",
    }), "终端已添加", { reload: load, errMsg: "保存失败" });
    if (ok) setModal(null);
  };
  // 取消超管会当场删掉超管盘。按钮和编辑表单里改回普通是同一件事。
  const confirmDropSuper = (t) => conf.ask({ title: "取消超管", danger: true, confirmText: "丢弃改动",
    message: `取消后，「${terminalTitle(t)}」超管盘上未保存的改动将全部丢弃，且无法恢复。\n如需保留，请改用「关机后存还原点」。` });
  const saveEdit = async (next) => {
    if (!next.mac?.trim()) { store.toast("请输入 MAC 地址", "err"); return; }
    if (!next.group_id) { store.toast("请选择所属分组", "err"); return; }
    const before = terminals.find(x => x.ID === next.id);
    if (before?.IsSuper && !next.is_super && !(await confirmDropSuper(before))) return;
    const ok = await run(() => api.updateTerminal(next.id, {
      name: next.name?.trim() || "",
      mac: next.mac.trim(),
      ip: next.ip?.trim() || "",
      group_id: next.group_id,
      is_super: !!next.is_super,
      state: next.state || "offline",
    }), "终端已更新", { reload: load, errMsg: "保存失败" });
    if (ok) setEditId(null);
  };

  const deleteOne = async (t) => {
    const ok = await conf.ask({ title: "删除终端", message: `确定删除「${terminalTitle(t)}」（${fmtMac(t.MAC)}）？其克隆盘将一并清理。`, danger: true });
    if (!ok) return;
    if (await run(() => api.deleteTerminal(t.ID), "终端已删除", { reload: load, errMsg: "删除失败" })) {
      if (detailId === t.ID) setDetailId(null);
      if (editId === t.ID) setEditId(null);
    }
  };
  const bulkDelete = async () => {
    const ok = await conf.ask({ title: "批量删除终端", message: `将删除 ${selectedIds.length} 台终端，无法恢复。`, danger: true, confirmText: "删除" });
    if (!ok) return;
    const errs = [];
    for (const id of selectedIds) { try { await api.deleteTerminal(id); } catch (e) { errs.push(id); } }
    store.toast(errs.length ? `完成，${errs.length} 台删除失败` : `已删除 ${selectedIds.length} 台`, errs.length ? "err" : "ok");
    clearSel(); await load();
  };
  const applyBulkGroup = async () => {
    if (!form.group_id) { store.toast("请选择目标分组", "err"); return; }
    if (await run(() => api.moveTerminals({ terminal_ids: selectedIds, group_id: form.group_id }),
      `已移动 ${selectedIds.length} 台终端`, { reload: load, errMsg: "移动失败" })) {
      setModal(null); clearSel();
    }
  };

  const setSuper = async (t, enable) => {
    if (!enable && !(await confirmDropSuper(t))) return;
    return run(() => (enable ? api.enableSuper(t.ID) : api.disableSuper(t.ID)),
      enable ? "已设为超管机" : "已取消超管", { reload: load });
  };
  // 发布一块数据盘：只要一个名字。说明放在弹窗里，别塞进按钮或错误里。
  const openPublish = (t, disk) => {
    setForm({ id: t.ID, name: t.Name || t.MAC, disk_id: disk.disk_id, mount: disk.mount_target, written: disk.written, publish_name: "" });
    setModal("publish"); setDetailId(null);
  };
  const doPublish = async () => {
    if (!form.publish_name?.trim()) { store.toast("请填写还原点名称", "err"); return; }
    if (await run(() => api.publishDataDisk(form.id, { disk_id: form.disk_id, name: form.publish_name.trim() }),
      "发布任务已提交，客户机下次开机生效", { reload: load, errMsg: "发布失败" })) setModal(null);
  };
  const openStopSuper = (t) => {
    // 系统盘默认保存；分组数据盘加载后逐个列出，需单独勾选并填还原点名。
    setForm({ id: t.ID, name: t.Name || t.MAC, save_system: true, reduction_name: "", disks: [], saves: {} });
    setModal("stopSuper"); setDetailId(null);
    api.listGroupDisks(t.GroupID)
      .then(r => setForm(f => (f.id === t.ID ? { ...f, disks: (r && r.items) || [] } : f)))
      .catch(() => {});
  };
  const doStopSuper = async () => {
    // 机器还在线就别提交，否则后端等满两分钟才失败。读当前列表而不是打开弹窗时的快照，
    // 填名字期间机器可能已经关了。
    const live = terminals.find(t => t.ID === form.id);
    if (live && live.State === "online") {
      store.toast(`${form.name || "该超管机"}运行中，请先关机再保存`, "err");
      return;
    }
    const saveSystem = form.save_system !== false;
    const systemName = (form.reduction_name || "").trim();
    if (saveSystem && !systemName) { store.toast("请输入系统盘还原点名称", "err"); return; }
    const dataDisks = [];
    for (const d of form.disks || []) {
      const save = form.saves?.[d.ID];
      if (!save?.on) continue;
      const name = (save.name || "").trim();
      if (!name) { store.toast(`请输入数据盘 ${d.MountTarget} 的还原点名称`, "err"); return; }
      dataDisks.push({ disk_id: d.ID, reduction_name: name });
    }
    if (!saveSystem && !dataDisks.length) { store.toast("至少勾选一块盘保存，或用「取消超管」丢弃改动", "err"); return; }
    const body = { reduction_name: saveSystem ? systemName : "" };
    if (dataDisks.length) body.data_disks = dataDisks;
    if (await run(() => api.stopSuper(form.id, body), "存还原点任务已提交", { reload: load })) setModal(null);
  };
  const setDiskSave = (id, patch) => setForm(f => ({ ...f, saves: { ...(f.saves || {}), [id]: { ...((f.saves || {})[id] || {}), ...patch } } }));

  const doExport = async () => {
    try { await downloadRaw(await api.exportTerminals(groupFilter === "all" ? "" : groupFilter), "terminals.xlsx"); }
    catch (e) { store.toast(e.message || "导出失败", "err"); }
  };
  const doTemplate = async () => {
    try { await downloadRaw(await api.terminalTemplate(), "terminals-template.xlsx"); }
    catch (e) { store.toast(e.message || "下载失败", "err"); }
  };
  const doImport = async (file) => {
    const fd = new FormData(); fd.append("file", file);
    try {
      const r = await api.importTerminals(fd);
      const errN = (r.errors || []).length;
      store.toast(`导入完成 · 新增 ${r.created || 0}${errN ? ` · ${errN} 行失败` : ""}`, errN ? "err" : "ok");
      await load();
    } catch (e) { store.toast(e.message || "导入失败", "err"); }
  };

  // 计数只放在筛选下拉里，不再另起一排看似可点的标签重复同样的数字。
  const stateOptions = [
    { id: "all", label: "全部", n: terminals.length },
    { id: "online", label: "在线", n: stateCount.online || 0 },
    { id: "offline", label: "离线", n: stateCount.offline || 0 },
  ];
  const filterActive = filtered.length !== terminals.length;

  return (
    <div className="fade-in">
      {selectedIds.length > 0 && (
        <div className="card" style={{ padding: 10, marginBottom: 14, display: "flex", alignItems: "center", gap: 10, background: "var(--cyan-soft)", borderColor: "oklch(0.55 0.1 200 / 0.4)" }}>
          <span className="chip cyan">已选 {selectedIds.length}</span>
          <button className="btn" onClick={() => { setForm({ group_id: groups[0]?.ID || "" }); setModal("bulk-group"); }}>切换分组</button>
          <button className="btn danger" onClick={bulkDelete}><Icons.Trash size={12}/> 删除</button>
          <button className="btn ghost" onClick={clearSel} style={{ marginLeft: "auto" }}>取消选择</button>
        </div>
      )}

      <div className="card" style={{ padding: 12, marginBottom: 14, display: "flex", flexDirection: "column", gap: 12 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
          <SegFilter value={stateFilter} onChange={setStateFilter} options={stateOptions}/>
          <select className="input" value={groupFilter} onChange={e => setGroupFilter(e.target.value)} style={{ width: 180 }}>
            <option value="all">全部分组</option>
            {groups.map(g => <option key={g.ID} value={g.ID}>{g.Name}</option>)}
          </select>
          <SearchBox value={search} onChange={setSearch} placeholder="搜索名称 / MAC / IP" width={260}/>
          {/* 只在有筛选时显示，否则没有新信息。 */}
          {filterActive && <span className="chip cyan">筛出 {filtered.length} / {terminals.length}</span>}
          <div style={{ marginLeft: "auto" }} className="seg">
            <button className={view === "grid" ? "on" : ""} onClick={() => setView("grid")}>矩阵</button>
            <button className={view === "table" ? "on" : ""} onClick={() => setView("table")}>列表</button>
          </div>
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", borderTop: "1px solid var(--line-soft)", paddingTop: 10 }}>
          {/* 已有 15 秒轮询，手动刷新只留图标按钮，免得让人以为不点就不更新。 */}
          <button className="btn ghost icon" onClick={() => load()} title="立即刷新" aria-label="立即刷新"><Icons.Refresh size={13}/></button>
          <span style={{ fontSize: 12, color: "var(--fg-faint)" }}>在线状态由 iSCSI 会话判定 · 每 15 秒自动刷新</span>
          <div style={{ flex: 1 }}/>
          <input ref={fileRef} type="file" accept=".xlsx" style={{ display: "none" }} onChange={e => { const f = e.target.files?.[0]; if (f) doImport(f); e.target.value = ""; }}/>
          <div className="btn-group">
            <button className="btn" onClick={() => fileRef.current?.click()}><Icons.ArrowDn size={12}/> 导入</button>
            <button className="btn" onClick={doExport}><Icons.ArrowUp size={12}/> 导出</button>
            <button className="btn" onClick={doTemplate} title="下载导入用的 xlsx 模板">模板</button>
          </div>
          <button className="btn primary" onClick={openAdd}><Icons.Plus size={12}/> 添加终端</button>
        </div>
      </div>

      {view === "grid" ? (
        <div className="card" style={{ padding: 16 }}>
            {groups.filter(g => groupFilter === "all" || g.ID === groupFilter).map(g => {
              const cells = filtered.filter(t => t.GroupID === g.ID);
              if (cells.length === 0) return null;
              return (
                <div key={g.ID} style={{ marginBottom: 22 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 10 }}>
                    {/* 不转大写：分组名是运维输入的数据，要原样显示。 */}
                    <span className="row" style={{ fontSize: 13, color: "var(--fg-mute)", fontWeight: 600, cursor: "pointer", gap: 2 }} onClick={() => goto("groups", { groupId: g.ID })} title="查看该分组">{g.Name}<Icons.Chevron size={11}/></span>
                    <span className="chip">{cells.filter(c => c.State === "online").length} / {cells.length} 在线</span>
                    <div style={{ flex: 1, height: 1, background: "var(--line-soft)" }}/>
                  </div>
                  <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(150px, 1fr))", gap: 8 }}>
                    {cells.map(t => {
                      const st = STATE[stateOf(t)];
                      return (
                        <div key={t.ID} className={`tmcell s-${t.State}`}
                             onClick={(e) => { if (e.shiftKey) toggleSel(t.ID); else openDetail(t); }}
                             style={{ borderColor: detailId === t.ID ? "var(--cyan)" : selectedIds.includes(t.ID) ? "var(--violet)" : undefined,
                                      boxShadow: (detailId === t.ID || selectedIds.includes(t.ID)) ? `0 0 0 1px ${selectedIds.includes(t.ID) ? "var(--violet)" : "var(--cyan)"}` : undefined }}>
                          <div className="tmh">
                            <span className={`dot ${st.dot}`}/>
                            <span className="tmname" title={terminalTitle(t)}>{terminalTitle(t)}</span>
                            {t.IsSuper && <span className="chip violet" style={{ padding: "0 6px" }} title="超管机">SU</span>}
                            {t.IsSuper && t.PendingBundleID && <span className="chip amber" style={{ padding: "0 5px" }} title={`已注入 ${bundleNameById[t.PendingBundleID] || t.PendingBundleID}，尚未固化`}><Icons.Clock size={10}/></span>}
                          </div>
                          <div className="tmip">{t.IP || "—"}</div>
                          <div className="tmfoot">
                            <span>{fmtMac(t.MAC).split(":").slice(-3).join(":")}</span>
                            <span className="tmstate">{st.t}</span>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </div>
              );
            })}
            {!filtered.length && <div style={{ fontSize: 13, color: "var(--fg-faint)", textAlign: "center", padding: 24 }}>{loading ? "加载中…" : error ? `加载失败：${error}` : "暂无终端"}</div>}
            <div className="hint" style={{ marginTop: 8 }}>提示：Shift + 点击进行多选 · 单击查看详情</div>
        </div>
      ) : (
        <div className="card" style={{ padding: 0, overflow: "hidden" }}>
          <table className="t">
            <thead><tr>
              <th style={{ width: 36 }}><input type="checkbox" checked={visible.length > 0 && visible.every(t => selectedIds.includes(t.ID))} onChange={e => e.target.checked ? selectAll() : clearSel()}/></th>
              <th>终端</th><th>MAC</th><th>IP</th><th>分组</th><th>系统镜像</th>{clustered && <th>节点</th>}<th>状态</th><th>最后心跳</th><th className="t-actions"></th>
            </tr></thead>
            <tbody>
              {visible.map(t => {
                const st = STATE[stateOf(t)];
                return (
                  <tr key={t.ID}>
                    <td><input type="checkbox" checked={selectedIds.includes(t.ID)} onChange={() => toggleSel(t.ID)} onClick={e => e.stopPropagation()}/></td>
                    {/* 不显示 t.ID：它就是 "terminal-" + 旁边一列的 MAC。 */}
                    <td onClick={() => openDetail(t)} style={{ cursor: "pointer" }} title="查看详情"><div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <span style={{ fontWeight: 600 }}>{terminalTitle(t)}</span>
                      {t.IsSuper && <span className="chip violet" style={{ padding: "0 6px" }}>超管</span>}
                      {t.IsSuper && t.PendingBundleID && <span className="chip amber" style={{ padding: "0 6px" }} title={`已注入 ${bundleNameById[t.PendingBundleID] || t.PendingBundleID}，尚未固化`}>驱动待固化</span>}
                    </div></td>
                    <td className="mono muted" style={{ fontSize: 12 }}>{fmtMac(t.MAC)}</td>
                    <td className="mono">{t.IP || "—"}</td>
                    <td className="muted" style={{ cursor: "pointer" }} onClick={() => goto("groups", { groupId: t.GroupID })} title="跳转到该分组">{groupById[t.GroupID]?.Name || "—"}</td>
                    <td className="muted">{imageNameOf(t.GroupID)}</td>
                    {clustered && <td className="mono muted" style={{ fontSize: 12 }}>{nodeLabel(t)}</td>}
                    {/* 状态只表示一次：圆点放在状态标签里，不在名称前重复。 */}
                    <td><span className={`chip ${st.c}`}><span className={`dot ${st.dot}`}/>{st.t}</span></td>
                    <td className="mono muted" style={{ fontSize: 12 }}>{fmtTime(t.LastHeartbeatAt)}</td>
                    <td className="t-actions"><div>
                      <button className="btn" style={{ padding: "3px 8px" }} onClick={() => openDetail(t)}>详情</button>
                      <button className="btn" style={{ padding: "3px 8px" }} onClick={() => openEdit(t)}>编辑</button>
                      <button className="btn ghost icon danger" onClick={() => deleteOne(t)} title="删除终端" aria-label={`删除终端 ${terminalTitle(t)}`}><Icons.Trash size={12}/></button>
                    </div></td>
                  </tr>
                );
              })}
              <TableState colSpan={clustered ? 10 : 9} loading={loading && !terminals.length} error={error}
                empty={!visible.length}
                hint={terminals.length ? "没有匹配的终端 · 调整筛选或搜索条件" : "暂无终端 · 点右上角「添加终端」登记 MAC"}/>
            </tbody>
          </table>
          <Pager {...paged.pager}/>
        </div>
      )}

      {detail && <TerminalDetail t={detail} group={groupById[detail.GroupID]} images={images} bundleNameById={bundleNameById}
        imageName={imageNameOf(detail.GroupID)} onClose={() => setDetailId(null)} onDelete={() => deleteOne(detail)}
        onSuper={(en) => setSuper(detail, en)} onStopSuper={() => openStopSuper(detail)} onPublish={(d) => openPublish(detail, d)}
        gotoDrivers={() => goto("drivers", { tab: "cure" })}/>}

      {editing && <TerminalEdit t={editing} groups={groups} busy={busy} onClose={() => setEditId(null)} onSave={saveEdit}/>}

      {/* 新增 / 编辑 */}
      <Modal open={modal === "add"} onClose={() => !busy && setModal(null)}
        title="添加终端"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={saveAdd}>{busy ? "保存中…" : "保存"}</button></>}>
        <Field label="终端名称"><input className="input" style={{ width: "100%" }} value={form.name || ""} onChange={e => setForm({ ...form, name: e.target.value })} placeholder="例如 前台-01"/></Field>
        <Field label="MAC 地址" required><input className="input mono" style={{ width: "100%" }} value={form.mac || ""} onChange={e => setForm({ ...form, mac: e.target.value })} placeholder="52:54:00:..."/></Field>
        <Field label="IP 地址" hint="留空由 DHCP 在分组网段内自动分配"><input className="input mono" style={{ width: "100%" }} value={form.ip || ""} onChange={e => setForm({ ...form, ip: e.target.value })} placeholder="192.168.10.x"/></Field>
        <Field label="所属分组" required>
          <Select value={form.group_id || ""} onChange={v => setForm({ ...form, group_id: v })} options={groups.length ? groups.map(g => ({ value: g.ID, label: g.Name })) : [{ value: "", label: "无可用分组" }]}/>
        </Field>
      </Modal>

      {/* 批量换组 */}
      <Modal open={modal === "bulk-group"} onClose={() => !busy && setModal(null)} title={`移动 ${selectedIds.length} 台终端到分组`}
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={applyBulkGroup}>{busy ? "移动中…" : "应用"}</button></>}>
        <Field label="目标分组" required hint="移动后按新分组的网络与系统镜像于下次开机生效">
          <Select value={form.group_id || ""} onChange={v => setForm({ ...form, group_id: v })} options={groups.map(g => ({ value: g.ID, label: g.Name }))}/>
        </Field>
      </Modal>

      {/* 数据盘在线发布 */}
      <Modal open={modal === "publish"} onClose={() => !busy && setModal(null)} title={`发布数据盘 ${form.mount || ""}`}
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={doPublish}>{busy ? "发布中…" : "发布"}</button></>}>
        <div className="hint" style={{ marginBottom: 10 }}>
          发布期间该磁盘短暂脱机，无需关机。已开机的客户机下次开机生效。
        </div>
        <Field label="还原点名称" required hint={form.written ? `未发布改动 ${fmtCap(form.written)}` : ""}>
          <input className="input" style={{ width: "100%" }} autoFocus placeholder="例如 2026Q3-游戏更新"
            value={form.publish_name || ""} onChange={e => setForm({ ...form, publish_name: e.target.value })}/>
        </Field>
      </Modal>

      {/* 停止超管并存还原点 */}
      <Modal open={modal === "stopSuper"} onClose={() => !busy && setModal(null)} title="超管停机 · 存还原点"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={doStopSuper}>{busy ? "提交中…" : "保存还原点"}</button></>}>
        {(terminals.find(t => t.ID === form.id) || {}).State === "online" && (
          <div className="hint" style={{ marginBottom: 10, color: "var(--amber)" }}>
            该终端运行中，请先关机再保存。
          </div>
        )}
        {/* 一行说会发生什么，一行说会失去什么。 */}
        <div className="hint" style={{ marginBottom: 4 }}>勾选的磁盘保存为还原点并设为当前点，客户机下次开机生效。保存后该终端仍为超管机。</div>
        <div className="hint" style={{ marginBottom: 10, color: "var(--amber)" }}>未勾选的磁盘将丢弃本次改动。</div>
        <div className="row" style={{ gap: 8, alignItems: "center", marginBottom: 8 }}>
          <label className="row" style={{ gap: 6, width: 150, fontSize: 13, cursor: "pointer" }}>
            <input type="checkbox" checked={form.save_system !== false} onChange={e => setForm({ ...form, save_system: e.target.checked })} aria-label="保存系统盘"/>
            系统盘
          </label>
          <input className="input" style={{ flex: 1 }} disabled={form.save_system === false} value={form.reduction_name || ""} onChange={e => setForm({ ...form, reduction_name: e.target.value })} placeholder="2026Q2-update" aria-label="系统盘还原点名称"/>
        </div>
        {(form.disks || []).map(d => {
          const save = form.saves?.[d.ID] || {};
          return (
            <div key={d.ID} className="row" style={{ gap: 8, alignItems: "center", marginBottom: 8 }}>
              <label className="row" style={{ gap: 6, width: 150, fontSize: 13, cursor: "pointer" }}>
                <input type="checkbox" checked={!!save.on} onChange={e => setDiskSave(d.ID, { on: e.target.checked })} aria-label={`保存数据盘 ${d.MountTarget}`}/>
                数据盘 {d.MountTarget}
                <span className="meta" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{images.find(i => i.ID === d.ImageID)?.Name || d.ImageID}</span>
              </label>
              <input className="input" style={{ flex: 1 }} disabled={!save.on} value={save.name || ""} onChange={e => setDiskSave(d.ID, { name: e.target.value })} placeholder="还原点名称" aria-label={`数据盘 ${d.MountTarget} 还原点名称`}/>
            </div>
          );
        })}
      </Modal>

      {conf.node}
    </div>
  );
}

// 详情和编辑是两个独立弹窗，互不切换，弹窗内容不会在用户眼前整体替换。
function TerminalModalTitle({ t }) {
  const st = STATE[stateOf(t)];
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
      <span className={`dot ${st.dot}`}/>
      <span>{terminalTitle(t)}</span>
      {/* 不显示 ID：t.ID 就是 "terminal-" + MAC，弹窗里已有 MAC。 */}
      {t.IsSuper && <span className="chip violet">超管机</span>}
    </div>
  );
}

function TerminalEdit({ t, groups, busy, onClose, onSave }) {
  const [form, setForm] = useState(terminalForm(t));
  // 只以 ID 为依赖：15 秒后台刷新不能冲掉正在编辑的内容。
  useEffect(() => { setForm(terminalForm(t)); }, [t.ID]);

  // 留空：同组保持原 IP，换组由后端在新组分配。原 IP 不在新组网段内就清空，回到原组再填回。
  const changeGroup = (groupID) => {
    let ip = form.ip;
    if (!ip?.trim() || ip === t.IP) {
      const g = groups.find(x => x.ID === groupID);
      ip = groupID === t.GroupID || (t.IP && ipInGroupPool(t.IP, g)) ? t.IP : "";
    }
    setForm({ ...form, group_id: groupID, ip });
  };
  const formGroup = groups.find(g => g.ID === form.group_id) || null;
  const ipOutOfPool = !!form.ip?.trim() && formGroup && !ipInGroupPool(form.ip, formGroup);
  const ipHint = ipOutOfPool ? (
    <span style={{ color: "var(--amber)" }}>
      不在「{formGroup.Name}」网段 {formGroup.StartIP} → {ipRangeEnd(formGroup.StartIP, formGroup.ClientMax)} 内，保存会被拒绝
    </span>
  ) : form.group_id !== t.GroupID
    ? "留空将在新分组网段内自动分配；填写时需落在该网段内"
    : "留空保持当前 IP；填写时需落在所属分组网段内";

  return (
    <Modal open onClose={() => !busy && onClose()} size="lg" title={<TerminalModalTitle t={t}/>}
      footer={<>
        <button className="btn" disabled={busy} onClick={onClose}>取消</button>
        <button className="btn primary" disabled={busy} onClick={() => onSave(form)}>{busy ? "保存中…" : "保存"}</button>
      </>}>
      <Field label="终端名称"><input className="input" style={{ width: "100%" }} value={form.name || ""} onChange={e => setForm({ ...form, name: e.target.value })} placeholder="例如 前台-01"/></Field>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: 12 }}>
        <Field label="MAC 地址" required><input className="input mono" style={{ width: "100%" }} value={form.mac || ""} onChange={e => setForm({ ...form, mac: e.target.value })}/></Field>
        <Field label="IP 地址" hint={ipHint}><input className="input mono" style={{ width: "100%" }} value={form.ip || ""} onChange={e => setForm({ ...form, ip: e.target.value })} placeholder={t.IP || "192.168.10.x"}/></Field>
      </div>
      <Field label="所属分组" required>
        <Select value={form.group_id || ""} onChange={changeGroup} options={groups.map(g => ({ value: g.ID, label: g.Name }))}/>
      </Field>
      <Field label="终端类型" helpInline
        help="超管机用于修改镜像内容，保存的还原点对同组客户机下次开机生效"
        hint={t.State === "online" ? "终端运行中，关机后可切换" : "同一配置同时只能有一台超管机"}>
        <div className="seg" style={{ width: "fit-content" }}>
          <button type="button" disabled={t.State === "online"} className={!form.is_super ? "on" : ""} onClick={() => setForm({ ...form, is_super: false })}>普通终端</button>
          <button type="button" disabled={t.State === "online"} className={form.is_super ? "on" : ""} onClick={() => setForm({ ...form, is_super: true })}>超管机</button>
        </div>
      </Field>
    </Modal>
  );
}

function TerminalDetail({ t, group, images, bundleNameById, imageName, onClose, onDelete, onSuper, onStopSuper, onPublish, gotoDrivers }) {
  const st = STATE[stateOf(t)];
  const { data: diskData, loading: disksLoading } = useResource(
    () => (group?.ID ? api.listGroupDisks(group.ID) : Promise.resolve({ items: [] })),
    [group?.ID],
  );
  const disks = (diskData && diskData.items) || [];
  // 超管机的数据盘要显示：当前发布点、还有多少改动没发布。
  const { data: superDiskData } = useResource(
    () => (t.IsSuper ? api.superDisks(t.ID) : Promise.resolve({ items: [] })),
    [t.IsSuper, t.ID],
  );
  const superDisks = (superDiskData && superDiskData.items) || [];
  const imageNameById = (id) => images.find(i => i.ID === id)?.Name || id || "—";
  // 配置、还原点原始 ID 带纳秒后缀，换成名称显示，查不到就显示 ID。
  const { data: nameData } = useResource(async () => {
    if (!group?.SystemImageID) return null;
    const [cfgs, reds] = await Promise.allSettled([
      api.listConfigs(group.SystemImageID),
      group.SystemConfigID ? api.listReductions(group.SystemConfigID) : Promise.resolve(null),
    ]);
    const items = (r) => (r.status === "fulfilled" && r.value && r.value.items) || [];
    return {
      configs: Object.fromEntries(items(cfgs).map(c => [c.ID, c.Name])),
      reductions: Object.fromEntries(items(reds).map(rp => [rp.ID, redName(rp)])),
    };
  }, [group?.SystemImageID, group?.SystemConfigID]);
  const running = t.State === "online";
  const configLabel = (nameData?.configs || {})[group?.SystemConfigID] || group?.SystemConfigID || "—";
  const reductionLabel = (nameData?.reductions || {})[group?.SystemReductionID] || group?.SystemReductionID || "—";

  return (
    <Modal open onClose={onClose} size="xl" title={<TerminalModalTitle t={t}/>}
      footer={<>
        {/* 机器在跑的时候这些都做不了，直接禁用并说明什么时候能做。 */}
        {running && <span className="hint" style={{ marginRight: "auto" }}>{t.IsSuper ? "终端运行中，关机后可取消超管或保存还原点" : "终端运行中，关机后可设为超管"}</span>}
        {t.IsSuper
          ? <>
              <button className="btn" disabled={running} onClick={() => onSuper(false)}>取消超管</button>
              <button className="btn primary" disabled={running} onClick={onStopSuper}><Icons.Snapshot size={12}/> 关机后存还原点</button>
            </>
          : <button className="btn" disabled={running} onClick={() => onSuper(true)}>设为超管</button>}
        <button className="btn danger" onClick={onDelete}>删除</button>
      </>}>
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {t.IsSuper && t.PendingBundleID && (
            <div style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, color: "var(--fg-mute)" }}>
              <span className="chip amber"><Icons.Clock size={11}/> 驱动固化会话进行中</span>
              <span>已注入「{bundleNameById[t.PendingBundleID] || t.PendingBundleID}」，尚未固化</span>
              <span className="row" style={{ color: "var(--cyan)", cursor: "pointer", gap: 2 }} onClick={gotoDrivers}>去驱动管理处理<Icons.Chevron size={11}/></span>
            </div>
          )}
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(240px, 1fr))", gap: 14 }}>
            <DetailSection title="基础信息" icon={<Icons.Monitor size={12}/>}>
              <DetailKV k="终端名称" v={terminalTitle(t)}/>
              <DetailKV k="MAC 地址" v={fmtMac(t.MAC)}/>
              <DetailKV k="IP 地址" v={t.IP || "—"}/>
              <DetailKV k="状态" v={st.t}/>
              <DetailKV k="最后心跳" v={fmtTime(t.LastHeartbeatAt)}/>
            </DetailSection>
            <DetailSection title="分组网络" icon={<Icons.Network size={12}/>}>
              <DetailKV k="所属分组" v={group?.Name || "—"}/>
              <DetailKV k="IP 范围" v={group ? `${group.StartIP} → ${ipRangeEnd(group.StartIP, group.ClientMax)}` : "—"}/>
              <DetailKV k="网关" v={group?.Gateway || "—"}/>
              <DetailKV k="子网掩码" v={group?.Netmask || "—"}/>
              <DetailKV k="DNS" v={group ? [group.DNS1, group.DNS2].filter(Boolean).join(" / ") || "—" : "—"}/>
            </DetailSection>
          </div>
          <DetailSection title="系统盘" icon={<Icons.Layers size={12}/>}>
            <DetailKV k="系统镜像" v={imageName || "—"}/>
            <DetailKV k="启动配置" v={configLabel}/>
            <DetailKV k="默认还原点" v={reductionLabel}/>
          </DetailSection>
          <DetailSection title="数据盘" icon={<Icons.Disk size={12}/>}>
            {t.IsSuper && <div className="hint" style={{ marginBottom: 8 }}>数据盘可在线发布，无需关机。发布期间该磁盘短暂脱机，客户机下次开机生效。</div>}
            <div style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 6, overflow: "hidden" }}>
              <table className="t" style={{ margin: 0 }}>
                <thead><tr><th>挂载目标</th>{t.IsSuper ? <><th>用途</th><th>当前发布点</th><th>未发布改动</th><th/></> : <><th>镜像</th><th>配置</th></>}</tr></thead>
                <tbody>
                  {t.IsSuper ? superDisks.map(d => {
                    // 不能发布时说清是哪种情况，而不是只给个灰按钮。
                    const why = !d.ready ? "开机后可发布" : (d.written > 0 ? null : "无改动");
                    return (
                      <tr key={d.disk_id}>
                        <td className="mono" style={{ fontWeight: 600 }}>{d.mount_target}</td>
                        <td>{d.config_name || "—"}</td>
                        <td className="muted">{d.current_name || "—"}</td>
                        <td className={why ? "muted" : "mono"}>{why || fmtCap(d.written)}</td>
                        <td style={{ textAlign: "right" }}>
                          <button className="btn" style={{ padding: "3px 8px" }} disabled={!!why} onClick={() => onPublish(d)}>保存并发布</button>
                        </td>
                      </tr>
                    );
                  }) : disks.map(d => (
                    <tr key={d.ID}>
                      <td className="mono" style={{ fontWeight: 600 }}>{d.MountTarget}</td>
                      <td className="muted">{imageNameById(d.ImageID)}</td>
                      <td className="mono muted" style={{ fontSize: 12 }}>{d.ConfigID}</td>
                    </tr>
                  ))}
                  {!(t.IsSuper ? superDisks : disks).length && <tr><td colSpan={5} style={{ textAlign: "center", padding: 14, color: "var(--fg-faint)", fontSize: 12 }}>{disksLoading ? "加载中…" : "暂无数据盘"}</td></tr>}
                </tbody>
              </table>
            </div>
          </DetailSection>
        </div>
    </Modal>
  );
}

function DetailSection({ title, icon, children }) {
  return (
    <div style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 8, padding: 12 }}>
      <div className="card-title-zh" style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>{icon}{title}</div>
      {children}
    </div>
  );
}

function DetailKV({ k, v }) {
  return (
    <div style={{ display: "flex", justifyContent: "space-between", padding: "7px 0", borderBottom: "1px solid var(--line-soft)", fontSize: 13 }}>
      <span style={{ color: "var(--fg-faint)" }}>{k}</span>
      <span className="mono" style={{ fontWeight: 500 }}>{v}</span>
    </div>
  );
}


window.PageTerminals = PageTerminals;

export { PageTerminals };
