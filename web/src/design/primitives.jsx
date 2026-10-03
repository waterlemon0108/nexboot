// 公共基础组件，只放至少两处真实调用的组件。
import React from 'react';
import { createPortal } from 'react-dom';
import { Icons } from './icons.jsx';
import { PAGE_SIZES } from '../lib/hooks.js';

// 表单项：标签在上，可选必填标记和辅助文字。`hint` 总是显示；`help` 收在小 ? 按钮后，
// 放操作者只需看一次的说明。`helpInline` 把说明放在控件旁边，`helpAfterLabel` 紧跟在标签后。
function Field({ label, required, hint, help, helpInline, helpAfterLabel, children }) {
  const [showHelp, setShowHelp] = React.useState(false);
  const helpText = help && showHelp ? <div className="hint" style={{ margin: 0 }}>{help}</div> : null;
  return (
    <div style={{ marginBottom: 14 }}>
      <div style={{ fontSize: 12, color: "var(--fg-mute)", marginBottom: 6, fontWeight: 500 }}>
        {label} {required && <span style={{ color: "var(--rose)" }}>*</span>}
        {help && (
          <button type="button" className="field-help" aria-label={`${label}说明`} aria-expanded={showHelp}
            onClick={() => setShowHelp(v => !v)}>?</button>
        )}
        {helpAfterLabel && help && showHelp && <span className="hint" style={{ marginLeft: 8, fontWeight: 400 }}>{help}</span>}
      </div>
      {helpInline
        ? <div style={{ display: "flex", alignItems: "center", gap: 12 }}>{children}{helpText}</div>
        : children}
      {hint && <div className="hint" style={{ marginTop: 4 }}>{hint}</div>}
      {!helpInline && !helpAfterLabel && help && showHelp && <div className="hint" style={{ marginTop: 4 }}>{help}</div>}
    </div>
  );
}

function Select({ value, onChange, options, ...rest }) {
  return (
    <select className="input" value={value} onChange={e => onChange(e.target.value)} style={{ width: "100%" }} {...rest}>
      {options.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
    </select>
  );
}

// 表格除「有数据」外三种状态共用的空行；否则筛选为空时只剩表头，看起来像页面坏了。
function TableState({ colSpan, loading, error, empty, hint = "暂无数据" }) {
  if (!loading && !error && !empty) return null;
  return (
    <tr>
      <td colSpan={colSpan} style={{
        textAlign: "center", padding: "32px 14px", fontSize: 13,
        color: error ? "var(--rose)" : "var(--fg-faint)",
      }}>
        {loading ? "加载中…" : error ? `加载失败：${error}` : hint}
      </td>
    </tr>
  );
}

// 带前置图标的搜索框。`onSubmit` 接回车，供查询后端而非就地过滤的页面使用。
function SearchBox({ value, onChange, placeholder, onSubmit, width = 240 }) {
  return (
    <div className="search-box" style={{ width }}>
      <Icons.Search size={12} style={{ color: "var(--fg-faint)", flexShrink: 0 }}/>
      <input value={value} placeholder={placeholder} aria-label={placeholder}
        onChange={e => onChange(e.target.value)}
        onKeyDown={e => { if (onSubmit && e.key === "Enter") onSubmit(); }}/>
    </div>
  );
}

// 列表分页：总数、每页条数、翻页。总数不超过最小一页时整条不显示，调用方可无条件放置。
function Pager({ page, size, total, onPage, onSize, sizes = PAGE_SIZES }) {
  if (total <= sizes[0]) return null;
  const pages = Math.max(1, Math.ceil(total / size));
  return (
    <div className="pager">
      <span className="meta" style={{ marginRight: "auto" }}>共 {total} 条</span>
      <Select aria-label="每页条数" value={String(size)} onChange={v => onSize(Number(v))}
        options={sizes.map(n => ({ value: String(n), label: `${n} 条/页` }))} style={{ width: 96 }}/>
      <button className="btn" disabled={page <= 1} onClick={() => onPage(page - 1)}>上一页</button>
      <span className="mono" style={{ fontSize: 13, color: "var(--fg-mute)" }}>{page} / {pages}</span>
      <button className="btn" disabled={page >= pages} onClick={() => onPage(page + 1)}>下一页</button>
    </div>
  );
}

// 分段筛选，计数放在分段内，控件与计数合为一体。选项：[{ id, label, n?, color? }]。
function SegFilter({ value, onChange, options }) {
  return (
    <div className="seg">
      {options.map(o => (
        <button key={o.id} type="button" className={value === o.id ? "on" : ""} onClick={() => onChange(o.id)}
          style={value === o.id && o.color ? { color: `var(--${o.color})`, boxShadow: `0 0 0 1px var(--${o.color}) inset`, background: `var(--${o.color}-soft)` } : null}>
          {o.label}{o.n != null && <span className="n">{o.n}</span>}
        </button>
      ))}
    </div>
  );
}

// 统计卡片。`pct` 加用量条，`sub` 加等宽说明。
function StatCard({ label, value, unit, sub, accent = "cyan", pct }) {
  return (
    <div className="card stat-card">
      <div className="lbl">{label}</div>
      <div style={{ display: "flex", alignItems: "baseline", gap: 5, marginTop: 5 }}>
        <span className="stat-num" style={{ color: `var(--${accent})` }}>{value}</span>
        {unit && <span style={{ fontSize: 13, color: "var(--fg-mute)" }}>{unit}</span>}
      </div>
      {sub && <div className="mono meta" style={{ marginTop: 7 }}>{sub}</div>}
      {pct != null && <div className={`bar ${pct > 85 ? "rose" : pct > 70 ? "amber" : ""}`} style={{ marginTop: 8 }}><span style={{ width: `${pct}%` }}/></div>}
      <div className="stat-card-orb" style={{ background: `var(--${accent}-soft)` }}/>
    </div>
  );
}

// 开关做成一个控件，而不是文字在「✓ 已启用」和「否」之间切换的按钮，那样读起来像操作而非当前状态。
function Toggle({ checked, onChange, onLabel = "已启用", offLabel = "已禁用" }) {
  return (
    <div className="seg" style={{ width: "fit-content" }}>
      <button type="button" className={!checked ? "on" : ""} onClick={() => onChange(false)}>{offLabel}</button>
      <button type="button" className={checked ? "on" : ""} onClick={() => onChange(true)}>{onLabel}</button>
    </div>
  );
}

// Tip：点击展开的说明。悬停没有可点的样子，原生 title 又要等一两秒才出现；
// 收起走一层透明背板，与通知中心同样做法，点页面任意处即收起。
function Tip({ text, label = "说明" }) {
  const [box, setBox] = React.useState(null); // 展开时的视口坐标
  React.useEffect(() => {
    if (!box) return undefined;
    const close = () => setBox(null);
    const onKey = (e) => { if (e.key === "Escape") close(); };
    window.addEventListener("keydown", onKey);
    window.addEventListener("scroll", close, true); // fixed 弹层滚动时会偏离按钮
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("scroll", close, true);
    };
  }, [box]);
  const toggle = (e) => {
    e.stopPropagation();
    if (box) { setBox(null); return; }
    // 挂在 body 上按视口坐标放，表格的滚动框裁不到它；窄屏收窄，左右各留 16px。
    const r = e.currentTarget.getBoundingClientRect();
    const vw = document.documentElement.clientWidth || window.innerWidth;
    const width = Math.min(320, vw - 32);
    const left = Math.max(16, Math.min(r.left - 8, vw - 16 - width));
    setBox({ left, top: r.bottom + 6, width });
  };
  return (
    <span style={{ display: "inline-flex" }}>
      <button type="button" aria-label={label} className="field-help" aria-expanded={!!box} onClick={toggle}>?</button>
      {box && createPortal(
        <>
          <span data-testid="tip-backdrop" onClick={() => setBox(null)}
            style={{ position: "fixed", inset: 0, zIndex: "var(--z-popover)" }}/>
          <span className="card" style={{
            position: "fixed", top: box.top, left: box.left, zIndex: "var(--z-popover)",
            width: box.width, padding: "10px 12px", fontSize: 13, lineHeight: 1.7,
            color: "var(--fg-soft)", fontWeight: 400, whiteSpace: "normal",
            boxShadow: "0 8px 24px oklch(0 0 0 / 0.45)",
          }}>{text}</span>
        </>,
        document.body,
      )}
    </span>
  );
}

export { Field, Select, TableState, SearchBox, Pager, SegFilter, StatCard, Toggle, Tip };
