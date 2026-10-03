// 基于 React context 的全局状态（路由、提示、加载好的集合）。UI 基础组件在 overlay.jsx / primitives.jsx。
import React from 'react';
import { api as backendApi } from '../lib/api.js';
import { onMutation } from '../lib/hooks.js';
import { TASK_LABELS, fmtDateTime, fmtTime } from '../lib/format.js';

const StoreCtx = React.createContext(null);

// ── 实时数据加载辅助 ──────────────────────────────────────────────
// 把后端原始记录映射成 ⌘K 面板和通知弹层用的精简结构（页面本身直接用原始记录）。
// 这里没有样例数据，后端为空时集合就为空。
const unwrapList = (r) => (Array.isArray(r) ? r : (r && Array.isArray(r.items) ? r.items : null));
const mapImage = (im) => ({
  id: im.ID,
  type: "system", // 后端区分 windows/linux，不区分 system/game
  name: im.Name,
  size: Math.max(0.1, +(Number(im.Size || 0) / (1024 ** 3)).toFixed(1)),
  status: im.State === "normal" ? "published" : im.State === "importing" ? "publishing" : im.State === "error" ? "building" : "idle",
  unit: 0, default: "—", updated: fmtDateTime(im.CreatedAt), configs: 0, snapshots: 0,
});
const mapTask = (t) => ({
  id: t.ID,
  type: TASK_LABELS[t.Type] || t.Type,
  target: t.TargetRef || t.Message || "—",
  progress: t.Progress || 0,
  status: t.Status === "success" ? "done" : t.Status === "failed" ? "failed" : t.Status === "running" ? "running" : "waiting",
  started: fmtTime(t.CreatedAt),
});

function StoreProvider({ children }) {
  // 这些集合只供侧栏/顶栏徽标、⌘K 搜索和通知弹层使用，页面经 useResource 自己取数。
  // 初始为空并无条件以后端结果覆盖（包括空），空库显示真实的 0。
  const [pools,     setPools]     = React.useState([]);
  const [groups,    setGroups]    = React.useState([]);
  const [images,    setImages]    = React.useState([]);
  const [alarms,    setAlarms]    = React.useState([]);
  const [terminals, setTerminals] = React.useState([]);
  const [tasks,     setTasks]     = React.useState([]);

  const [toasts, setToasts]       = React.useState([]);
  // 错误提示比成功提示停留更久，因为带有操作者可能要读的后端细节；点击任何提示即关闭。
  const toast = (msg, kind = "info") => {
    const id = Math.random().toString(36).slice(2);
    setToasts(t => [...t, { id, msg, kind }]);
    setTimeout(() => setToasts(t => t.filter(x => x.id !== id)), kind === "err" ? 6500 : 3200);
  };
  const dismissToast = (id) => setToasts(t => t.filter(x => x.id !== id));

  // 页面间导航
  const [route, setRoute] = React.useState({ page: "overview", params: {} });
  const goto = (page, params = {}) => setRoute({ page, params });

  const refreshActiveTasks = React.useCallback(async () => {
    const rawTasks = unwrapList(await backendApi.listTasks());
    keepIfSame(setTasks, (rawTasks || []).map(mapTask));
  }, []);

  React.useEffect(() => {
    const load = async () => {
      try {
        await refreshActiveTasks();
      } catch {
        return;
      }
    };
    load();
    const id = setInterval(load, 3000);
    return () => {
      clearInterval(id);
    };
  }, [refreshActiveTasks]);

  // 从后端加载徽标/搜索/通知集合并定时刷新：会话一开就是几小时，只在挂载时加载会让数字整天过时。
  React.useEffect(() => {
    let cancelled = false;
    const hydrate = async () => {
      const [imgR, grpR, termR, poolR, taskR, alarmR, nodeR, cpoolR] = await Promise.allSettled([
        backendApi.listImages(), backendApi.listGroups(), backendApi.listTerminals(), backendApi.listPools(), backendApi.listTasks(), backendApi.listAlarms({ size: 100 }),
        backendApi.listClusterNodes(), backendApi.listClusterPools(),
      ]);
      if (cancelled) return;
      const raw = (r) => (r.status === "fulfilled" ? unwrapList(r.value) : null);
      const rawImgs = raw(imgR), rawGrps = raw(grpR), rawTerms = raw(termR), rawPools = raw(poolR), rawTasks = raw(taskR), rawAlarms = raw(alarmR);
      const rawNodes = raw(nodeR);
      const clusterPools = cpoolR.status === "fulfilled" ? cpoolR.value : null;

      const imgById = {};
      (rawImgs || []).forEach(im => { imgById[im.ID] = im; });

      // 分组（⌘K 用的结构）：解析镜像名，统计在线客户机
      const designGroups = (rawGrps || []).map(g => {
        const inGroup = (rawTerms || []).filter(t => t.GroupID === g.ID);
        return {
          id: g.ID, name: g.Name,
          startIp: g.StartIP || "—", max: g.ClientMax || inGroup.length || 0,
          gateway: g.Gateway || "—", mask: g.Netmask || "—", dns: g.DNS1 || "—",
          image: (imgById[g.SystemImageID] && imgById[g.SystemImageID].Name) || "—",
          total: g.ClientMax || inGroup.length || 0,
          online: inGroup.filter(t => t.State === "online").length,
        };
      });
      const grpById = {};
      designGroups.forEach(g => { grpById[g.id] = g; });

      keepIfSame(setImages, (rawImgs || []).map(mapImage));
      keepIfSame(setGroups, designGroups);
      // 池记录复制有先后，只数本机库里的记录会和存储池页对不上；徽标与存储池页、总览一样
      // 以「各节点自报的池」为准。只读取池的数量（.length）。
      const clustered = (rawNodes || []).length > 1;
      keepIfSame(setPools, clustered && clusterPools
        ? (clusterPools.nodes || []).flatMap(n => n.pools || [])
        : (rawPools || []));
      keepIfSame(setTerminals, (rawTerms || []).map((t, i) => ({
        idx: i + 1, id: t.ID, mac: t.MAC, ip: t.IP, group: t.GroupID,
        state: t.State === "online" ? "on" : t.State === "offline" ? "off" : "warn",
        image: (grpById[t.GroupID] && grpById[t.GroupID].image) || "—",
        superAdmin: !!t.IsSuper,
      })));

      if (taskR.status === "fulfilled") {
        keepIfSame(setTasks, (rawTasks || []).map(mapTask));
      }
      if (alarmR.status === "fulfilled") {
        keepIfSame(setAlarms, (rawAlarms || []).map(a => ({
          id: a.ID, severity: a.Severity,
          status: a.Status === "recovered" ? "recovered" : a.Status === "acknowledged" ? "acknowledged" : "active",
          type: a.Type, resource: a.Resource, value: a.Value, threshold: a.Threshold,
          time: fmtDateTime(a.UpdatedAt || a.CreatedAt),
        })));
      }
    };
    hydrate().catch(() => { /* 全部失败时集合保持为空 */ });
    const id = setInterval(() => hydrate().catch(() => { /* 下次重试 */ }), 30000);
    // 页面刚改了东西（删镜像、建池）时立即重读，徽标不必等到下次刷新。
    const unsubscribe = onMutation.subscribe(() => hydrate().catch(() => { /* 下次重试 */ }));
    return () => { cancelled = true; clearInterval(id); unsubscribe(); };
  }, []);

  return (
    <StoreCtx.Provider value={{ pools, groups, images, alarms, terminals, tasks, route, goto, toast }}>
      {children}
      <ToastStack toasts={toasts} onDismiss={dismissToast}/>
    </StoreCtx.Provider>
  );
}

function useStore() { return React.useContext(StoreCtx); }

// keepIfSame 让内容没变的重读什么也不做：轮询每 3 秒重新 map 出全新数组，React 按身份比较，
// 会让 provider 和整棵树白白重渲染。
// 用 JSON 比较：字段随接口演进，手写逐字段比较迟早漏字段，漏掉的字段变了却不更新更糟；
// 列表只有几十行，序列化远比全树重渲染便宜。
function keepIfSame(setter, next) {
  setter(prev => (sameContent(prev, next) ? prev : next));
}

function sameContent(a, b) {
  if (a === b) return true;
  try {
    return JSON.stringify(a) === JSON.stringify(b);
  } catch {
    return false; // 循环引用等情况：当作已变，宁可多渲染一次
  }
}

function ToastStack({ toasts, onDismiss }) {
  return (
    <div style={{ position: "fixed", top: 24, left: "50%", transform: "translateX(-50%)", zIndex: "var(--z-toast)", display: "flex", flexDirection: "column", alignItems: "center", gap: 8, pointerEvents: "none" }}>
      {toasts.map(t => (
        <div key={t.id} className="card" onClick={() => onDismiss && onDismiss(t.id)} title="点击关闭" style={{
          padding: "10px 14px", display: "flex", alignItems: "center", gap: 10, minWidth: 260, maxWidth: "min(560px, 90vw)", pointerEvents: "auto", cursor: "pointer",
          borderColor: t.kind === "ok" ? "oklch(0.55 0.1 155 / 0.5)" : t.kind === "err" ? "oklch(0.55 0.14 18 / 0.5)" : "var(--line)",
          background: t.kind === "ok" ? "var(--emerald-soft)" : t.kind === "err" ? "var(--rose-soft)" : "var(--bg-2)",
          animation: "fadeIn 0.2s",
        }}>
          <span className={`dot ${t.kind === "ok" ? "live" : t.kind === "err" ? "err" : "cyan"}`}/>
          <span style={{ fontSize: 13, color: t.kind === "ok" ? "var(--emerald)" : t.kind === "err" ? "var(--rose)" : "var(--fg)" }}>{t.msg}</span>
        </div>
      ))}
    </div>
  );
}

window.StoreProvider = StoreProvider;
window.useStore = useStore;

export { StoreProvider, useStore };
