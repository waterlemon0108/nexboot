import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageServers } from './servers.jsx';


const STATUS = (over = {}) => ({
  config: { backup_pool: '', enabled: false, schedule: '', ...(over.config || {}) },
  items: over.items || [],
});

function stubApi(status) {
  vi.spyOn(api, 'getServer').mockResolvedValue({ Hostname: 'nd-1', OS: 'Ubuntu', UptimeSeconds: 100 });
  vi.spyOn(api, 'listServices').mockResolvedValue({ items: [] });
  vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(status);
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageServers/></ConfirmProvider></StoreProvider>,
);

const rowOf = async (source) => (await screen.findByText(source)).closest('tr');

// 防止没有失败记录就画成「正常」：只有真正存下副本的行才算正常。
describe('数据备份的每行状态', () => {
  afterEach(() => vi.restoreAllMocks());

  function stubNodes(nodes) {
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'b', ip: '192.168.10.3', ha_state: 'active', online: true },
    ], total: 1 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [] });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue({
      config: { enabled: true, schedule: 'daily@03:00', schedule_text: '每天 03:00' },
      node: {},
    });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({
      nodes, total: nodes.length,
      protected: nodes.filter(n => n.last_backup_at).length,
    });
  }

  const rowOfNode = async (ip) => {
    const panel = await screen.findByTestId('backup-nodes');
    return (await within(panel).findByText(ip)).closest('tr');
  };

  it('没有备份池时不说「正常」，并且点明没有池', async () => {
    const nodes = [{ node_id: 'b', ip: '192.168.10.3', role: 'active', online: true, reachable: true, backup_pool: '' }];
    stubNodes(nodes);
    let resolveBackups;
    api.listClusterBackups.mockReturnValueOnce(new Promise(resolve => { resolveBackups = resolve; }));
    renderPage();
    const rowPending = rowOfNode('192.168.10.3');
    await screen.findByTestId('backup-nodes');
    resolveBackups({ nodes, total: 1, protected: 0 });
    const row = await rowPending;
    expect(within(row).queryByText('正常')).toBeNull();
    expect(within(row).getByText('没有备份池')).toBeTruthy();
    // 空的池名会让整列没有内容，看着像坏了；缺值一律用 — 占位
    expect(row.cells[2].textContent).toBe('—');
  });

  it('有池但一次都没存过，是「待备份」而不是「正常」', async () => {
    stubNodes([{ node_id: 'b', ip: '192.168.10.3', role: 'active', online: true, reachable: true, backup_pool: 'w' }]);
    renderPage();
    const row = await rowOfNode('192.168.10.3');
    expect(within(row).queryByText('正常')).toBeNull();
    expect(within(row).getByText('待备份')).toBeTruthy();
    expect(within(row).getByText('w')).toBeTruthy();
  });

  it('真的存下副本才是「正常」', async () => {
    stubNodes([{ node_id: 'b', ip: '192.168.10.3', role: 'active', online: true, reachable: true,
                 backup_pool: 'w', last_backup_at: '2026-08-23T03:00:00Z' }]);
    renderPage();
    const row = await rowOfNode('192.168.10.3');
    expect(within(row).getByText('正常')).toBeTruthy();
  });

  it('读不到那台的状态就说读不到，不拿旧值充数', async () => {
    stubNodes([{ node_id: 'c', ip: '192.168.10.5', role: 'standby', online: false, reachable: false, error: '节点失联' }]);
    renderPage();
    const row = await rowOfNode('192.168.10.5');
    expect(within(row).getByText('节点失联')).toBeTruthy();
    expect(within(row).queryByText('正常')).toBeNull();
  });
});

describe('集群节点', () => {
  afterEach(() => vi.restoreAllMocks());

  const NODES = (items) => ({ items, total: items.length });

  function stubCluster(items) {
    vi.spyOn(api, 'getServer').mockResolvedValue({ hostname: 'nimblex1', ip: '192.168.10.4', os: 'Ubuntu 24.04' });
    vi.spyOn(api, 'listServices').mockResolvedValue({ items: [] });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(STATUS());
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue(NODES(items));
  }

  it('多节点时列出每一台，并标出谁是主机、谁掉线', async () => {
    stubCluster([
      { id: 'a', name: 'a', ip: '192.168.10.3', portal_ip: '192.168.10.3', ha_state: 'standby', online: true },
      { id: 'b', name: 'b', ip: '192.168.10.4', portal_ip: '192.168.10.250', ha_state: 'active', online: true },
      { id: 'c', name: 'c', ip: '192.168.10.5', portal_ip: '192.168.10.5', ha_state: 'standby', online: false },
    ]);
    renderPage();
    // 规模写在徽章上，不再是写死的「单节点」
    const badge = await screen.findAllByText(/3\s*节点/);
    expect(badge.length).toBeGreaterThan(0);
    expect(screen.queryByText(/单节点/)).toBeNull();
    // 一行一台：节点多了也不会把服务名重复印 N 遍
    const list = await screen.findByTestId('cluster-nodes');
    const rowFor = (ip) => Array.from(list.querySelectorAll('tbody tr'))
      .find(tr => tr.textContent.includes(ip));
    const self = rowFor('192.168.10.4');
    expect(within(self).getByText(/主机/)).toBeTruthy();
    expect(within(rowFor('192.168.10.5')).getByText('离线')).toBeTruthy();
    expect(within(rowFor('192.168.10.3')).getByText(/备机/)).toBeTruthy();
  });

  // 每个服务必须是真列，表头和格子由表格自己对齐。
  it('每个服务是真正的一列，表头和内容对得上', async () => {
    stubCluster([
      { id: 'a', name: 'a', ip: '192.168.10.3', ha_state: 'active', online: true },
      { id: 'b', name: 'b', ip: '192.168.10.4', ha_state: 'standby', online: true },
    ]);
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({
      nodes: [
        { node_id: 'a', ip: '192.168.10.3', role: 'active', reachable: true, services: [
          { key: 'dnsmasq', label: 'dnsmasq', capability: 'DHCP', unit: 'dnsmasq.service', ok: true, state: 'running' },
          { key: 'target', label: 'target', capability: '磁盘服务', unit: 'target.service', ok: true, state: 'running' },
        ] },
        { node_id: 'b', ip: '192.168.10.4', role: 'standby', reachable: true, services: [
          { key: 'dnsmasq', label: 'dnsmasq', capability: 'DHCP', unit: 'dnsmasq.service', ok: true, state: 'inactive', provided: '由主机提供' },
          { key: 'target', label: 'target', capability: '磁盘服务', unit: 'target.service', ok: true, state: 'running' },
        ] },
      ],
    });
    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    await waitFor(() => expect(within(list).getAllByText('DHCP').length).toBeGreaterThan(0));
    const headers = Array.from(list.querySelectorAll('thead th'));
    expect(headers.map(h => h.textContent)).toContain('DHCP');
    // 表头列数 = 每行单元格数，才谈得上对齐
    for (const tr of list.querySelectorAll('tbody tr')) {
      const cells = Array.from(tr.querySelectorAll('td'));
      const span = cells.reduce((n, td) => n + (Number(td.getAttribute('colspan')) || 1), 0);
      expect(span).toBe(headers.length);
    }
  });

  // 出故障的节点往往最难 SSH 上去，控制台要能直接重启它的服务。
  it('可以重启别的节点的服务，并且指名道姓', async () => {
    stubCluster([
      { id: 'a', name: 'a', ip: '192.168.10.3', ha_state: 'active', online: true },
      { id: 'b', name: 'b', ip: '192.168.10.4', ha_state: 'standby', online: true },
    ]);
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({
      nodes: [
        { node_id: 'a', ip: '192.168.10.3', role: 'active', reachable: true, services: [
          { key: 'dnsmasq', label: 'dnsmasq', capability: 'DHCP', unit: 'dnsmasq.service', ok: true, status: 'running', installed: true },
        ] },
        { node_id: 'b', ip: '192.168.10.4', role: 'standby', reachable: true, services: [
          { key: 'target', label: 'target', capability: '磁盘服务', unit: 'target.service', ok: true, status: 'running', installed: true },
        ] },
      ],
    });
    const act = vi.fn().mockResolvedValue({});
    vi.spyOn(api, 'clusterServiceAction').mockImplementation(act);

    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    const rowFor = (ip) => Array.from(list.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes(ip));
    await userEvent.click(rowFor('192.168.10.4'));

    const panel = await screen.findByTestId('node-services');
    await userEvent.click(within(panel).getByRole('button', { name: '重启' }));
    // 危险动作先问一句，问的是哪台机器上的哪个服务
    const dialog = (await screen.findByText('重启服务')).closest('.card');
    expect(dialog.textContent).toContain('192.168.10.4');
    await userEvent.click(within(dialog).getByRole('button', { name: '重启' }));

    await waitFor(() => expect(act).toHaveBeenCalledWith('b', 'target', 'restart'));
  });

  // 报废机器要能移出花名册（否则一直计入多数派分母）；移除不可逆，只对离线节点提供且须核对地址。
  it('可以把已经离线的节点移出集群，且要核对地址', async () => {
    stubCluster([
      { id: 'a', name: 'a', ip: '192.168.10.3', ha_state: 'active', online: true },
      { id: 'c', name: 'c', ip: '192.168.10.5', ha_state: 'standby', online: false },
    ]);
    const forget = vi.fn().mockResolvedValue({});
    vi.spyOn(api, 'forgetClusterNode').mockImplementation(forget);

    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    const rowFor = (ip) => Array.from(list.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes(ip));

    // 在线的那台不给移除入口：移除在线节点几乎总是误操作
    expect(within(rowFor('192.168.10.3')).queryByRole('button', { name: /移除/ })).toBeNull();

    await userEvent.click(within(rowFor('192.168.10.5')).getByRole('button', { name: /移除/ }));
    const dialog = (await screen.findByText(/移出集群/)).closest('.card');
    expect(dialog.textContent).toContain('192.168.10.5');
    await userEvent.type(dialog.querySelector('input'), '192.168.10.5');
    await userEvent.click(within(dialog).getByRole('button', { name: /移除|确认/ }));
    await waitFor(() => expect(forget).toHaveBeenCalledWith('c'));
  });

  // 单机与集群同一条渲染路径，版本等主机信息照常显示。
  it('单机部署走同一条路：表里一行，主机信息照常显示', async () => {
    stubCluster([{ id: 'a', name: 'a', ip: '192.168.10.3', portal_ip: '192.168.10.3', ha_state: 'active', online: true }]);
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [
      { node_id: 'a', ip: '192.168.10.3', role: 'active', online: true, reachable: true,
        host: { hostname: 'only-one', os: 'Ubuntu 24.04', uptime_sec: 60, version: '9.9.9' }, services: [] },
    ]});
    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    expect(list.querySelectorAll('tbody tr').length).toBe(1);
    expect(screen.queryByText(/单节点/)).toBeNull();
    const row = list.querySelector('tbody tr');
    expect(within(row).getByText('9.9.9')).toBeTruthy();
    expect(row.getAttribute('title')).toMatch(/only-one/);
    expect(await screen.findByTestId('node-services')).toBeTruthy();
  });
});

// 按应然状态着色：备机的 dnsmasq 必须停（两个 authoritative DHCP 会互相 NAK），
// target.service 是 oneshot、跑完即退，按「在不在跑」上色会让健康集群满屏告警。
describe('集群服务矩阵', () => {
  afterEach(() => vi.restoreAllMocks());

  const MATRIX = {
    nodes: [
      { node_id: 'b', ip: '192.168.10.4', role: 'active', is_self: true, online: true, reachable: true, services: [
        { key: 'dnsmasq', capability: 'DHCP / PXE 引导', label: 'DNSMASQ', unit: 'dnsmasq.service', status: 'running', expected: 'running', ok: true, provided: true },
        { key: 'iscsi', capability: '客户机磁盘', label: 'iSCSI', unit: 'target.service', status: 'stopped', expected: 'ready', ok: true, provided: true },
      ]},
      { node_id: 'a', ip: '192.168.10.3', role: 'standby', is_self: false, online: true, reachable: true, services: [
        { key: 'dnsmasq', capability: 'DHCP / PXE 引导', label: 'DNSMASQ', unit: 'dnsmasq.service', status: 'stopped', expected: 'stopped', ok: true, provided: false },
        { key: 'iscsi', capability: '客户机磁盘', label: 'iSCSI', unit: 'target.service', status: 'stopped', expected: 'ready', ok: false, provided: false },
      ]},
    ],
  };

  function stubMatrix(matrix) {
    vi.spyOn(api, 'getServer').mockResolvedValue({ hostname: 'nimblex1', ip: '192.168.10.4', os: 'Ubuntu' });
    vi.spyOn(api, 'listServices').mockResolvedValue({ items: [] });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(STATUS());
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'b', ip: '192.168.10.4', portal_ip: '192.168.10.250', ha_state: 'active', online: true },
      { id: 'a', ip: '192.168.10.3', portal_ip: '192.168.10.3', ha_state: 'standby', online: true },
    ], total: 2 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue(matrix);
  }

  it('按「应然状态」着色：备机停掉的 DHCP 不算故障，缺内核态的 iSCSI 才算', async () => {
    stubMatrix(MATRIX);
    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    const rowFor = (ip) => Array.from(list.querySelectorAll('tbody tr'))
      .find(tr => tr.textContent.includes(ip));

    // 每台的服务浓缩成一排状态点，逐个可悬停问「它是什么、现在怎样」
    const standby = rowFor('192.168.10.3');
    expect(within(standby).getByTitle(/DHCP.*由主机提供/s)).toBeTruthy();
    expect(within(standby).getByTitle(/磁盘.*异常/s)).toBeTruthy();
    // 主机那台的磁盘就是「运行中」
    expect(within(rowFor('192.168.10.4')).getAllByText('运行中').length).toBeGreaterThan(0);
    // 有异常的那台在行上直接标出来
    expect(within(standby).getByText(/1 处异常/)).toBeTruthy();
    // 行末的总判定仍是「正常」，格子里是「运行中」
    expect(within(rowFor('192.168.10.4')).getByText('正常')).toBeTruthy();
    // 顶部汇总与出问题的那一行各标一次
    expect(within(list).getAllByText(/1 处异常/).length).toBe(2);
  });

  it('全部符合预期时说「全部就绪」，不数「几个在跑」', async () => {
    const allOK = { nodes: MATRIX.nodes.map(n => ({ ...n, services: n.services.map(s => ({ ...s, ok: true })) })) };
    stubMatrix(allOK);
    renderPage();
    expect(await screen.findByText('全部就绪')).toBeTruthy();
  });

  it('节点不可达时该列标出来，不把它算成服务故障', async () => {
    const off = { nodes: [MATRIX.nodes[0], { ...MATRIX.nodes[1], reachable: false, services: [], error: '节点无响应' }] };
    stubMatrix(off);
    renderPage();
    const list2 = await screen.findByTestId('cluster-nodes');
    const row = Array.from(list2.querySelectorAll('tbody tr'))
      .find(tr => tr.textContent.includes('192.168.10.3'));
    // 那台自己报了原因就显示原因，比笼统的「读取不到」更能指路
    expect(within(row).getByText('节点无响应')).toBeTruthy();
    // 读不到服务状态 ≠ 服务故障，不该计入异常数
    expect(screen.queryByText(/处异常/)).toBeNull();
  });
});

// 钉住服务状态的结论：集群列表与服务区只有一个来源，不能一处绿一处灰。
describe('服务状态的判定', () => {
  afterEach(() => vi.restoreAllMocks());

  function stubOne(services) {
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(STATUS());
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'n1', ip: '192.168.10.4', ha_state: 'active', online: true },
    ], total: 1 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [
      { node_id: 'n1', ip: '192.168.10.4', role: 'active', online: true, reachable: true, services },
    ]});
  }

  it('oneshot 单元说「运行中」，不造一个「就绪」让人琢磨差别', async () => {
    stubOne([{
      key: 'iscsi', capability: '客户机磁盘', label: 'iSCSI Target · LIO', unit: 'target.service',
      status: 'stopped', expected: 'ready', ok: true, provided: true, installed: true, critical: true,
    }]);
    renderPage();
    const row = (await screen.findByText('iSCSI Target · LIO')).closest('tr');
    expect(within(row).getByText('运行中')).toBeTruthy();
    expect(within(row).queryByText('已停止')).toBeNull();
    expect(within(row).queryByText('就绪')).toBeNull();
    await userEvent.click(within(row).getByLabelText('展开详情'));
    expect(await screen.findByText('target.service')).toBeTruthy();
  });

  it('汇总不再数「几个在跑」，而是数几处不符合预期', async () => {
    stubOne([
      { key: 'a', capability: 'A 能力', label: 'A', unit: 'a.service', status: 'running', expected: 'running', ok: true, provided: true, installed: true },
      { key: 'b', capability: 'B 能力', label: 'B', unit: 'b.service', status: 'stopped', expected: 'ready', ok: true, provided: true, installed: true },
    ]);
    renderPage();
    expect(await screen.findByText('全部运行中')).toBeTruthy();
    expect(screen.queryByText(/\d\/\d 运行中/)).toBeNull();
  });
});

describe('节点数变化时的密度', () => {
  afterEach(() => vi.restoreAllMocks());

  const nodeAt = (i) => ({
    id: `n${i}`, ip: `192.168.10.${10 + i}`, portal_ip: `192.168.10.${10 + i}`,
    ha_state: i === 0 ? 'active' : 'standby', online: true,
  });
  const svcOf = (i) => ({
    node_id: `n${i}`, ip: `192.168.10.${10 + i}`, role: i === 0 ? 'active' : 'standby',
    is_self: i === 0, online: true, reachable: true,
    services: [
      { key: 'dnsmasq', capability: 'DHCP / PXE 引导', label: 'DNSMASQ · DHCP/TFTP/PXE', unit: 'dnsmasq.service', status: i === 0 ? 'running' : 'stopped', expected: i === 0 ? 'running' : 'stopped', ok: true, provided: i === 0 },
      { key: 'zfs', capability: '存储', label: 'ZFS · 存储', unit: 'zfs-zed.service', status: 'running', expected: 'running', ok: true, provided: true },
    ],
  });

  it('十个节点也是十行，服务名只在表头出现一次', async () => {
    const n = 10;
    const idx = Array.from({ length: n }, (_, i) => i);
    vi.spyOn(api, 'getServer').mockResolvedValue({ hostname: 'n1', ip: '192.168.10.10', os: 'Ubuntu' });
    vi.spyOn(api, 'listServices').mockResolvedValue({ items: [] });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(STATUS());
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: idx.map(nodeAt), total: n });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: idx.map(svcOf) });
    renderPage();

    const list = await screen.findByTestId('cluster-nodes');
    expect(list.querySelectorAll('tbody tr').length).toBe(n);
    // 服务名在表头列一次，不在每一行重复
    expect(within(list).getAllByText('DHCP / PXE 引导').length).toBe(1);
  });
});

// 版面只展示能力和结论，systemd 实现细节展开才看。
describe('能力视角', () => {
  afterEach(() => vi.restoreAllMocks());

  const items = [
    { key: 'dnsmasq', capability: 'DHCP / PXE 引导', label: 'DNSMASQ · DHCP/TFTP/PXE',
      unit: 'dnsmasq.service', status: 'running', expected: 'running', ok: true, provided: true,
      installed: true, enabled: false, critical: true },
    { key: 'iscsi', capability: '客户机磁盘', label: 'iSCSI Target · LIO',
      unit: 'target.service', status: 'stopped', expected: 'ready', ok: true, provided: true,
      installed: true, enabled: false, critical: true },
  ];

  function stub(list, role = 'active') {
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(STATUS());
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'n1', ip: '192.168.10.3', ha_state: role, online: true },
    ], total: 1 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [
      { node_id: 'n1', ip: '192.168.10.3', role, online: true, reachable: true,
        host: { hostname: 'n1', os: 'Ubuntu', version: 'dev' }, services: list },
    ]});
  }

  it('台面上是服务名 + 能力和结论，不是单元名与「已禁用」', async () => {
    stub(items);
    renderPage();
    // 服务名保留：运维认得的是「iSCSI Target · LIO」这个名字
    expect(await screen.findByText('iSCSI Target · LIO')).toBeTruthy();
    const cap = screen.getByText('iSCSI Target · LIO').closest('.card');
    // 常态下实现细节不出现在版面上；「已禁用」在备份卡片里另有含义，所以只在服务区里查。
    expect(within(cap).queryByText('target.service')).toBeNull();
    expect(within(cap).queryByText('已禁用')).toBeNull();
    expect(within(cap).queryByText('systemd 单元')).toBeNull();
    expect(within(cap).queryByText(/开机跑完即退出/)).toBeNull();
  });

  it('要排障时展开才看到单元、开机自启这些实现细节', async () => {
    stub(items);
    const user = userEvent.setup();
    renderPage();
    await user.click((await screen.findByText('iSCSI Target · LIO')).closest('tr').querySelector('button[aria-label="展开详情"]'));
    expect(await screen.findByText('target.service')).toBeTruthy();
    expect(screen.getByText(/开机自启/)).toBeTruthy();
  });

  it('备机说「由主机提供」，而不是让人猜一个停掉的服务', async () => {
    stub([{ ...items[0], status: 'stopped', expected: 'stopped', ok: true, provided: false }], 'standby');
    renderPage();
    // 表格格子和服务区都会说这句：同一判定，两处呈现，都不该冒出「已停止」
    const svc = await screen.findByTestId('node-services');
    expect(within(svc).getByText('由主机提供')).toBeTruthy();
    expect(screen.queryByText('已停止')).toBeNull();
  });
});

// 「本机」只是此刻持有虚 IP 的机器，页面改为在表里选一台、下面显示它。
describe('服务器页以节点为中心', () => {
  afterEach(() => vi.restoreAllMocks());

  const HOSTS = {
    nodes: [
      { node_id: 'b', ip: '192.168.10.4', role: 'active', is_self: true, online: true, reachable: true,
        host: { hostname: 'nimblex2', os: 'Ubuntu 24.04.1 LTS', uptime_sec: 111600, version: 'dev' },
        services: [{ key: 'dnsmasq', capability: 'DHCP / PXE 引导', label: 'DNSMASQ', unit: 'dnsmasq.service', status: 'running', expected: 'running', ok: true, installed: true }] },
      { node_id: 'a', ip: '192.168.10.3', role: 'standby', is_self: false, online: true, reachable: true,
        host: { hostname: 'nimblex1', os: 'Debian 12', uptime_sec: 3600, version: '1.2.3' },
        services: [{ key: 'dnsmasq', capability: 'DHCP / PXE 引导', label: 'DNSMASQ', unit: 'dnsmasq.service', status: 'stopped', expected: 'stopped', ok: true, installed: true }] },
    ],
  };

  function stubHosts() {
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue(STATUS());
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'b', ip: '192.168.10.4', ha_state: 'active', online: true },
      { id: 'a', ip: '192.168.10.3', ha_state: 'standby', online: true },
    ], total: 2 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue(HOSTS);
  }

  it('没有「本机」这个说法，默认选中主机并显示它的服务', async () => {
    stubHosts();
    renderPage();
    await screen.findByTestId('cluster-nodes');
    // 「本机信息」卡和「本机」标记都不该再出现
    expect(screen.queryByText('本机信息')).toBeNull();
    expect(screen.queryByText('本机')).toBeNull();
    // 服务区的标题写死是哪一台：重启按的是它，不能靠「当前高亮那行」意会
    const svc = await screen.findByTestId('node-services');
    expect(within(svc).getAllByText(/192\.168\.10\.4/).length).toBeGreaterThan(0);
    expect(within(svc).getAllByText(/主机/).length).toBeGreaterThan(0);
  });

  it('选另一台，服务区就跟过去——标题写死是哪台', async () => {
    stubHosts();
    renderPage();
    const svc = await screen.findByTestId('node-services');
    expect(within(svc).getAllByText(/192\.168\.10\.4/).length).toBeGreaterThan(0);

    const list = screen.getByTestId('cluster-nodes');
    const other = Array.from(list.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes('192.168.10.3'));
    await userEvent.click(other);
    await waitFor(() => expect(
      within(screen.getByTestId('node-services')).getAllByText(/192\.168\.10\.3/).length).toBeGreaterThan(0));
    // 换过去之后，操作打的就是这台：标题里的角色也跟着变
    expect(within(screen.getByTestId('node-services')).getAllByText(/备机/).length).toBeGreaterThan(0);
  });

  it('版本和已运行在节点表里就能比，不必逐台点开', async () => {
    stubHosts();
    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    const rowFor = (ip) => Array.from(list.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes(ip));
    expect(within(rowFor('192.168.10.4')).getByText('dev')).toBeTruthy();
    expect(within(rowFor('192.168.10.3')).getByText('1.2.3')).toBeTruthy();
    // 已运行要能横向比较：一台刚起来而别人没动过，说明它自己重启了。
    expect(within(rowFor('192.168.10.4')).getByText(/1\s*天/)).toBeTruthy();
    expect(within(rowFor('192.168.10.3')).getByText(/1\s*小时/)).toBeTruthy();
  });

  // 主机名和系统只进悬停，不在服务区占一行。
  it('服务区不再挂主机信息条', async () => {
    stubHosts();
    renderPage();
    const svc = await screen.findByTestId('node-services');
    expect(within(svc).queryByText('主机名')).toBeNull();
    expect(within(svc).queryByText('操作系统')).toBeNull();
    expect(within(svc).queryByText('ndiskless 版本')).toBeNull();
    // 但不是丢掉：挂在节点行的悬停里
    const list = screen.getByTestId('cluster-nodes');
    const row = Array.from(list.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes('192.168.10.4'));
    expect(row.getAttribute('title')).toMatch(/nimblex2/);
    expect(row.getAttribute('title')).toMatch(/Ubuntu/);
  });

  it('节点行不再有「详情」按钮，也不再挂那段说明', async () => {
    stubHosts();
    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    expect(within(list).queryByRole('button', { name: '详情' })).toBeNull();
    expect(screen.queryByText(/回退到主机/)).toBeNull();
    expect(screen.queryByText(/备机不跑 DHCP 是对的/)).toBeNull();
  });
});

// 备份按节点逐台列出副本；备份池不手填（另一个池就是备份池），计划不用 cron（后端只收时刻）。
describe('数据备份以节点为单位', () => {
  afterEach(() => vi.restoreAllMocks());

  const BACKUPS = {
    total: 3, protected: 2,
    nodes: [
      { node_id: 'b', ip: '192.168.10.3', role: 'active', online: true, reachable: true,
        backup_pool: 'w', last_backup_at: '2026-08-23T03:00:00Z', pending: false },
      { node_id: 'a', ip: '192.168.10.4', role: 'standby', online: true, reachable: true,
        backup_pool: 'w2', last_backup_at: '2026-08-22T03:00:00Z', pending: true },
      { node_id: 'c', ip: '192.168.10.5', role: 'standby', online: true, reachable: true,
        backup_pool: '', pending: false },
    ],
  };

  function stubBackups() {
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'b', ip: '192.168.10.3', ha_state: 'active', online: true },
    ], total: 1 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [] });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue({
      config: { enabled: true, schedule: 'daily@03:00', schedule_text: '每天 03:00',
                next_run_at: '2026-08-24T03:00:00Z', last_run_at: '2026-08-23T03:00:00Z' },
      node: { backup_pool: 'w', last_backup_at: '2026-08-23T03:00:00Z', pending: false },
    });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue(BACKUPS);
  }

  it('正在送副本的节点显示「备份中」，不和没开始的混在一起', async () => {
    stubBackups();
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ ...BACKUPS, nodes: [
      { ...BACKUPS.nodes[0] },
      { ...BACKUPS.nodes[1], copying: true },
      { node_id: 'c', ip: '192.168.10.5', role: 'standby', online: true, reachable: true, backup_pool: 'backup', pending: true, copying: true },
    ] });
    renderPage();
    const first = (await screen.findByText('192.168.10.4')).closest('tr');
    expect(within(first).getByText('备份中')).toBeTruthy();
    const never = screen.getByText('192.168.10.5').closest('tr');
    expect(within(never).getByText('备份中')).toBeTruthy();
    expect(within(never).queryByText('待备份')).toBeNull();
  });

  it('不再让人手填备份目标池——那是机器已经知道的事', async () => {
    stubBackups();
    renderPage();
    await screen.findByText(/数据备份/);
    expect(screen.queryByLabelText('备份目标池')).toBeNull();
    expect(screen.queryByPlaceholderText('backup')).toBeNull();
  });

  it('不再假装支持 cron，用「每天几点」说话', async () => {
    stubBackups();
    renderPage();
    await screen.findByText(/数据备份/);
    expect(screen.queryByText(/cron/i)).toBeNull();
    expect(screen.queryByPlaceholderText('0 3 * * *')).toBeNull();
    expect(await screen.findByLabelText('时刻')).toBeTruthy();
  });

  it('一台一行：谁存着副本、存的什么时候、谁还没有地方存', async () => {
    stubBackups();
    renderPage();
    const panel = await screen.findByTestId('backup-nodes');
    const rowFor = (ip) => Array.from(panel.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes(ip));

    expect(within(rowFor('192.168.10.3')).getByText('w')).toBeTruthy();
    // 欠着一轮的那台要说出来，而不是画成正常
    expect(within(rowFor('192.168.10.4')).getByText(/待同步|落后/)).toBeTruthy();
    // 没有备份池的那台不能画成正常
    expect(within(rowFor('192.168.10.5')).getByText(/没有备份池/)).toBeTruthy();
    expect(within(rowFor('192.168.10.5')).queryByText('正常')).toBeNull();
    // 表里已逐台说明，上面不再挂只有一枚标签的横幅。
    expect(within(panel).queryByText(/台存有副本/)).toBeNull();
  });
});

describe('版面继续收敛', () => {
  afterEach(() => vi.restoreAllMocks());

  function stub(nodes, services) {
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: nodes, total: nodes.length });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: services });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue({
      config: { enabled: true, schedule: 'daily@03:00', schedule_text: '每天 03:00',
                next_run_at: '2026-08-24T03:00:00Z', next_run_text: '2026-08-24 03:00' },
      node: {},
    });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getRuntime').mockResolvedValue({ portal_ip: '192.168.10.250' });
  }

  it('虚 IP 写在集群卡的标题上，主机行下面不再重复', async () => {
    stub([{ id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true, portal_ip: '192.168.10.250' }], []);
    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    expect(within(list).getByText(/虚 IP\s+192\.168\.10\.250/)).toBeTruthy();
    expect(screen.queryByText('持有虚 IP')).toBeNull();
  });

  // 备机的 dnsmasq 必须停着（两个 authoritative 的 DHCP 会互相拒绝客户机），
  // systemctl restart 会把它拉起来，所以不给重启按钮。
  it('该停着的服务不给重启按钮', async () => {
    stub(
      [{ id: 'b', ip: '192.168.10.5', ha_state: 'standby', online: true }],
      [{ node_id: 'b', ip: '192.168.10.5', role: 'standby', online: true, reachable: true, services: [
        { key: 'dnsmasq', capability: 'DHCP', label: 'DNSMASQ · DHCP/TFTP/PXE', unit: 'dnsmasq.service',
          status: 'stopped', expected: 'stopped', ok: true, provided: false, installed: true },
        { key: 'zfs', capability: '存储', label: 'ZFS · 存储', unit: 'zfs-zed.service',
          status: 'running', expected: 'running', ok: true, provided: true, installed: true },
      ]}],
    );
    renderPage();
    const svc = await screen.findByTestId('node-services');
    const rowOf = (name) => Array.from(svc.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes(name));
    expect(within(rowOf('DNSMASQ')).queryByRole('button', { name: '重启' })).toBeNull();
    // 该跑的服务照旧可以重启
    expect(within(rowOf('ZFS')).getByRole('button', { name: '重启' })).toBeTruthy();
  });

  // 备机的 dnsmasq 若真的在跑就是错的，要能停掉它。
  it('该停着却在跑的服务，给的是「停止」不是「重启」', async () => {
    stub(
      [{ id: 'b', ip: '192.168.10.5', ha_state: 'standby', online: true }],
      [{ node_id: 'b', ip: '192.168.10.5', role: 'standby', online: true, reachable: true, services: [
        { key: 'dnsmasq', capability: 'DHCP', label: 'DNSMASQ · DHCP/TFTP/PXE', unit: 'dnsmasq.service',
          status: 'running', expected: 'stopped', ok: false, provided: false, installed: true },
      ]}],
    );
    renderPage();
    const svc = await screen.findByTestId('node-services');
    const row = Array.from(svc.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes('DNSMASQ'));
    expect(within(row).queryByRole('button', { name: '重启' })).toBeNull();
    expect(within(row).getByRole('button', { name: '停止' })).toBeTruthy();
  });

  // 启用开关并进频率：未保存时亮着「已启用」会误导。
  it('用一个选择表达计划，没有单独的启用开关', async () => {
    stub([{ id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true }], []);
    renderPage();
    await screen.findByText(/数据备份/);
    expect(screen.queryByRole('button', { name: '已启用' })).toBeNull();
    expect(screen.queryByRole('button', { name: '已禁用' })).toBeNull();
    const sel = screen.getByLabelText('备份计划');
    expect(Array.from(sel.options).map(o => o.textContent)).toEqual(['禁用', '每天', '每周']);
  });

  // 改动按保存才生效；没有改动时保存按钮置灰。
  it('改动要保存才生效，按钮自己说明有没有待保存的改动', async () => {
    stub([{ id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true }], []);
    const save = vi.fn().mockResolvedValue({});
    vi.spyOn(api, 'saveBackupConfig').mockImplementation(save);
    renderPage();
    await screen.findByText(/数据备份/);

    expect(screen.getByRole('button', { name: /保存/ }).disabled).toBe(true);

    await userEvent.selectOptions(screen.getByLabelText('备份计划'), 'off');
    expect(save).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.getByRole('button', { name: /保存/ }).disabled).toBe(false));
    // 禁用就没有时刻可填——这一格跟着消失，而不是灰在那里让人猜
    expect(screen.queryByLabelText('时刻')).toBeNull();

    // 改回去：又没什么可保存了
    await userEvent.selectOptions(screen.getByLabelText('备份计划'), 'daily');
    await waitFor(() => expect(screen.getByRole('button', { name: /保存/ }).disabled).toBe(true));
    expect(save).not.toHaveBeenCalled();

    // 真的要改，按保存才下发
    await userEvent.selectOptions(screen.getByLabelText('备份计划'), 'weekly');
    await userEvent.click(screen.getByRole('button', { name: /保存/ }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(
      expect.objectContaining({ enabled: true, schedule: expect.stringMatching(/^weekly@/) })));
  });

  it('选「禁用」保存下去的是关闭，而不是一个空计划', async () => {
    stub([{ id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true }], []);
    const save = vi.fn().mockResolvedValue({});
    vi.spyOn(api, 'saveBackupConfig').mockImplementation(save);
    renderPage();
    await screen.findByText(/数据备份/);
    await userEvent.selectOptions(screen.getByLabelText('备份计划'), 'off');
    await userEvent.click(screen.getByRole('button', { name: /保存/ }));
    // 时刻仍旧带上：下次再开的时候还是原来那个点，不必重填
    await waitFor(() => expect(save).toHaveBeenCalledWith({ enabled: false, schedule: 'daily@03:00' }));
  });

  // 下拉默认 width:100%，不限宽三个控件会被挤成三行。
  it('这排控件占一行，下拉不撑满', async () => {
    stub([{ id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true }], []);
    renderPage();
    await screen.findByText(/数据备份/);
    const sel = screen.getByLabelText('备份计划');
    expect(sel.style.width).not.toBe('100%');
    const row = screen.getByTestId('backup-controls');
    expect(row.style.flexWrap).not.toBe('wrap');
  });

  it('备份的表单不再挂标签文字', async () => {
    stub([{ id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true }], []);
    renderPage();
    await screen.findByText(/数据备份/);
    expect(screen.queryByText('定时备份')).toBeNull();
    expect(screen.queryByText('什么时候跑')).toBeNull();
    // 右边只补控件算不出来的「下次」，按服务器的钟显示：备份在那台机器上跑。
    const line = screen.getByTestId('backup-controls');
    expect(line.textContent).toMatch(/下次 2026-08-24 03:00/);
    expect(line.textContent.match(/每天/g).length).toBe(1);
    expect(within(line).getByRole('button', { name: /保存/ })).toBeTruthy();
  });
});

describe('只读服务的说明挂在它自己身上', () => {
  afterEach(() => vi.restoreAllMocks());

  function stub() {
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true },
    ], total: 1 });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [
      { node_id: 'a', ip: '192.168.10.3', role: 'active', online: true, reachable: true, services: [
        { key: 'zfs', capability: '存储', label: 'ZFS · 存储', unit: 'zfs-zed.service',
          status: 'running', expected: 'running', ok: true, installed: true },
        { key: 'ndiskless', capability: '控制服务', label: 'ndiskless · 控制/引导', unit: 'ndiskless.service',
          status: 'running', expected: 'running', ok: true, installed: true, read_only: true },
      ]},
    ]});
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue({ config: {}, node: {} });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
  }

  // 说明要点开：悬停没有可点的样子，原生 title 还要等一两秒才浮出来。
  it('说明点开才出现，表格底下不再挂一行小字', async () => {
    stub();
    renderPage();
    const svc = await screen.findByTestId('node-services');
    const rowOf = (name) => Array.from(svc.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes(name));

    // 没点之前，页面上任何地方都没有这句话
    expect(screen.queryByText(/控制服务自身只读/)).toBeNull();

    const mark = within(rowOf('ndiskless')).getByRole('button', { name: /说明$/ });
    await userEvent.click(mark);
    const tip = await screen.findByText(/控制服务自身只读/);
    expect(tip.textContent).toMatch(/systemctl restart ndiskless/);

    // 再点一次收起
    await userEvent.click(mark);
    await waitFor(() => expect(screen.queryByText(/控制服务自身只读/)).toBeNull());

    // 能重启的服务不挂这个问号
    expect(within(rowOf('ZFS')).queryByRole('button', { name: /说明$/ })).toBeNull();
  });

  it('点页面别处也收起，不用回去再点一次', async () => {
    stub();
    renderPage();
    const svc = await screen.findByTestId('node-services');
    const row = Array.from(svc.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes('ndiskless'));
    await userEvent.click(within(row).getByRole('button', { name: /说明$/ }));
    await screen.findByText(/控制服务自身只读/);
    await userEvent.click(screen.getByTestId('tip-backdrop'));
    await waitFor(() => expect(screen.queryByText(/控制服务自身只读/)).toBeNull());
  });

  // 矩阵未返回时每个节点 reachable 都是 undefined，不能画成「读取不到」。
  it('矩阵还没回来时说「读取中」，不说「读取不到」', async () => {
    let release;
    const held = new Promise(r => { release = r; });
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({ items: [
      { id: 'a', ip: '192.168.10.3', ha_state: 'active', online: true },
      { id: 'b', ip: '192.168.10.4', ha_state: 'standby', online: true },
    ], total: 2 });
    vi.spyOn(api, 'listClusterServices').mockImplementation(async () => {
      await held;
      return { nodes: [
        { node_id: 'a', ip: '192.168.10.3', role: 'active', online: true, reachable: true, services: [] },
        { node_id: 'b', ip: '192.168.10.4', role: 'standby', online: true, reachable: true, services: [] },
      ]};
    });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue({ config: {}, node: {} });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });

    renderPage();
    const list = await screen.findByTestId('cluster-nodes');
    // 矩阵还在路上：不能说「读取不到」
    expect(within(list).queryByText(/读取不到/)).toBeNull();
    expect(within(list).getAllByText(/读取中/).length).toBeGreaterThan(0);

    release();
    await waitFor(() => expect(within(screen.getByTestId('cluster-nodes')).queryByText(/读取中/)).toBeNull());
  });
});

describe('集群节点 · 添加节点', () => {
  afterEach(() => vi.restoreAllMocks());

  function stubCluster(candidates, adopt) {
    vi.spyOn(api, 'getServer').mockResolvedValue({ Hostname: 'nd-1', OS: 'Ubuntu', UptimeSeconds: 100 });
    vi.spyOn(api, 'listServices').mockResolvedValue({ items: [] });
    vi.spyOn(api, 'listClusterBackups').mockResolvedValue({ nodes: [], total: 0, protected: 0 });
    vi.spyOn(api, 'getBackupStatus').mockResolvedValue({ config: {}, items: [] });
    vi.spyOn(api, 'listClusterNodes').mockResolvedValue({
      items: [{ id: 'a1', ip: '192.168.10.3', portal_ip: '192.168.10.250', ha_state: 'active', online: true }],
      total: 1,
    });
    vi.spyOn(api, 'listClusterServices').mockResolvedValue({ nodes: [] });
    vi.spyOn(api, 'discoverNodes').mockResolvedValue({ items: candidates, total: candidates.length });
    vi.spyOn(api, 'adoptNode').mockImplementation(adopt);
  }

  it('发现出厂态节点并一键纳管，全程不填虚 IP 和令牌', async () => {
    const adopt = vi.fn(async () => ({ status: 'adopting' }));
    stubCluster([{ node_id: 'fresh-b', version: 'v1', address: '192.168.10.4:8080' }], adopt);
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /添加节点/ }));
    expect(await screen.findByText(/192\.168\.10\.4:8080/)).toBeTruthy();

    const dialog = screen.getByTestId('adopt-nodes');
    expect(dialog.querySelectorAll('input[type="text"]').length).toBe(0); // 没有任何要填的字段
    expect(dialog.textContent).not.toContain('池名要与集群一致'); // 各节点池名自己定
    expect(dialog.textContent).toContain('存储池管理');
    await user.click(within(dialog).getByRole('button', { name: /加入集群|纳管/ }));

    await waitFor(() => expect(adopt).toHaveBeenCalledWith({ address: '192.168.10.4:8080' }));
  });

  it('没发现候选时说明原因，不留一个空白列表', async () => {
    stubCluster([], vi.fn());
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /添加节点/ }));
    expect(await screen.findByText(/没有发现|未发现/)).toBeTruthy();
  });
});
