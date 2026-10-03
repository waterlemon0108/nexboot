import React from 'react';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../lib/api.js';
import { StoreProvider, useStore } from './store.jsx';
import { onMutation } from '../lib/hooks.js';

// store 支撑侧栏计数、⌘K 面板和告警铃，同时从六个接口加载并持续刷新数小时：
// 一个接口坏只影响它自己的徽标，数字必须来自后端而非编造。

const list = (items) => ({ items, total: items.length });

function stubApi(overrides = {}) {
  const base = {
    listImages: async () => list([{ ID: 'win11', Name: 'Win 11', OSType: 'windows', State: 'normal', Size: 1e9, CreatedAt: '2026-07-30T10:00:00Z' }]),
    listGroups: async () => list([{ ID: 'g-1', Name: '教学一班', SystemImageID: 'win11', ClientMax: 30, StartIP: '192.168.50.1' }]),
    listTerminals: async () => list([
      { ID: 't-1', MAC: 'AABBCCDDEE01', IP: '192.168.50.1', GroupID: 'g-1', State: 'online', IsSuper: false },
      { ID: 't-2', MAC: 'AABBCCDDEE02', IP: '192.168.50.2', GroupID: 'g-1', State: 'offline', IsSuper: true },
    ]),
    listPools: async () => list([{ ID: 'pool-tank', Name: 'tank' }]),
    listTasks: async () => list([]),
    listAlarms: async () => list([{ ID: 'a-1', Severity: 'critical', Status: 'active', Type: 'pool_degraded', Resource: 'tank', CreatedAt: '2026-07-30T10:00:00Z' }]),
    listClusterNodes: async () => list([{ id: 'n1' }]),
    listClusterPools: async () => ({ nodes: [{ node_id: 'n1', reachable: true, pools: [{ ID: 'pool-tank', Name: 'tank' }] }], pool_count: 1 }),
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

function Probe() {
  const s = useStore();
  return (
    <div>
      <span data-testid="counts">{`${s.images.length}/${s.groups.length}/${s.terminals.length}/${s.pools.length}/${s.alarms.length}`}</span>
      <span data-testid="group">{s.groups[0] ? `${s.groups[0].name}·${s.groups[0].image}·${s.groups[0].online}` : ''}</span>
      <span data-testid="route">{s.route.page}</span>
      <button onClick={() => s.goto('images', { imageId: 'win11' })}>去镜像页</button>
      <button onClick={() => s.toast('保存成功', 'ok')}>提示</button>
      <button onClick={() => s.toast('删除失败：仍被占用', 'err')}>报错</button>
    </div>
  );
}

const renderStore = () => render(<StoreProvider><Probe/></StoreProvider>);

describe('全局 store', () => {
  beforeEach(() => stubApi());
  afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

  it('从后端把各处徽标的数字灌进来', async () => {
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('1/1/2/1/1'));
    // 分组卡片解析镜像名，只统计在线客户机。
    expect(screen.getByTestId('group').textContent).toBe('教学一班·Win 11·1');
  });

  // 任一页面写入成功后 store 立即重读，徽标不能等下一次 30 秒刷新。
  it('页面操作成功后立即重读，徽标不再滞后', async () => {
    let images = [{ ID: 'win11', Name: 'Win 11', OSType: 'windows', State: 'normal', Size: 1e9, CreatedAt: '2026-07-30T10:00:00Z' },
      { ID: 'ubuntu', Name: 'Ubuntu', OSType: 'linux', State: 'normal', Size: 1e9, CreatedAt: '2026-07-30T10:00:00Z' }];
    stubApi({ listImages: async () => list(images) });
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('2/1/2/1/1'));

    images = images.slice(0, 1);
    onMutation.emit();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('1/1/2/1/1'));
  });

  // 六个接口各自独立完成：告警接口故障不能让侧栏全空（单个 Promise.all 就会这样）。
  it('某个接口挂了只丢它自己那一格', async () => {
    stubApi({ listAlarms: async () => { throw new Error('500'); } });
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('1/1/2/1/0'));
  });

  it('后端为空时显示真零，而不是样例数据', async () => {
    stubApi({
      listImages: async () => list([]), listGroups: async () => list([]),
      listTerminals: async () => list([]), listPools: async () => list([]),
      listAlarms: async () => list([]),
    });
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('0/0/0/0/0'));
  });

  it('goto 带着参数换页', async () => {
    renderStore();
    await userEvent.click(screen.getByText('去镜像页'));
    expect(screen.getByTestId('route').textContent).toBe('images');
  });

  // 错误提示比成功提示停留更久，因为带有操作者可能要读的后端细节。
  it('错误提示比成功提示留得久', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    renderStore();

    await user.click(screen.getByText('提示'));
    await user.click(screen.getByText('报错'));
    expect(screen.getByText('保存成功')).toBeTruthy();
    expect(screen.getByText('删除失败：仍被占用')).toBeTruthy();

    await act(async () => { await vi.advanceTimersByTimeAsync(4000); });
    expect(screen.queryByText('保存成功')).toBeNull();
    expect(screen.getByText('删除失败：仍被占用')).toBeTruthy();

    await act(async () => { await vi.advanceTimersByTimeAsync(4000); });
    expect(screen.queryByText('删除失败：仍被占用')).toBeNull();
  });

  // 会话会持续数小时，只在挂载时加载会让徽标整天过时。
  it('任务列表持续刷新', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderStore();
    await waitFor(() => expect(api.listTasks).toHaveBeenCalled());
    const before = api.listTasks.mock.calls.length;

    await act(async () => { await vi.advanceTimersByTimeAsync(7000); });
    expect(api.listTasks.mock.calls.length).toBeGreaterThan(before);
  });
});

// 徽标与存储池页一样按「各节点自报的池」计数，否则复制未追平时两处数字不一致。
describe('侧边栏存储池徽标', () => {
  afterEach(() => vi.restoreAllMocks());

  it('集群里按各节点上报的池数，和存储池页对得上', async () => {
    stubApi({
      listPools: async () => list([{ ID: 'pool-a', Name: 'tank' }, { ID: 'pool-b', Name: 'tank' }]),
      listClusterNodes: async () => list([{ id: 'n1' }, { id: 'n2' }, { id: 'n3' }]),
      listClusterPools: async () => ({
        nodes: [
          { node_id: 'n1', reachable: true, pools: [{ ID: 'p1', Name: 'tank' }] },
          { node_id: 'n2', reachable: true, pools: [{ ID: 'p2', Name: 'tank' }] },
          { node_id: 'n3', reachable: true, pools: [{ ID: 'p3', Name: 'tank' }] },
        ],
        pool_count: 3,
      }),
    });
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('1/1/2/3/1'));
  });

  it('单机不绕集群接口，仍用本机的池列表', async () => {
    stubApi({
      listPools: async () => list([{ ID: 'pool-a', Name: 'tank' }]),
      listClusterNodes: async () => list([{ id: 'n1' }]),
    });
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('1/1/2/1/1'));
  });

  it('集群接口挂了就退回本机池列表，徽标不空掉', async () => {
    stubApi({
      listPools: async () => list([{ ID: 'pool-a', Name: 'tank' }, { ID: 'pool-b', Name: 'tank' }]),
      listClusterNodes: async () => list([{ id: 'n1' }, { id: 'n2' }]),
      listClusterPools: async () => { throw new Error('500'); },
    });
    renderStore();
    await waitFor(() => expect(screen.getByTestId('counts').textContent).toBe('1/1/2/2/1'));
  });
});

// 后端返回内容不变时，轮询重读不能产生新引用触发整棵树重渲染。
describe('重读不该把没变的东西换个身份', () => {
  beforeEach(() => stubApi());
  afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

  function Counter({ seen }) {
    const s = useStore();
    seen.push({ tasks: s.tasks, images: s.images });
    return <span data-testid="n">{s.tasks.length}</span>;
  }

  it('内容没变时，消费者拿到的还是同一批数组', async () => {
    const rows = [{ ID: 'task-1', Type: 'import_image', Status: 'running', Progress: 10, CreatedAt: '2026-08-24T00:00:00Z' }];
    stubApi({ listTasks: async () => list(rows) });
    const renders = [];
    render(<StoreProvider><Counter seen={renders}/></StoreProvider>);
    await waitFor(() => expect(screen.getByTestId('n').textContent).toBe('1'));
    const settled = renders[renders.length - 1];

    // 再读一遍，后端返回同样的内容
    await act(async () => { onMutation.emit(); await Promise.resolve(); });
    await act(async () => { await Promise.resolve(); });

    const after = renders[renders.length - 1];
    expect(after.tasks).toBe(settled.tasks);
    expect(after.images).toBe(settled.images);
  });

  it('内容真变了才换身份', async () => {
    let rows = [];
    stubApi({ listTasks: async () => list(rows) });
    const renders = [];
    render(<StoreProvider><Counter seen={renders}/></StoreProvider>);
    await waitFor(() => expect(screen.getByTestId('n').textContent).toBe('0'));
    const empty = renders[renders.length - 1].tasks;

    rows = [{ ID: 'task-1', Type: 'import_image', Status: 'running', Progress: 10, CreatedAt: '2026-08-24T00:00:00Z' }];
    await act(async () => { onMutation.emit(); await Promise.resolve(); });
    await waitFor(() => expect(screen.getByTestId('n').textContent).toBe('1'));
    expect(renders[renders.length - 1].tasks).not.toBe(empty);
  });
});
