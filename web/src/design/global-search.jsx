// 全局 ⌘K 命令面板 + 通知中心弹层
import React from 'react';
import { Icons } from './icons.jsx';
import { useStore } from './store.jsx';
import { Overlay } from './overlay.jsx';

function GlobalSearch({ open, onClose }) {
  const store = useStore();
  const [q, setQ] = React.useState("");
  const inputRef = React.useRef(null);
  const [activeIdx, setActiveIdx] = React.useState(0);

  React.useEffect(() => { if (open && inputRef.current) inputRef.current.focus(); }, [open]);
  React.useEffect(() => { setActiveIdx(0); }, [q]);

  if (!open) return null;

  const ql = q.toLowerCase();
  const results = q ? [
    ...store.terminals.filter(t => t.id.toLowerCase().includes(ql) || t.ip.includes(ql)).slice(0,4).map(t => ({ kind: "客户机", icon: <Icons.Monitor size={14}/>, color: "violet", title: t.id, sub: `${t.ip} · ${t.image}`, action: () => store.goto("terminals") })),
    ...store.images.filter(i => i.name.toLowerCase().includes(ql)).slice(0,3).map(i => ({ kind: "镜像", icon: <Icons.Layers size={14}/>, color: "emerald", title: i.name, sub: `${i.id} · ${i.size} GiB`, action: () => store.goto("images", { imageId: i.id }) })),
    ...store.groups.filter(g => g.name.toLowerCase().includes(ql)).slice(0,3).map(g => ({ kind: "分组", icon: <Icons.Group size={14}/>, color: "amber", title: g.name, sub: `${g.online}/${g.total} 在线 · ${g.startIp}`, action: () => store.goto("groups") })),
    ...store.alarms.filter(a => a.type.includes(q) || a.resource.includes(q)).slice(0,3).map(a => ({ kind: "告警", icon: <Icons.Bell size={14}/>, color: "rose", title: `${a.type} · ${a.resource}`, sub: `${a.value} · ${a.time}`, action: () => store.goto("alarms") })),
  ] : [
    ...store.alarms.filter(a => a.status === "active").slice(0,3).map(a => ({ kind: "未恢复告警", icon: <Icons.Bell size={14}/>, color: "rose", title: `${a.type} · ${a.resource}`, sub: a.time, action: () => store.goto("alarms") })),
    { kind: "快捷", icon: <Icons.Server size={14}/>, color: "cyan", title: "服务器集群", sub: "⌥1", action: () => store.goto("servers") },
    { kind: "快捷", icon: <Icons.Monitor size={14}/>, color: "violet", title: "客户机矩阵", sub: "⌥2", action: () => store.goto("terminals") },
    { kind: "快捷", icon: <Icons.Layers size={14}/>, color: "emerald", title: "镜像", sub: "⌥3", action: () => store.goto("images") },
    { kind: "快捷", icon: <Icons.Database size={14}/>, color: "amber", title: "存储池", sub: "⌥4", action: () => store.goto("storage") },
  ];

  const onKey = (e) => {
    if (e.key === "Escape") onClose();
    if (e.key === "ArrowDown") { e.preventDefault(); setActiveIdx(i => Math.min(i+1, results.length-1)); }
    if (e.key === "ArrowUp")   { e.preventDefault(); setActiveIdx(i => Math.max(i-1, 0)); }
    if (e.key === "Enter") {
      const r = results[activeIdx];
      if (r) { r.action(); onClose(); }
    }
  };

  return (
    <Overlay open onClose={onClose} placement="top" width={620} className="card">
        <div style={{ padding: 14, borderBottom: "1px solid var(--line-soft)", display: "flex", alignItems: "center", gap: 10 }}>
          <Icons.Search size={16} style={{ color: "var(--fg-faint)" }}/>
          <input ref={inputRef} value={q} onChange={e=>setQ(e.target.value)} onKeyDown={onKey} placeholder="搜索服务器 / 客户机 / 镜像 / 分组 / 告警..." style={{ flex: 1, background: "transparent", border: "none", outline: "none", color: "var(--fg)", fontSize: 14, fontFamily: "inherit" }}/>
          <kbd className="mono" style={{ fontSize: 11, padding: "2px 6px", border: "1px solid var(--line)", borderRadius: 4, color: "var(--fg-faint)" }}>ESC</kbd>
        </div>
        <div style={{ flex: 1, overflowY: "auto", padding: 6 }}>
          {results.length === 0 ? (
            <div style={{ padding: 40, textAlign: "center", color: "var(--fg-faint)", fontSize: 13 }}>没有匹配的结果</div>
          ) : results.map((r, i) => (
            <div key={i} onMouseEnter={()=>setActiveIdx(i)} onClick={()=>{r.action(); onClose();}} style={{
              display: "flex", alignItems: "center", gap: 12,
              padding: "9px 12px", borderRadius: 6,
              background: i === activeIdx ? "var(--bg-3)" : "transparent",
              cursor: "pointer", marginBottom: 2,
            }}>
              <div style={{ width: 28, height: 28, borderRadius: 6, background: `var(--${r.color}-soft)`, color: `var(--${r.color})`, display: "grid", placeItems: "center", flexShrink: 0 }}>{r.icon}</div>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 14, fontWeight: 500, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{r.title}</div>
                <div className="mono meta ellip" style={{ marginTop: 2 }}>{r.sub}</div>
              </div>
              <span className="chip">{r.kind}</span>
              {i === activeIdx && <Icons.ArrowUp size={12} style={{ transform: "rotate(90deg)", color: "var(--cyan)" }}/>}
            </div>
          ))}
        </div>
        <div style={{ padding: "8px 14px", borderTop: "1px solid var(--line-soft)", display: "flex", gap: 16, fontSize: 11, color: "var(--fg-faint)" }}>
          <span><kbd className="mono" style={{ padding: "1px 5px", border: "1px solid var(--line)", borderRadius: 3 }}>↑↓</kbd> 选择</span>
          <span><kbd className="mono" style={{ padding: "1px 5px", border: "1px solid var(--line)", borderRadius: 3 }}>↵</kbd> 跳转</span>
          <span><kbd className="mono" style={{ padding: "1px 5px", border: "1px solid var(--line)", borderRadius: 3 }}>ESC</kbd> 关闭</span>
        </div>
    </Overlay>
  );
}

function NotifPopover({ open, onClose, anchor }) {
  const store = useStore();
  if (!open) return null;
  // 通知取自 store 数据：活动告警 + 进行中/失败的任务。
  const notifs = [
    ...store.alarms.filter(a => a.status === "active").slice(0, 6).map(a => ({
      id: "al-" + a.id, kind: "alarm", level: a.severity === "error" ? "err" : "warn",
      text: `${a.type} · ${a.resource}${a.value ? " · " + a.value : ""}`, time: a.time,
    })),
    ...store.tasks.filter(t => t.status === "running" || t.status === "failed").slice(0, 6).map(t => ({
      id: "tk-" + t.id, kind: "task", level: t.status === "failed" ? "err" : "info",
      text: `${t.type} · ${t.target}${t.status === "failed" ? " · 失败" : ` · ${Math.round(t.progress)}%`}`, time: t.started,
    })),
  ];
  const colorMap = { err: "rose", warn: "amber", info: "cyan", ok: "emerald" };
  const iconMap  = { alarm: <Icons.Bell size={13}/>, task: <Icons.Tasks size={13}/>, system: <Icons.Cog size={13}/> };

  return (
    <>
      <div onClick={onClose} style={{ position: "fixed", inset: 0, zIndex: "var(--z-popover)" }}/>
      <div className="card fade-in" style={{
        position: "fixed", top: 56, right: 24, zIndex: "var(--z-popover)",
        width: 360, maxHeight: 480, display: "flex", flexDirection: "column", overflow: "hidden",
        boxShadow: "0 8px 28px -8px oklch(0 0 0 / 0.6)",
      }}>
        <div className="card-h">
          <div className="row"><Icons.Bell size={14}/><span className="card-title-zh">通知中心</span><span className="chip rose">{notifs.filter(n=>n.level==="err"||n.level==="warn").length} 未读</span></div>
          <button className="btn ghost icon" onClick={onClose}><Icons.X size={12}/></button>
        </div>
        <div style={{ flex: 1, overflowY: "auto" }}>
          {notifs.length === 0 && (
            <div style={{ padding: 32, textAlign: "center", color: "var(--fg-faint)", fontSize: 13 }}>暂无通知</div>
          )}
          {notifs.map(n => (
            <div key={n.id} className="lrow" style={{ alignItems: "flex-start", padding: "10px 14px", cursor: "pointer" }} onClick={() => { store.goto(n.kind === "alarm" ? "alarms" : n.kind === "task" ? "tasks" : "logs"); onClose(); }}>
              <div style={{ width: 24, height: 24, borderRadius: 6, background: `var(--${colorMap[n.level]}-soft)`, color: `var(--${colorMap[n.level]})`, display: "grid", placeItems: "center", flexShrink: 0, marginTop: 1 }}>{iconMap[n.kind]}</div>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 13, lineHeight: 1.5 }}>{n.text}</div>
                <div className="mono" style={{ fontSize: 11, color: "var(--fg-faint)", marginTop: 4 }}>{n.time} · {n.kind === "alarm" ? "告警" : n.kind === "task" ? "任务" : "系统"}</div>
              </div>
            </div>
          ))}
        </div>
        <div style={{ padding: "10px 14px", borderTop: "1px solid var(--line-soft)", display: "flex", gap: 8 }}>
          <button className="btn primary" style={{ flex: 1, justifyContent: "center" }} onClick={() => { store.goto("alarms"); onClose(); }}>查看全部</button>
        </div>
      </div>
    </>
  );
}

window.GlobalSearch = GlobalSearch;
window.NotifPopover = NotifPopover;

export { GlobalSearch, NotifPopover };
