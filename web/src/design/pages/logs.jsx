// 操作日志由审计中间件自动记录，登录日志（含失败）由登录处理器记录。
import React from 'react';
import { Icons } from '../icons.jsx';
import { Select, SearchBox, TableState, Pager } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, usePageState } from '../../lib/hooks.js';
import { fmtDateTimeSec as fmtDateTime } from '../../lib/format.js';
const { useState } = React;

const MODULES = ["镜像", "终端", "分组", "存储", "用户", "服务器", "备份", "驱动", "适配", "日志", "系统"];
const fmtCost = (ms) => {
  ms = Number(ms || 0);
  if (ms <= 0) return "—";
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`;
};


function PageLogs() {
  const [tab, setTab] = useState("operation");
  const [q, setQ] = useState("");
  const [appliedQ, setAppliedQ] = useState("");
  const [moduleF, setModuleF] = useState("");
  const [statusF, setStatusF] = useState("");
  const { page, size, setPage, pager } = usePageState();

  const { data, loading, error, reload: load } = useResource(() => api.listLogs({
    type: tab,
    q: appliedQ,
    module: tab === "operation" ? moduleF : "",
    status: statusF,
    page, size,
  }), [tab, appliedQ, moduleF, statusF, page, size]);
  const items = (data && data.items) || [];
  const total = (data && data.total) || 0;

  const apply = () => { setPage(1); if (appliedQ === q) load(); else setAppliedQ(q); };
  const switchTab = (t) => { setTab(t); setModuleF(""); setPage(1); };
  const isOp = tab === "operation";

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div className="tabs">
        <div className={`tab ${isOp ? "active" : ""}`} onClick={() => switchTab("operation")}>操作日志</div>
        <div className={`tab ${!isOp ? "active" : ""}`} onClick={() => switchTab("login")}>登录日志</div>
        <div style={{ flex: 1 }}/>
      </div>

      <div className="card" style={{ padding: "10px 14px", display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <SearchBox value={q} onChange={setQ} onSubmit={apply} placeholder={isOp ? "用户 / 操作 / 详情" : "用户 / 消息"}/>
        {isOp && <Select value={moduleF} onChange={v => { setModuleF(v); setPage(1); }} options={[{ value: "", label: "全部模块" }, ...MODULES.map(m => ({ value: m, label: m }))]} style={{ width: 130 }}/>}
        <Select value={statusF} onChange={v => { setStatusF(v); setPage(1); }} options={[{ value: "", label: "全部状态" }, { value: "ok", label: "成功" }, { value: "err", label: "失败" }]} style={{ width: 110 }}/>
        <button className="btn" onClick={() => apply()}><Icons.Search size={12}/> 查询</button>
        <div style={{ flex: 1 }}/>
        <span className="meta">共 {total} 条</span>
        <button className="btn ghost icon" onClick={() => load()}><Icons.Refresh size={14}/></button>
      </div>

      <div className="card" style={{ padding: 0 }}>
        <table className="t">
          {isOp ? (
            <thead><tr><th>时间</th><th>用户</th><th>模块</th><th>操作</th><th>详情</th><th>IP</th><th>耗时</th><th>状态</th></tr></thead>
          ) : (
            <thead><tr><th>时间</th><th>用户</th><th>IP</th><th>客户端</th><th>结果</th><th>状态</th></tr></thead>
          )}
          <tbody>
            {items.map(l => isOp ? (
              <tr key={l.ID}>
                <td className="mono muted" style={{ fontSize: 12 }}>{fmtDateTime(l.CreatedAt)}</td>
                <td><div className="row" style={{ gap: 6 }}><div style={{ width: 22, height: 22, borderRadius: "50%", background: "var(--bg-3)", display: "grid", placeItems: "center", fontSize: 11, fontWeight: 600 }}>{(l.Username || "?").charAt(0).toUpperCase()}</div>{l.Username || "—"}</div></td>
                <td><span className="chip">{l.Module || "—"}</span></td>
                <td>{l.Action || "—"}</td>
                <td className="mono muted ellip" style={{ fontSize: 12, maxWidth: 280 }} title={l.Detail || ""}>{l.Detail || "—"}</td>
                <td className="mono muted" style={{ fontSize: 12 }}>{l.IP || "—"}</td>
                <td className="mono muted" style={{ fontSize: 12 }}>{fmtCost(l.CostMs)}</td>
                <td>{l.Status === "ok" ? <span className="chip emerald">成功</span> : <span className="chip rose">失败{l.HTTPStatus ? ` · ${l.HTTPStatus}` : ""}</span>}</td>
              </tr>
            ) : (
              <tr key={l.ID}>
                <td className="mono muted" style={{ fontSize: 12 }}>{fmtDateTime(l.CreatedAt)}</td>
                <td><div className="row" style={{ gap: 6 }}><div style={{ width: 22, height: 22, borderRadius: "50%", background: "var(--bg-3)", display: "grid", placeItems: "center", fontSize: 11, fontWeight: 600 }}>{(l.Username || "?").charAt(0).toUpperCase()}</div>{l.Username || "—"}</div></td>
                <td className="mono">{l.IP || "—"}</td>
                <td className="muted ellip" style={{ fontSize: 12, maxWidth: 320 }} title={l.UserAgent || ""}>{l.UserAgent || "—"}</td>
                <td>{l.Action || "—"}</td>
                <td>{l.Status === "ok" ? <span className="chip emerald">成功</span> : <span className="chip rose">失败</span>}</td>
              </tr>
            ))}
            <TableState colSpan={isOp ? 8 : 6} loading={loading} error={error} empty={!items.length} hint="暂无日志"/>
          </tbody>
        </table>
        <Pager {...pager(total)}/>
      </div>
    </div>
  );
}

window.PageLogs = PageLogs;

export { PageLogs };
