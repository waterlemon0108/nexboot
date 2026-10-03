// 应用外壳：侧栏导航、顶栏、页面路由，接真实后端会话与认证。
import React from 'react';
import { createRoot } from 'react-dom/client';
import './styles.css';

import { Icons } from './design/icons.jsx';
import { Logo, Wordmark } from './design/brand.jsx';
import { StoreProvider, useStore } from './design/store.jsx';
import { Modal, ConfirmProvider } from './design/overlay.jsx';
import { StandbyBanner } from './design/ha.jsx';
import { Field } from './design/primitives.jsx';
import { GlobalSearch, NotifPopover } from './design/global-search.jsx';
import { LoginPage } from './design/pages/login.jsx';
import { api } from './lib/api.js';
import { PageOverview } from './design/pages/overview.jsx';
import { PageServers } from './design/pages/servers.jsx';
import { PageTerminals } from './design/pages/terminals.jsx';
import { PageImages } from './design/pages/images.jsx';
import { PageGroups } from './design/pages/groups.jsx';
import { PageAlarms } from './design/pages/alarms.jsx';
import { PageStorage } from './design/pages/storage.jsx';
import { PageTasks } from './design/pages/tasks.jsx';
import { PageDrivers } from './design/pages/drivers.jsx';
import { PageLogs } from './design/pages/logs.jsx';
import { PageSettings, PageSystemParams } from './design/pages/settings.jsx';
import { clearSession, getStoredUser, getToken, onUnauthorized } from './lib/api.js';

const { useState: useS, useEffect: useE } = React;

// PAGES 是唯一的页面注册表：侧栏导航（顺序与分区）、路由、面包屑、页头、侧栏徽标和
// Alt-N 快捷键都由它派生，新增页面只需加一项。依赖实时数据的 `count`/`header` 可以是 store 的函数。
const PAGES = [
  { section: "运维监控" },
  { id: "overview",  label: "总览",         icon: Icons.Dashboard, component: PageOverview,
    header: { h: "总览", s: "无盘系统实时概览 · 终端、分组、存储与镜像" } },
  { id: "alarms",    label: "告警中心",     icon: Icons.Bell, accent: "amber", component: PageAlarms,
    count: (store) => store.alarms.filter(a => a.status === "active").length,
    header: { h: "告警中心", s: "服务器、存储、心跳与服务异常告警 · 处理与归档" } },
  { id: "tasks",     label: "任务中心",     icon: Icons.Tasks, component: PageTasks,
    count: (store) => store.tasks.filter(t => t.status === "running" || t.status === "waiting").length,
    header: { h: "任务中心", s: "镜像分发、快照、备份等后台异步任务执行情况" } },

  { section: "资源管理" },
  { id: "servers",   label: "服务器管理",   icon: Icons.Server, component: PageServers, shortcut: "1",
    header: { h: "服务器管理", s: "节点状态 · 无盘服务(dnsmasq / iSCSI / ZFS)管理与数据备份" } },
  { id: "storage",   label: "存储池管理",   icon: Icons.Database, component: PageStorage, shortcut: "4",
    count: (store) => store.pools.length,
    header: { h: "存储池管理", s: "ZFS 存储池容量、读写缓存与磁盘成员管理" } },

  { id: "images",    label: "镜像管理",     icon: Icons.Layers, component: PageImages, shortcut: "3",
    count: (store) => store.images.length,
    header: { h: "镜像管理", s: "镜像导入 · 每个镜像下的配置版本与还原点" } },
  { id: "groups",    label: "分组管理",     icon: Icons.Group, component: PageGroups,
    count: (store) => store.groups.length,
    header: { h: "分组管理", s: "按区域/配置组织客户机 · 网络配置与镜像分配" } },
  { id: "terminals", label: "客户机管理",   icon: Icons.Monitor, component: PageTerminals, shortcut: "2",
    count: (store) => store.terminals.length,
    // 数量已在侧栏徽标和页面筛选里，副标题不再重复。
    header: { h: "客户机管理", s: "在线状态、分组与系统镜像" } },
  { id: "drivers",   label: "驱动管理",     icon: Icons.Cpu, component: PageDrivers,
    header: { h: "驱动管理", s: "驱动包资产化管理 · INF 解析、启动驱动集组合与风险标记" } },

  { section: "系统管理" },
  { id: "logs",      label: "操作日志",     icon: Icons.Log, component: PageLogs,
    header: { h: "日志审计", s: "按用户、时间、模块查询操作日志与登录日志" } },
  { id: "settings",  label: "用户管理",     icon: Icons.User, component: PageSettings,
    header: { h: "用户管理", s: "账号、密码与管理员用户维护" } },
  { id: "system-params", label: "系统参数", icon: Icons.Cog, component: PageSystemParams,
    header: { h: "系统参数", s: "全局运行参数与服务目录配置" } },
];
const PAGE_BY_ID = Object.fromEntries(PAGES.filter(p => p.id).map(p => [p.id, p]));

function Sidebar({ onLogout, account }) {
  const store = useStore();
  const [userMenuOpen, setUserMenuOpen] = useS(false);
  const [pwOpen, setPwOpen] = useS(false);
  const [version, setVersion] = useS("");
  const userMenuRef = React.useRef(null);
  const name = account?.username || account?.Username || "admin";
  const avatar = name.charAt(0).toUpperCase();
  // 真实后端版本，首次打开菜单时取一次。
  useE(() => {
    if (!userMenuOpen || version) return;
    api.getServer().then(s => setVersion(s?.version || "")).catch(() => {});
  }, [userMenuOpen, version]);
  useE(() => {
    if (!userMenuOpen) return;
    const onDoc = (e) => { if (userMenuRef.current && !userMenuRef.current.contains(e.target)) setUserMenuOpen(false); };
    const onKey = (e) => { if (e.key === "Escape") setUserMenuOpen(false); };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("mousedown", onDoc); document.removeEventListener("keydown", onKey); };
  }, [userMenuOpen]);
  return (
    <aside className="sidebar">
      <div className="brand">
        <Logo size={34}/>
        <div>
          <div className="brand-name"><Wordmark size={15}/></div>
          <div className="brand-sub">Diskless Endpoint</div>
        </div>
      </div>

      <div className="nav">
        {PAGES.map((it, i) => {
          if (it.section) return <div key={i} className="nav-section">{it.section}</div>;
          const count = it.count ? it.count(store) : null;
          return (
            <div key={it.id} className={`nav-item ${store.route.page === it.id ? "active" : ""}`} onClick={() => store.goto(it.id)}>
              <it.icon size={16}/>
              <span>{it.label}</span>
              {count != null && <span className="count" style={it.accent && count > 0 ? { color: `var(--${it.accent})` } : null}>{count}</span>}
            </div>
          );
        })}
      </div>

      <div className="sidebar-foot" ref={userMenuRef} style={{ position: "relative" }}>
        <div
          className={`user-trigger ${userMenuOpen ? "open" : ""}`}
          onClick={() => setUserMenuOpen(o => !o)}
          role="button"
          tabIndex={0}
        >
          <div className="avatar">{avatar}</div>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ fontSize: 13, fontWeight: 500 }}>{name}</div>
            <div className="meta">管理员</div>
          </div>
          <Icons.Chevron size={12} className={`user-trigger-chev ${userMenuOpen ? "rot" : ""}`}/>
        </div>
        {userMenuOpen && (
          <div className="user-menu">
            <div className="user-menu-head">
              <div className="avatar" style={{ width: 36, height: 36, fontSize: 14 }}>{avatar}</div>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 14, fontWeight: 600 }}>{name}</div>
                <div className="meta" style={{ marginTop: 2 }}>管理员</div>
              </div>
              <span className="chip emerald"><span className="dot live"/> 在线</span>
            </div>
            <div className="user-menu-body">
              <button className="user-mi" onClick={() => { setUserMenuOpen(false); store.goto("settings"); }}>
                <Icons.User size={14}/><span>用户管理</span>
              </button>
              <button className="user-mi" onClick={() => { setUserMenuOpen(false); setPwOpen(true); }}>
                <Icons.Lock size={14}/><span>修改密码</span>
              </button>
              <button className="user-mi" onClick={() => { setUserMenuOpen(false); store.goto("logs"); }}>
                <Icons.Log size={14}/><span>登录日志</span>
              </button>
              <div className="user-menu-sep"/>
              <button className="user-mi danger" onClick={() => { setUserMenuOpen(false); onLogout && onLogout(); }}>
                <Icons.Power size={14}/><span>退出登录</span>
                <span style={{ flex: 1 }}/>
                <Icons.Chevron size={11}/>
              </button>
            </div>
            <div className="user-menu-foot mono">
              ndiskless {version || "—"}
            </div>
          </div>
        )}
      </div>
      <ChangePasswordModal open={pwOpen} account={account} onClose={() => setPwOpen(false)} onChanged={onLogout}/>
    </aside>
  );
}

// 修改当前用户自己的密码；成功后退出登录，立即用新密码登录。
function ChangePasswordModal({ open, account, onClose, onChanged }) {
  const store = useStore();
  const [form, setForm] = useS({ old_password: "", new_password: "" });
  const [busy, setBusy] = useS(false);
  useE(() => { if (open) setForm({ old_password: "", new_password: "" }); }, [open]);

  const submit = async () => {
    if (!form.old_password || !form.new_password) { store.toast("请填写原密码与新密码", "err"); return; }
    if (form.new_password.length < 6) { store.toast("新密码至少 6 位", "err"); return; }
    const id = account?.id || account?.ID;
    if (!id) { store.toast("未识别当前用户，请在「用户管理」中修改", "err"); return; }
    setBusy(true);
    try {
      await api.changeUserPassword(id, { old_password: form.old_password, new_password: form.new_password });
      store.toast("密码已修改，请用新密码重新登录", "ok");
      onClose();
      onChanged && onChanged();
    } catch (e) {
      store.toast(e.message || "修改失败", "err");
    } finally { setBusy(false); }
  };

  return (
    <Modal open={open} onClose={() => !busy && onClose()} title="修改密码" size="sm"
      footer={<><button className="btn" disabled={busy} onClick={onClose}>取消</button><button className="btn primary" disabled={busy} onClick={submit}>{busy ? "提交中…" : "确认"}</button></>}>
      <Field label="原密码" required><input className="input mono" type="password" style={{ width: "100%" }} value={form.old_password} onChange={e => setForm({ ...form, old_password: e.target.value })}/></Field>
      <Field label="新密码" required hint="至少 6 位，改完需重新登录"><input className="input mono" type="password" style={{ width: "100%" }} value={form.new_password} onChange={e => setForm({ ...form, new_password: e.target.value })}/></Field>
    </Modal>
  );
}

function Topbar({ onSearch, onNotif, notifBtnRef }) {
  const store = useStore();
  const [now, setNow] = useS(new Date());
  useE(() => { const id = setInterval(() => setNow(new Date()), 1000); return () => clearInterval(id); }, []);
  const fmt = (n) => String(n).padStart(2,"0");
  const time = `${fmt(now.getHours())}:${fmt(now.getMinutes())}:${fmt(now.getSeconds())}`;

  return (
    <header className="topbar">
      <div className="crumbs">
        <Icons.Dashboard size={13}/>
        <span style={{ cursor: "pointer" }} onClick={() => store.goto("overview")}>控制台</span>
        <Icons.Chevron size={11} className="sep"/>
        <span className="cur">{PAGE_BY_ID[store.route.page]?.label || store.route.page}</span>
      </div>
      <div className="grow"/>
      <div className="search" onClick={onSearch} style={{ cursor: "pointer" }}>
        <Icons.Search size={13}/>
        <span className="ellip" style={{ flex: 1, color: "var(--fg-faint)" }}>搜索服务器 / 客户机 / 镜像 / 告警...</span>
        <kbd>⌘K</kbd>
      </div>
      <div className="row" style={{ gap: 10 }}>
        <SystemHealthChip/>
        <span className="mono" style={{ fontSize: 12, color: "var(--fg-mute)" }}>{time}</span>
        <button ref={notifBtnRef} className="btn ghost icon" onClick={onNotif} style={{ position: "relative" }}>
          <Icons.Bell size={15}/>
          {store.alarms.filter(a => a.status === "active").length > 0 &&
            <span style={{ position: "absolute", top: 4, right: 4, width: 6, height: 6, borderRadius: "50%", background: "var(--amber)", boxShadow: "0 0 0 2px var(--bg-1)" }}/>}
        </button>
      </div>
    </header>
  );
}

// 系统健康状态由活动告警推导（store 负责加载和刷新），点击进入告警中心。
function SystemHealthChip() {
  const store = useStore();
  const active = store.alarms.filter(a => a.status === "active");
  const errors = active.filter(a => a.severity === "error").length;
  const cls = errors > 0 ? "rose" : active.length > 0 ? "amber" : "emerald";
  const dot = errors > 0 ? "err" : active.length > 0 ? "warn" : "live";
  const text = active.length > 0 ? `${active.length} 条告警` : "系统正常";
  return (
    <span className={`chip ${cls}`} style={{ cursor: "pointer" }} title="点击查看告警中心"
      onClick={() => store.goto("alarms")}>
      <span className={`dot ${dot}`}/> {text}
    </span>
  );
}

function AppInner({ onLogout, account }) {
  const store = useStore();
  const page = store.route.page;
  const [searchOpen, setSearchOpen] = useS(false);
  const [notifOpen, setNotifOpen] = useS(false);
  const notifBtnRef = React.useRef(null);

  useE(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); setSearchOpen(true); }
      if (e.key === "Escape") { setSearchOpen(false); setNotifOpen(false); }
      if (e.altKey) {
        const target = PAGES.find(p => p.shortcut === e.key);
        if (target) store.goto(target.id);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const pg = PAGE_BY_ID[page] || PAGE_BY_ID.overview;
  const Content = pg.component;
  const head = typeof pg.header === "function" ? pg.header(store) : pg.header;

  return (
    <div className="app" data-screen-label={page}>
      <Sidebar onLogout={onLogout} account={account}/>
      <Topbar onSearch={() => setSearchOpen(true)} onNotif={() => setNotifOpen(o => !o)} notifBtnRef={notifBtnRef}/>
      <StandbyBanner/>
      <GlobalSearch open={searchOpen} onClose={() => setSearchOpen(false)}/>
      <NotifPopover open={notifOpen} onClose={() => setNotifOpen(false)}/>
      <main className="main">
        {head && (
          <div className="page-head">
            <div className="page-title">
              <h1>{head.h}</h1>
              <div className="sub">{head.s}</div>
            </div>
          </div>
        )}
        <Content/>
      </main>
    </div>
  );
}

function App() {
  const [user, setUser] = useS(() => (getToken() ? getStoredUser() : null));
  const [leaving, setLeaving] = useS(false);

  useE(() => onUnauthorized.subscribe(() => { setUser(null); setLeaving(false); }), []);

  const handleLogout = () => {
    clearSession();
    setUser(null);
    setLeaving(false);
  };

  if (!user) {
    return (
      <div className={leaving ? "login-root leaving" : ""} style={leaving ? { position: "fixed", inset: 0, zIndex: 100 } : null}>
        <LoginPage onLogin={(u) => {
          setLeaving(true);
          setTimeout(() => setUser(u || getStoredUser() || { username: "admin" }), 520);
        }}/>
      </div>
    );
  }

  return <StoreProvider><ConfirmProvider><AppInner onLogout={handleLogout} account={user}/></ConfirmProvider></StoreProvider>;
}

createRoot(document.getElementById('root')).render(<App/>);
