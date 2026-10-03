// 后端没有取消、重试、删除任务的接口，所以行内不提供这些操作。
import React from 'react';
import { Icons } from '../icons.jsx';
import { api } from '../../lib/api.js';
import { useResource, usePolling, usePageState } from '../../lib/hooks.js';
import { TableState, SegFilter, Pager } from '../primitives.jsx';
import { TASK_LABELS, fmtDateTimeSec as fmtDateTime, pad2 } from '../../lib/format.js';
const { useState } = React;

const STATUS = {
  pending: { t: "等待中", c: "" },
  running: { t: "执行中", c: "cyan" },
  success: { t: "成功", c: "emerald" },
  failed:  { t: "失败", c: "rose" },
};
const duration = (t) => {
  if (!t.FinishedAt || !t.CreatedAt) return "—";
  const ms = new Date(t.FinishedAt) - new Date(t.CreatedAt);
  if (isNaN(ms) || ms < 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  const s = Math.round(ms / 1000);
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${pad2(s % 60)}s`;
};

// shortTaskID 去掉 "task-<类型>-" 前缀，只留那串纳秒时间戳。
const shortTaskID = (id) => String(id || "").replace(/^task-[a-z_]+-/, "");

function PageTasks() {
  const { page, size, setPage, setSize, pager } = usePageState();
  const [status, setStatus] = useState("");
  const [expanded, setExpanded] = useState(null);

  const { data, loading, error, reload } = useResource(
    () => api.listTaskHistory({ page, size, status }), [page, size, status]);
  const tasks = (data && data.items) || [];
  const total = (data && data.total) || 0;
  const unreachable = (data && data.unreachable) || [];

  // 尽力查出客户机、镜像名称给「对象」列用，查不到就显示原始 MAC 或 ID。
  const { data: refData } = useResource(async () => {
    const [tr, ir] = await Promise.allSettled([api.listTerminals(), api.listImages()]);
    const items = (r) => (r.status === "fulfilled" && r.value && r.value.items) || [];
    const byMac = {};
    for (const t of items(tr)) if (t.MAC && t.Name) byMac[t.MAC] = t.Name;
    const byImage = {};
    for (const im of items(ir)) if (im.Name && im.Name !== im.ID) byImage[im.ID] = im.Name;
    return { byMac, byImage };
  }, []);
  const refLabel = (ref) => {
    if (!ref) return "";
    // 池的 ID 是「pool-<节点>--<池名>」，服务器另有一列，这里只留池名
    const pool = /^pool-[0-9a-f]+--(.+)$/.exec(ref);
    if (pool) return pool[1];
    const norm = String(ref).replace(/[:\-.\s]/g, "").toUpperCase();
    if (refData?.byMac?.[norm]) return `${refData.byMac[norm]} · ${ref}`;
    if (refData?.byImage?.[ref]) return `${refData.byImage[ref]} · ${ref}`;
    return ref;
  };

  // 本页还有未结束的任务时自动刷新。
  const hasInflight = tasks.some(t => t.Status === "running" || t.Status === "pending");
  usePolling(reload, { intervalMs: 3000, active: hasInflight });

  const setFilter = (s) => { setStatus(s); setPage(1); setExpanded(null); };
  const goPage = (p) => { setPage(p); setExpanded(null); };

  const filters = [
    { id: "", label: "全部" },
    { id: "running", label: "执行中", color: "cyan" },
    { id: "pending", label: "等待中" },
    { id: "success", label: "成功", color: "emerald" },
    { id: "failed", label: "失败", color: "rose" },
  ];

  return (
    <div className="fade-in card" style={{ padding: 0 }}>
      <div className="card-h">
        <SegFilter value={status} onChange={setFilter} options={filters}/>
        <div className="row" style={{ gap: 8 }}>
          {unreachable.length > 0 && <span className="meta" style={{ color: "var(--amber)" }}>{unreachable.join("、")} 读不到，它上面的任务没有列出</span>}
          <span className="meta">共 {total} 条{hasInflight ? " · 3 秒自动刷新" : ""}</span>
          <button className="btn ghost icon" onClick={() => reload()} title="立即刷新" aria-label="立即刷新"><Icons.Refresh size={13}/></button>
        </div>
      </div>
      <table className="t">
        <thead><tr><th>任务ID</th><th>类型</th><th>对象</th><th>服务器</th><th>进度</th><th>状态</th><th>创建时间</th><th>耗时</th></tr></thead>
        <tbody>
          {tasks.map(t => {
            const st = STATUS[t.Status] || { t: t.Status, c: "" };
            const isOpen = expanded === t.ID;
            return (
              <React.Fragment key={t.ID}>
                <tr style={{ cursor: "pointer" }} onClick={() => setExpanded(isOpen ? null : t.ID)}>
                  {/* 类型自己有一列，ID 不必再带前缀；完整 ID 在 title 里。 */}
                  <td className="mono" style={{ fontSize: 12 }} title={t.ID}>{shortTaskID(t.ID)}</td>
                  <td style={{ whiteSpace: "nowrap" }}>{TASK_LABELS[t.Type] || t.Type}</td>
                  <td className="muted ellip" style={{ maxWidth: 220 }} title={t.TargetRef || ""}>{t.TargetName || refLabel(t.TargetRef) || t.Message || "—"}</td>
                  <td className="mono muted" style={{ whiteSpace: "nowrap" }}>{t.Node || "—"}</td>
                  <td style={{ width: 340 }}>
                    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <div className={`bar ${t.Status === "success" ? "emerald" : t.Status === "failed" ? "rose" : ""}`} style={{ flex: 1 }}>
                        <span style={{ width: `${t.Status === "success" ? 100 : t.Progress || 0}%` }}/>
                      </div>
                      <span className="mono" style={{ fontSize: 12, width: 36, textAlign: "right" }}>{t.Status === "success" ? 100 : t.Progress || 0}%</span>
                    </div>
                    {t.Status === "running" && t.Message && (
                      <div className="meta ellip" style={{ marginTop: 3, maxWidth: 220 }}>{t.Message}</div>
                    )}
                    {/* 失败第一眼要看到的是「为什么」，不必点开。 */}
                    {t.Status === "failed" && t.Error && (
                      <div className="meta ellip" style={{ marginTop: 3, maxWidth: 340, color: "var(--rose)" }} title={t.Error}>{t.Error}</div>
                    )}
                  </td>
                  <td><span className={`chip ${st.c}`}>{t.Status === "running" && <span className="dot cyan"/>}{st.t}</span></td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{fmtDateTime(t.CreatedAt)}</td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{duration(t)}</td>
                </tr>
                {isOpen && (
                  <tr>
                    <td colSpan={8} style={{ background: "var(--bg-0)", padding: "10px 16px" }}>
                      <div style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "4px 16px", fontSize: 12 }}>
                        <span style={{ color: "var(--fg-faint)" }}>消息</span><span>{t.Message || "—"}</span>
                        <span style={{ color: "var(--fg-faint)" }}>结果</span><span className="mono">{t.Result || "—"}</span>
                        <span style={{ color: "var(--fg-faint)" }}>结束时间</span><span className="mono">{fmtDateTime(t.FinishedAt)}</span>
                        {t.Error && <><span style={{ color: "var(--rose)" }}>错误</span><span style={{ color: "var(--rose)" }}>{t.Error}</span></>}
                      </div>
                    </td>
                  </tr>
                )}
              </React.Fragment>
            );
          })}
          <TableState colSpan={8} loading={loading} error={error} empty={!tasks.length} hint="暂无任务"/>
        </tbody>
      </table>
      <Pager {...pager(total)} onPage={goPage} onSize={(n) => { setSize(n); setExpanded(null); }}/>
    </div>
  );
}

window.PageTasks = PageTasks;

export { PageTasks };
