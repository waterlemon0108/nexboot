import React from 'react';
import { render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageOverview } from './overview.jsx';

// 总览在集群里必须按集群统计池和容量，不能只报本机。
const NODES = [
  { id: 'n1', ip: '10.0.0.3', ha_state: 'active', online: true },
  { id: 'n2', ip: '10.0.0.4', ha_state: 'standby', online: true },
  { id: 'n3', ip: '10.0.0.5', ha_state: 'standby', online: false },
];

const CLUSTER_POOLS = {
  nodes: [
    { node_id: 'n1', ip: '10.0.0.3', role: 'active', is_self: true, online: true, reachable: true,
      pools: [{ ID: 'p1', Name: 'tank', Capacity: 80 * 2 ** 30, Used: 20 * 2 ** 30, Health: 'ONLINE' }] },
    { node_id: 'n2', ip: '10.0.0.4', role: 'standby', online: true, reachable: true,
      pools: [{ ID: 'p2', Name: 'tank', Capacity: 80 * 2 ** 30, Used: 60 * 2 ** 30, Health: 'ONLINE' }] },
    { node_id: 'n3', ip: '10.0.0.5', role: 'standby', online: false, reachable: false, pools: [], error: '节点失联' },
  ],
  total_capacity: 160 * 2 ** 30, total_used: 80 * 2 ** 30, pool_count: 2, unreachable: 1,
};

function stubApi({ nodes = NODES, clusterPools = CLUSTER_POOLS, terminals = [] } = {}) {
  vi.spyOn(api, 'listTerminals').mockResolvedValue({ items: terminals });
  vi.spyOn(api, 'listGroups').mockResolvedValue({ items: [] });
  vi.spyOn(api, 'listImages').mockResolvedValue({ items: [] });
  vi.spyOn(api, 'listPools').mockResolvedValue({ items: [] });
  vi.spyOn(api, 'listTaskHistory').mockResolvedValue({ items: [] });
  vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: nodes });
  vi.spyOn(api, 'listClusterPools').mockResolvedValue(clusterPools);
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageOverview/></ConfirmProvider></StoreProvider>,
);

afterEach(() => vi.restoreAllMocks());

describe('总览页的集群口径', () => {
  it('存储池统计用集群合计，不是本机那一个', async () => {
    stubApi();
    renderPage();
    const card = (await screen.findAllByText('存储池')).map(el => el.closest('.stat-card')).find(Boolean);
    expect(card).toBeTruthy();
    // 2 个池、合计 160G、已用 80G —— 而不是本机的 1 个池
    expect(within(card).getByText('2')).toBeTruthy();
    expect(within(card).getByText(/80\.0 ?GiB/)).toBeTruthy();
  });

  it('多节点时先告诉运维集群是什么状态', async () => {
    stubApi();
    renderPage();
    expect(await screen.findByText('集群')).toBeTruthy();
    // 一台失联必须直说，不能混在「3 节点」里让人以为都好着
    expect(await screen.findByText(/1 台离线/)).toBeTruthy();
    expect(await screen.findByText(/主机 10\.0\.0\.3/)).toBeTruthy();
  });

  it('单机不摆集群的架子', async () => {
    stubApi({ nodes: [NODES[0]], clusterPools: { ...CLUSTER_POOLS, nodes: [CLUSTER_POOLS.nodes[0]] } });
    renderPage();
    await screen.findAllByText('镜像');
    expect(screen.queryByText('集群')).toBeNull();
  });

  it('每个池标出在哪台，读不到的说明原因', async () => {
    stubApi();
    renderPage();
    const card = (await screen.findByText('存储池容量')).closest('.card');
    expect(within(card).getByText('10.0.0.4')).toBeTruthy();
    expect(within(card).getByText(/节点失联/)).toBeTruthy();
  });

  // 数量要带对象名，单写「0 台」看不出数的是什么。
  it('节点负载数的是终端，就把「终端」写出来', async () => {
    stubApi({ terminals: [
      { ID: 't1', State: 'online', StorageServerID: 'n1' },
      { ID: 't2', State: 'online', StorageServerID: 'n2' },
    ] });
    renderPage();
    const card = (await screen.findByText('节点负载')).closest('.card');
    expect(within(card).getAllByText(/台终端/).length).toBe(3);
  });

  // 近期任务只列几条，免得把容量和镜像面板挤出屏幕。
  it('近期任务只留最近 5 条', async () => {
    const many = Array.from({ length: 12 }, (_, i) => ({
      ID: `k${i}`, Type: 'reduction_create', TargetRef: `testsys_${i}`, Status: 'success', Progress: 100,
    }));
    stubApi();
    vi.spyOn(api, 'listTaskHistory').mockResolvedValue({ items: many });
    renderPage();
    const card = (await screen.findByText('近期任务')).closest('.card');
    await within(card).findByText(/testsys_0/);
    expect(within(card).queryByText(/testsys_5/)).toBeNull();
    expect(within(card).getAllByText(/testsys_/).length).toBe(5);
  });

  // 对象 ID 是中文名折成 ASCII 的数据集名（win11-_default____…），要显示名称。
  it('近期任务显示对象名称而不是数据集 ID', async () => {
    stubApi();
    vi.spyOn(api, 'listTaskHistory').mockResolvedValue({ items: [
      { ID: 'k1', Type: 'reduction_create', TargetRef: 'win11-_default', TargetName: 'Win11 电竞版 / default', Status: 'success', Progress: 100 },
    ] });
    renderPage();
    const card = (await screen.findByText('近期任务')).closest('.card');
    expect(await within(card).findByText(/Win11 电竞版 \/ default/)).toBeTruthy();
    expect(within(card).queryByText(/win11-_default/)).toBeNull();
  });

  // 这三块小面板不能各占一整行，否则总览要滚两屏。
  it('节点负载、存储池容量、镜像排在同一行', async () => {
    stubApi();
    renderPage();
    const load = (await screen.findByText('节点负载')).closest('.card');
    const cap = (await screen.findByText('存储池容量')).closest('.card');
    const img = (await screen.findAllByText('镜像')).map(el => el.closest('.card-title')).find(Boolean).closest('.card');
    expect(load.parentElement).toBe(cap.parentElement);
    expect(img.parentElement).toBe(cap.parentElement);
  });

  it('自动均衡之后，运维第一个问题是每台扛了多少', async () => {
    stubApi({ terminals: [
      { ID: 't1', State: 'online', StorageServerID: 'n1' },
      { ID: 't2', State: 'online', StorageServerID: 'n2' },
      { ID: 't3', State: 'online', StorageServerID: 'n2' },
      { ID: 't4', State: 'offline', StorageServerID: 'n2' },
    ] });
    renderPage();
    const card = (await screen.findByText('节点负载')).closest('.card');
    // 在线的才算负载：t4 离线不占 n2 的容量
    expect(within(card).getByText(/10\.0\.0\.4/)).toBeTruthy();
    const row = within(card).getByText(/10\.0\.0\.4/).closest('div');
    expect(row.parentElement.textContent).toMatch(/2 台/);
  });
});
