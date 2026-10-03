// 数据层 hooks：统一管理加载/错误状态、写入→提示→重载流程和防重入轮询，
// 页面只需声明取数函数和成功提示。
import React from 'react';
import { api } from './api.js';

// 解开后端的 `{items: [...]}` 列表包装（或直接是数组）。

// useResource(fetcher, deps) → { data, loading, error, reload, setData }
// 挂载及 deps 变化时取数，reload() 原地重取；重取失败保留旧数据并给出 `error`。
export function useResource(fetcher, deps = []) {
  const [data, setData] = React.useState(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  // 只认最后一次发出的请求：快速翻页或切筛选时，慢的旧请求后到会盖掉新数据。
  const seq = React.useRef(0);
  const reload = React.useCallback(async () => {
    const mine = ++seq.current;
    try {
      const d = await fetcher();
      if (mine !== seq.current) return;
      setData(d);
      setError("");
    } catch (e) {
      if (mine !== seq.current) return;
      setError(e.message || "加载失败");
    } finally {
      if (mine === seq.current) setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  // 只有 deps 变化才回到加载中；轮询和写入后的 reload() 原地刷新，不闪加载态。
  React.useEffect(() => { setLoading(true); reload(); }, [reload]);
  return { data, loading, error, reload, setData };
}

// followTask 轮询任务直到终态。返回 task_id 的写入只表示已受理，
// 结果要跟踪任务才知道；失败时展示控制层记录的面向操作者的原因。
const TASK_POLL_MS = 1500;
const TASK_GIVE_UP_MS = 30 * 60 * 1000; // 镜像导入可能跑好几分钟

async function followTask(taskId, { onSettled, alive }) {
  const deadline = Date.now() + TASK_GIVE_UP_MS;
  while (Date.now() < deadline) {
    await new Promise(r => setTimeout(r, TASK_POLL_MS));
    if (alive && !alive()) return;
    let task;
    try {
      task = await api.getTask(taskId);
    } catch {
      continue; // 单次轮询失败不等于任务失败
    }
    if (task?.Status === "success" || task?.Status === "failed") {
      onSettled(task);
      return;
    }
  }
}

// onMutation 在任一写入成功后（以及跟踪的任务结束时）触发，让全局 store
// 立即重读侧栏徽标和 ⌘K 依赖的数据，不必等下一次 30 秒刷新。
export const onMutation = (() => {
  const target = new EventTarget();
  return {
    emit: () => target.dispatchEvent(new Event('mutation')),
    subscribe: (fn) => {
      target.addEventListener('mutation', fn);
      return () => target.removeEventListener('mutation', fn);
    },
  };
})();

// useMutation(toast) → { busy, run }
// run(fn, okMsg, {reload}) 为写入调用包上忙碌标记、成功/失败提示和后续重取，成功返回 true。
// 返回 task_id 时受理即返回并在后台跟踪任务：完成后刷新列表，失败时展示任务记录的原因。
export function useMutation(toast) {
  const [busy, setBusy] = React.useState(false);
  const alive = React.useRef(true);
  React.useEffect(() => () => { alive.current = false; }, []);
  const run = React.useCallback(async (fn, okMsg, { reload, errMsg = "操作失败" } = {}) => {
    setBusy(true);
    try {
      const res = await fn();
      if (okMsg) toast(okMsg, "ok");
      onMutation.emit();
      const taskId = res && (res.task_id || res.taskId);
      if (taskId) {
        followTask(taskId, {
          alive: () => alive.current,
          onSettled: (task) => {
            if (task.Status === "failed") toast(task.Error || errMsg, "err");
            if (reload) reload();
            onMutation.emit();
          },
        });
        return true;
      }
      if (reload) await reload();
      return true;
    } catch (e) {
      toast(e.message || errMsg, "err");
      return false;
    } finally {
      setBusy(false);
    }
  }, [toast]);
  return { busy, run };
}

// usePolling(fn, { intervalMs, active }) 定时调用并防重入，慢请求不会堆积；
// 轮询错误静默处理（下次重试，useResource 保留上次的好数据）。
export function usePolling(fn, { intervalMs, active = true } = {}) {
  React.useEffect(() => {
    if (!active || !intervalMs) return;
    let inflight = false;
    const id = setInterval(async () => {
      if (inflight) return;
      inflight = true;
      try { await fn(); } catch { /* 下次重试 */ }
      inflight = false;
    }, intervalMs);
    return () => clearInterval(id);
  }, [fn, intervalMs, active]);
}

export const PAGE_SIZES = [10, 20, 50];

// 服务端分页列表的页码状态，改每页条数时回到第 1 页。
export function usePageState(initialSize = PAGE_SIZES[0]) {
  const [page, setPage] = React.useState(1);
  const [size, setSizeState] = React.useState(initialSize);
  const setSize = React.useCallback((n) => { setSizeState(n); setPage(1); }, []);
  return { page, size, setPage, setSize, pager: (total) => ({ page, size, total, onPage: setPage, onSize: setSize }) };
}

// 对内存中列表做前端分页。
export function usePaged(items, initialSize = PAGE_SIZES[0]) {
  const { page, size, setPage, setSize } = usePageState(initialSize);
  const pages = Math.max(1, Math.ceil(items.length / size));
  const cur = Math.min(page, pages);
  return {
    rows: items.slice((cur - 1) * size, cur * size),
    page: cur, size, setPage,
    pager: { page: cur, size, total: items.length, onPage: setPage, onSize: setSize },
  };
}
