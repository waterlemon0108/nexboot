// 告警由后端巡检按 key 去重、条件消失时自动恢复。「确认」只静音活动告警，仍会自动恢复；删除清的是历史。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { useConfirm } from '../overlay.jsx';
import { Select, TableState, SegFilter, StatCard, Pager } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation, usePolling, usePageState } from '../../lib/hooks.js';
import { fmtDateTimeSec as fmtDateTime } from '../../lib/format.js';
const { useState, useEffect } = React;

const SEV = {
  error: { t: "错误", c: "rose", dot: "err" },
  warn:  { t: "警告", c: "amber", dot: "warn" },
  info:  { t: "信息", c: "cyan", dot: "cyan" },
};
const ST = {
  active:       { t: "活动", c: "rose" },
  acknowledged: { t: "已确认", c: "cyan" },
  recovered:    { t: "已恢复", c: "emerald" },
};

function PageAlarms() {
  const store = useStore();
  const conf = useConfirm();
  const [statusF, setStatusFState] = useState("");
  const [sevF, setSevFState] = useState("");
  const { page, size, setPage, pager } = usePageState();
  const setStatusF = (v) => { setStatusFState(v); setPage(1); };
  const setSevF = (v) => { setSevFState(v); setPage(1); };

  const { data, loading, error, reload: load } = useResource(
    () => api.listAlarms({ status: statusF, severity: sevF, page, size }), [statusF, sevF, page, size]);
  const items = (data && data.items) || [];
  const total = (data && data.total) || 0;
  const summary = (data && data.summary) || { active: 0, error: 0, warn: 0, info: 0 };
  const { run } = useMutation(store.toast);

  // 告警由后端巡检改写，需定时刷新。
  usePolling(load, { intervalMs: 15000 });

  const ack = (a) => run(() => api.ackAlarm(a.ID), "告警已确认", { reload: load });
  // 勾选只作用于当前页：翻页、换条数或筛选后清空，免得删掉看不见的行。
  const [selected, setSelected] = useState([]);
  useEffect(() => { setSelected([]); }, [page, size, statusF, sevF]);
  const toggle = (id) => setSelected(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  const allOnPage = items.length > 0 && items.every(a => selected.includes(a.ID));
  const delSelected = async () => {
    const ok = await conf.ask({ title: "删除告警", message: `删除 ${selected.length} 条告警？删除后无法恢复。`, danger: true, confirmText: "删除" });
    if (!ok) return;
    let failed = 0;
    for (const id of selected) { try { await api.deleteAlarm(id); } catch { failed++; } }
    store.toast(failed ? `${failed} 条删除失败` : `已删除 ${selected.length} 条告警`, failed ? "err" : "ok");
    setSelected([]);
    load();
  };
  // 不限当前页：反复取活动告警，确认过的会移出活动列表，取空为止。
  const ackAll = async () => {
    let acked = 0;
    await run(async () => {
      for (let round = 0; round < 20; round++) {
        const res = await api.listAlarms({ status: "active", page: 1, size: 200 });
        const active = (res && res.items) || [];
        const before = acked;
        for (const a of active) { try { await api.ackAlarm(a.ID); acked++; } catch { /* ignore */ } }
        if (!active.length || acked === before) break;
      }
    }, null, { reload: load });
    if (acked) store.toast(`已确认 ${acked} 条告警`, "ok");
  };

  const statusFilters = [
    { id: "", label: "全部" },
    { id: "active", label: "活动", n: summary.active, color: "rose" },
    { id: "acknowledged", label: "已确认" },
    { id: "recovered", label: "已恢复" },
  ];

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(170px, 1fr))", gap: 12 }}>
        <StatCard label="活动告警" value={summary.active} accent="rose"/>
        <StatCard label="错误" value={summary.error} accent="rose"/>
        <StatCard label="警告" value={summary.warn} accent="amber"/>
        <StatCard label="信息" value={summary.info} accent="cyan"/>
      </div>

      <div className="card" style={{ padding: 0 }}>
        <div className="card-h">
          <SegFilter value={statusF} onChange={setStatusF} options={statusFilters}/>
          <div className="row" style={{ gap: 8 }}>
            <Select value={sevF} onChange={setSevF} options={[{ value: "", label: "全部级别" }, { value: "error", label: "错误" }, { value: "warn", label: "警告" }, { value: "info", label: "信息" }]} style={{ width: 110 }}/>
            <button className="btn" disabled={!summary.active} onClick={ackAll}><Icons.Check size={12}/> 全部确认</button>
            <button className="btn danger" disabled={!selected.length} onClick={delSelected}><Icons.Trash size={12}/> 删除所选{selected.length ? ` (${selected.length})` : ""}</button>
            <button className="btn ghost icon" onClick={() => load()}><Icons.Refresh size={14}/></button>
          </div>
        </div>
        <div style={{ overflowX: "auto" }}><table className="t">
          <thead><tr><th style={{ width: 36 }}><input type="checkbox" aria-label="选择本页全部告警" checked={allOnPage} onChange={e => setSelected(e.target.checked ? items.map(a => a.ID) : [])}/></th><th>级别</th><th>类型</th><th>资源</th><th>消息</th><th>时间</th><th>状态</th><th className="t-actions"></th></tr></thead>
          <tbody>
            {items.map(a => {
              const sev = SEV[a.Severity] || SEV.info;
              const st = ST[a.Status] || ST.active;
              return (
                <tr key={a.ID}>
                  <td><input type="checkbox" aria-label={`选择告警 ${a.Resource || a.Type}`} checked={selected.includes(a.ID)} onChange={() => toggle(a.ID)}/></td>
                  <td><span className={`chip ${sev.c}`}><span className={`dot ${sev.dot}`}/>{sev.t}</span></td>
                  <td style={{ whiteSpace: "nowrap" }}>{a.Type}</td>
                  <td className="mono ellip" style={{ fontSize: 12, maxWidth: 130 }} title={a.Resource || ""}>{a.Resource || "—"}</td>
                  {/* 消息可换行、最多两行：窄时靠换行收缩，不把表格撑出卡片 */}
                  <td className="muted" style={{ fontSize: 12, minWidth: 160 }} title={a.Message || ""}>
                    <div style={{ display: "-webkit-box", WebkitLineClamp: 2, WebkitBoxOrient: "vertical", overflow: "hidden" }}>{a.Message || "—"}</div>
                  </td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{fmtDateTime(a.UpdatedAt || a.CreatedAt)}</td>
                  <td><span className={`chip ${st.c}`}>{a.Status !== "active" && <Icons.Check size={10}/>}{st.t}</span></td>
                  <td className="t-actions"><div>
                    {a.Status === "active" && <button className="btn" style={{ padding: "3px 8px" }} onClick={() => ack(a)}>确认</button>}
                  </div></td>
                </tr>
              );
            })}
            <TableState colSpan={8} loading={loading} error={error} empty={!items.length} hint="暂无告警 · 系统健康"/>
          </tbody>
        </table></div>
        <Pager {...pager(total)}/>
      </div>
      {conf.node}
    </div>
  );
}

window.PageAlarms = PageAlarms;

export { PageAlarms };
