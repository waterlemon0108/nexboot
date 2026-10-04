import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageStorage } from './storage.jsx';

// 销毁会带走池上全部镜像、配置、还原点和克隆且无法恢复，销毁与建池的护栏都要有测试。

const list = (items) => ({ items, total: items.length });
const POOLS = [
  { ID: 'pool-tank', Name: 'tank', Health: 'ONLINE', Capacity: 500e9, Used: 200e9, Disks: ['sda'], ReadCacheDisks: [], WriteCacheDisks: [], Layout: 'stripe', GroupWidth: 1 },
  { ID: 'pool-cold', Name: 'cold', Health: 'DEGRADED', Capacity: 100e9, Used: 10e9, Disks: ['/dev/sdb', '/dev/sdd', '/dev/sde', '/dev/sdf'], ReadCacheDisks: [], WriteCacheDisks: [],
    Layout: 'mirror', GroupWidth: 2,
    Groups: [
      { Name: 'mirror-0', Kind: 'mirror', Role: 'data', Status: 'DEGRADED', Disks: [
        { Path: '/dev/sdb', Role: 'data', Status: 'ONLINE', Vdev: 'mirror-0' },
        { Path: '/dev/sdd', Role: 'data', Status: 'OFFLINE', Vdev: 'mirror-0' } ] },
      { Name: 'mirror-1', Kind: 'mirror', Role: 'data', Status: 'ONLINE', Disks: [
        { Path: '/dev/sde', Role: 'data', Status: 'ONLINE', Vdev: 'mirror-1' },
        { Path: '/dev/sdf', Role: 'data', Status: 'ONLINE', Vdev: 'mirror-1' } ] },
      { Name: '/dev/sdz', Kind: 'disk', Role: 'spare', Status: 'AVAIL', Disks: [ { Path: '/dev/sdz', Role: 'spare', Status: 'AVAIL', Vdev: '' } ] },
    ],
    DiskItems: [
      { Path: '/dev/sdb', Role: 'data', Status: 'ONLINE', Vdev: 'mirror-0' }, { Path: '/dev/sdd', Role: 'data', Status: 'OFFLINE', Vdev: 'mirror-0' },
      { Path: '/dev/sde', Role: 'data', Status: 'ONLINE', Vdev: 'mirror-1' }, { Path: '/dev/sdf', Role: 'data', Status: 'ONLINE', Vdev: 'mirror-1' },
      { Path: '/dev/sdz', Role: 'spare', Status: 'AVAIL', Vdev: '' } ] },
];
const RAIDZ_POOL = { ID: 'pool-backup', Name: 'backup', Health: 'ONLINE', Capacity: 400e9, Used: 40e9, Disks: ['/dev/sdj', '/dev/sdk', '/dev/sdl', '/dev/sdm'], ReadCacheDisks: [], WriteCacheDisks: [],
  Layout: 'raidz2', GroupWidth: 4,
  Groups: [ { Name: 'raidz2-0', Kind: 'raidz2', Role: 'data', Status: 'ONLINE', Disks: ['/dev/sdj', '/dev/sdk', '/dev/sdl', '/dev/sdm'].map(p => ({ Path: p, Role: 'data', Status: 'ONLINE', Vdev: 'raidz2-0' })) } ],
  DiskItems: ['/dev/sdj', '/dev/sdk', '/dev/sdl', '/dev/sdm'].map(p => ({ Path: p, Role: 'data', Status: 'ONLINE', Vdev: 'raidz2-0' })) };
const DISKS = [
  { Path: '/dev/sdc', Name: 'sdc', Size: 500e9, InUse: false },
  { Path: '/dev/sdg', Name: 'sdg', Size: 500e9, InUse: false },
  { Path: '/dev/sdh', Name: 'sdh', Size: 500e9, InUse: false },
  { Path: '/dev/sdi', Name: 'sdi', Size: 400e9, InUse: false },
  { Path: '/dev/sda', Name: 'sda', Size: 500e9, InUse: true },
];

function stubApi(overrides = {}) {
  const base = {
    listPools: async () => list(POOLS),
    listDisks: async () => list(DISKS),
    listImages: async () => list([]),
    listGroups: async () => list([]),
    listTerminals: async () => list([]),
    listTasks: async () => list([]),
    listAlarms: async () => list([]),
    listClusterNodes: async () => list([]),
    listClusterPools: async () => ({ nodes: [], total_capacity: 0, total_used: 0, pool_count: 0 }),
    listClusterDisks: async () => list(DISKS),
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageStorage/></ConfirmProvider></StoreProvider>,
);

// 集群下要列出每台节点的池（容量和健康只有池所在节点读得出），服务器列写 IP 而不是节点 ID 哈希。
describe('存储页的集群口径', () => {
  const CLUSTER_NODES = [
    { id: 'n1', ip: '192.168.10.3', ha_state: 'active', online: true },
    { id: 'n2', ip: '192.168.10.4', ha_state: 'standby', online: true },
  ];
  const CLUSTER_POOLS = {
    nodes: [
      { node_id: 'n1', ip: '192.168.10.3', role: 'active', is_self: true, online: true, reachable: true,
        pools: [{ ...POOLS[0], ServerID: 'n1' }] },
      { node_id: 'n2', ip: '192.168.10.4', role: 'standby', online: true, reachable: true,
        pools: [{ ID: 'pool-peer', Name: 'tank', Health: 'ONLINE', Capacity: 800e9, Used: 100e9,
          Disks: ['/dev/sdb'], ReadCacheDisks: [], WriteCacheDisks: [], Layout: 'stripe', GroupWidth: 1, ServerID: 'n2' }] },
    ],
    total_capacity: 1300e9, total_used: 300e9, pool_count: 2, unreachable: 0,
  };
  const stubCluster = (over = {}) => stubApi({
    listClusterNodes: async () => list(CLUSTER_NODES),
    listClusterPools: async () => CLUSTER_POOLS,
    ...over,
  });
  afterEach(() => vi.restoreAllMocks());

  it('列出所有节点的池，服务器列写 IP 不写哈希', async () => {
    stubCluster();
    renderPage();
    // 等对端那台出现在表里；只等「行数≥2」会被加载态的占位行骗过，导致偶发失败。
    const table = await screen.findByRole('table');
    await waitFor(() => expect(within(table).getByText('192.168.10.4')).toBeTruthy());
    expect(table.textContent).not.toContain('n2');
  });

  // 有节点读不到时总容量偏小，会误导扩容决策；钉住的是数字旁注明不完整，不是某种排版。
  it('有节点读不到时，容量数字旁注明不完整', async () => {
    stubCluster({
      listClusterPools: async () => ({
        nodes: [
          CLUSTER_POOLS.nodes[0],
          { node_id: 'n2', ip: '192.168.10.4', role: 'standby', online: false, reachable: false, pools: [], error: '节点失联' },
        ],
        total_capacity: 500e9, total_used: 200e9, pool_count: 1, unreachable: 1,
      }),
    });
    renderPage();
    // 两个受影响的数字各自带上注解，且说清有几台没算进去
    const capCard = (await screen.findByText('总容量')).closest('.stat-card');
    expect(capCard.textContent).toMatch(/不含.*1 台/);
    const usedCard = (await screen.findByText('已使用')).closest('.stat-card');
    expect(usedCard.textContent).toMatch(/不含.*1 台/);
    // 节点那张卡说清有几台读不到
    const poolCard = (await screen.findByText('存储池')).closest('.stat-card');
    expect(poolCard.textContent).toMatch(/1 台读不到/);
    // 不再为此单开一块横幅
    expect(screen.queryByText(/上面的总容量与使用率/)).toBeNull();
  });

  // 在别的节点建池要列那台自己的空闲盘：同名设备在那台上可能是系统盘或不存在。
  it('建池选了别的节点，盘列表就换成那台的', async () => {
    const create = vi.fn().mockResolvedValue({});
    stubCluster({ listClusterDisks: async (node) => list(
      node === 'n2' ? [{ Path: '/dev/sdx', Name: 'sdx', Size: 900e9, InUse: false }] : DISKS) });
    vi.spyOn(api, 'clusterCreatePool').mockImplementation(create);
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /创建存储池|新建存储池/ }));
    const dlg = (await screen.findByText('池名称')).closest('.card');
    const nodeSel = within(dlg).getByLabelText(/建在哪台/);
    await userEvent.selectOptions(nodeSel, 'n2');
    await waitFor(() => expect(within(dlg).getByText('/dev/sdx')).toBeTruthy());
    expect(within(dlg).queryByText('/dev/sdc')).toBeNull();
  });

  // 每台一个数据池、一个备份池：类型必填，已有的那种不能再选，两种都有就建不了。
  const withPeerPools = (peerPools) => stubCluster({
    listClusterPools: async () => ({ ...CLUSTER_POOLS, nodes: [
      { ...CLUSTER_POOLS.nodes[0], pools: [{ ...POOLS[0], ServerID: 'n1', Role: 'data' }] },
      { ...CLUSTER_POOLS.nodes[1], pools: peerPools },
    ] }),
    listClusterDisks: async () => list([{ Path: '/dev/sdx', Name: 'sdx', Size: 900e9, InUse: false }]),
  });
  const openCreateDialog = async () => {
    await userEvent.click(await screen.findByRole('button', { name: /创建存储池/ }));
    return (await screen.findByText('池名称')).closest('.card');
  };
  const optionsOf = (sel) => [...sel.querySelectorAll('option')].map(o => o.textContent);

  it('已有数据池的节点只能建备份池', async () => {
    const create = vi.fn(async () => ({ task_id: 't' }));
    withPeerPools([]);
    vi.spyOn(api, 'createPool').mockImplementation(create);
    renderPage();
    const dlg = await openCreateDialog();
    const typeSel = within(dlg).getByLabelText('存储池类型');
    expect(optionsOf(typeSel)).toEqual(['备份池']);
    expect(within(dlg).getByText(/这台已有数据池 tank，只能建备份池/)).toBeTruthy();
    await userEvent.type(within(dlg).getByPlaceholderText('tank2'), 'bak');
    await userEvent.click(within(dlg).getByRole('button', { name: /^条带/ }));
    await userEvent.click(await within(dlg).findByText('/dev/sdc'));
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    await userEvent.click(await screen.findByRole('button', { name: '仍要创建' }));
    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({ name: 'bak', role: 'backup' })));
    expect(screen.queryByText(/自动重启服务/)).toBeNull();
  });

  // 建在别处且是条带：两件事一次说完，只确认一次。
  it('建在别处的条带池只确认一次', async () => {
    const create = vi.fn(async () => ({ task_id: 't' }));
    withPeerPools([]);
    vi.spyOn(api, 'clusterCreatePool').mockImplementation(create);
    renderPage();
    const dlg = await openCreateDialog();
    await userEvent.selectOptions(within(dlg).getByLabelText(/建在哪台/), 'n2');
    await userEvent.selectOptions(within(dlg).getByLabelText('存储池类型'), 'data');
    await userEvent.type(within(dlg).getByPlaceholderText('tank2'), 'data');
    await userEvent.click(within(dlg).getByRole('button', { name: /^条带/ }));
    await userEvent.click(await within(dlg).findByText('/dev/sdx'));
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    const msg = await screen.findByText(/确认以条带布局创建/);
    expect(msg.textContent).toBe('将在 192.168.10.4 上创建存储池 data，使用磁盘：/dev/sdx。\n'
      + '条带布局无冗余：任一磁盘故障，data 中的全部镜像、配置、还原点及客户机克隆将丢失且无法恢复。确认以条带布局创建？');
    await userEvent.click(screen.getByRole('button', { name: '仍要创建' }));
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(screen.queryByText(/在其他服务器上创建/)).toBeNull();
  });

  it('两种都没有的节点任选一种，必须选', async () => {
    withPeerPools([]);
    renderPage();
    const dlg = await openCreateDialog();
    await userEvent.selectOptions(within(dlg).getByLabelText(/建在哪台/), 'n2');
    const typeSel = within(dlg).getByLabelText('存储池类型');
    expect(optionsOf(typeSel)).toEqual(['请选择', '数据池', '备份池']);
    const submit = screen.getByRole('button', { name: '创建' });
    expect(submit.disabled).toBe(true);
    await userEvent.selectOptions(typeSel, 'data');
    expect(submit.disabled).toBe(false);
  });

  it('两种都有的节点，类型置灰、不能创建', async () => {
    withPeerPools([
      { ID: 'pool-d', Name: 'tank', Role: 'data', Health: 'ONLINE', Capacity: 1, Used: 0, Disks: ['/dev/sdb'], ReadCacheDisks: [], WriteCacheDisks: [], ServerID: 'n2' },
      { ID: 'pool-b', Name: 'bak', Role: 'backup', Health: 'ONLINE', Capacity: 1, Used: 0, Disks: ['/dev/sdc'], ReadCacheDisks: [], WriteCacheDisks: [], ServerID: 'n2' },
    ]);
    renderPage();
    const dlg = await openCreateDialog();
    await userEvent.selectOptions(within(dlg).getByLabelText(/建在哪台/), 'n2');
    expect(within(dlg).getByLabelText('存储池类型').disabled).toBe(true);
    expect(within(dlg).getByText(/这台已有数据池 tank 和备份池 bak/)).toBeTruthy();
    expect(screen.getByRole('button', { name: '创建' }).disabled).toBe(true);
  });

  // 节点 ID 是 machine-id 的哈希，详情里也写 IP。
  it('池详情里的服务器写 IP', async () => {
    stubCluster();
    renderPage();
    const rows = await screen.findAllByText('tank');
    await userEvent.click(rows[rows.length - 1]);
    const dlg = (await screen.findByText(/存储池详情/)).closest('.card');
    expect(within(dlg).getByText('192.168.10.4')).toBeTruthy();
    expect(within(dlg).queryByText('n2')).toBeNull();
  });

  // 对别的节点的池操作，后果在另一台机器上，确认时要点明目标节点。
  it('对别的节点的池动手，要先核对目标节点', async () => {
    const destroy = vi.fn().mockResolvedValue({});
    stubCluster();
    vi.spyOn(api, 'clusterDestroyPool').mockImplementation(destroy);
    renderPage();
    const peerRow = await waitFor(async () => {
      const rs = Array.from((await screen.findByRole('table')).querySelectorAll('tbody tr'));
      const r = rs.find(x => x.textContent.includes('192.168.10.4'));
      if (!r) throw new Error('还没渲染出对端的池');
      return r;
    });
    await userEvent.click(peerRow);
    await userEvent.click(await screen.findByRole('button', { name: /销毁存储池/ }));
    const dialog = (await screen.findByText(/将删除该池及其全部数据/)).closest('.card');
    // 强确认输入目标节点而不是池名：几台的池都叫 tank。
    expect(dialog.textContent).toContain('192.168.10.4');
    const input = dialog.querySelector('input');
    await userEvent.type(input, '192.168.10.4');
    await userEvent.click(within(dialog).getByRole('button', { name: /销毁|确认/ }));
    await waitFor(() => expect(destroy).toHaveBeenCalledWith('n2', 'pool-peer'));
  });
});

describe('存储页', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  // 池行标出用途，运维靠它分辨哪个池是哪个。
  it('池行标出数据池、备份池和未指定', async () => {
    stubApi({ listPools: async () => list([
      { ...POOLS[0], Role: 'data' },
      { ...POOLS[1], Role: 'backup' },
      { ...RAIDZ_POOL, Role: '' },
    ]) });
    renderPage();
    const table = await screen.findByRole('table');
    expect(await within(table).findByText('数据池')).toBeTruthy();
    expect(within(table).getByText('备份池')).toBeTruthy();
    expect(within(table).getByText('未指定')).toBeTruthy();
  });

  it('数据池排在备份池前面，同类保持原来的节点顺序', async () => {
    stubApi({ listPools: async () => list([
      { ...POOLS[0], ID: 'b3', Name: 'backup3', Role: 'backup' },
      { ...POOLS[0], ID: 'd3', Name: 'tank3', Role: 'data' },
      { ...POOLS[0], ID: 'b4', Name: 'backup4', Role: 'backup' },
      { ...POOLS[0], ID: 'd4', Name: 'tank4', Role: 'data' },
    ]) });
    renderPage();
    await screen.findByText('tank3');
    const names = screen.getAllByRole('row').slice(1).map(r => r.textContent.match(/(tank\d|backup\d)/)?.[0]).filter(Boolean);
    expect(names).toEqual(['tank3', 'tank4', 'backup3', 'backup4']);
  });

  it('按池类型筛选', async () => {
    stubApi({ listPools: async () => list([
      { ...POOLS[0], ID: 'd3', Name: 'tank3', Role: 'data' },
      { ...POOLS[0], ID: 'b3', Name: 'backup3', Role: 'backup' },
    ]) });
    renderPage();
    await screen.findByText('tank3');
    await userEvent.selectOptions(screen.getByLabelText('按类型筛选'), 'backup');
    await waitFor(() => expect(screen.queryByText('tank3')).toBeNull());
    expect(screen.getByText('backup3')).toBeTruthy();
  });

  // 每台最多两个池（数据 + 备份），到上限时按钮说明原因，不等后端拒绝。
  it('已有两个池时创建入口禁用并说明原因', async () => {
    renderPage();
    await screen.findByText('tank');
    const create = screen.getByRole('button', { name: /创建存储池/ });
    expect(create.disabled).toBe(true);
    expect(screen.getByText(/每个节点最多两个存储池/)).toBeTruthy();
  });

  it('列出存储池', async () => {
    renderPage();
    expect(await screen.findByText('tank')).toBeTruthy();
    expect(screen.getByText('cold')).toBeTruthy();
  });

  // 可用量单独写出：ZFS 按它判定 `out of space`。
  it('容量列写明已用和可用', async () => {
    renderPage();
    expect(await screen.findByText('已用 186.3 GiB · 可用 279.4 GiB')).toBeTruthy();
    expect(screen.getByText('已用 9.3 GiB · 可用 83.8 GiB')).toBeTruthy();
  });

  // 磁盘探测走 lsblk、可能单独失败，失败只影响空闲盘列表，不能让池列表消失。
  it('磁盘探测失败也照样显示存储池', async () => {
    stubApi({ listDisks: async () => { throw new Error('lsblk 不可用'); } });
    renderPage();
    expect(await screen.findByText('tank')).toBeTruthy();
  });

  it('存储池接口失败要报错', async () => {
    stubApi({ listPools: async () => { throw new Error('后端不可用'); } });
    renderPage();
    expect(await screen.findByText(/后端不可用|加载失败/)).toBeTruthy();
  });

  it('按健康状态过滤', async () => {
    renderPage();
    await screen.findByText('tank');
    // 过滤器是 <select>，不是一排按钮。
    await userEvent.selectOptions(screen.getByLabelText('按状态筛选'), 'abnormal');
    await waitFor(() => expect(screen.queryByText('tank')).toBeNull());
    expect(screen.getByText('cold')).toBeTruthy();
  });

  // 销毁要照抄池名确认，误点到不了，并且对话框说明会删掉什么。
  it('销毁存储池要先照抄池名', async () => {
    const deletePool = vi.fn(async () => ({ task_id: 'task-destroy_pool-1' }));
    stubApi({ deletePool });
    renderPage();
    await userEvent.click(await screen.findByText('tank'));

    await userEvent.click(await screen.findByRole('button', { name: /销毁/ }));
    const dialog = await screen.findByText(/无法恢复/);
    expect(dialog.textContent).toContain('镜像、配置、还原点、客户机克隆');

    const confirm = screen.getByRole('button', { name: '销毁' });
    expect(confirm.disabled).toBe(true);
    expect(deletePool).not.toHaveBeenCalled();

    await userEvent.type(screen.getByPlaceholderText('tank'), 'tank');
    expect(confirm.disabled).toBe(false);
    await userEvent.click(confirm);
    await waitFor(() => expect(deletePool).toHaveBeenCalledWith('pool-tank'));
  });

  // 盘被换掉或清空后库里记录还在，要写「找不到」，不能显示旧容量并计入总量。
  const GONE = { ID: 'pool-data1', Name: 'data1', Health: 'MISSING', Capacity: 0, Used: 0, Disks: ['/dev/sdb', '/dev/sdc'], ReadCacheDisks: [], WriteCacheDisks: [], Layout: 'stripe', GroupWidth: 1 };

  it('节点上已不在的池写明找不到，不显示容量', async () => {
    stubApi({ listPools: async () => list([POOLS[0], GONE]) });
    renderPage();
    await screen.findByText('data1');
    const row = screen.getByText('data1').closest('tr');
    expect(within(row).getByText('本机上已找不到该池')).toBeTruthy();
    expect(within(row).getByText('未找到')).toBeTruthy();
    expect(within(row).queryByText(/已用/)).toBeNull();
    expect(screen.getByText(/^1 块存储盘/)).toBeTruthy();
  });

  // 池已不存在，只删记录；照抄池名是为了防毁数据，这里没有数据，所以不要求。
  it('已不在的池只删记录，说明不动磁盘', async () => {
    const deletePool = vi.fn(async () => ({ task_id: 'task-destroy_pool-1' }));
    stubApi({ listPools: async () => list([POOLS[0], GONE]), deletePool });
    renderPage();
    await userEvent.click(await screen.findByText('data1'));
    expect(await screen.findByText(/删除这条记录不会改动任何磁盘/)).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: /删除记录/ }));
    const confirm = await screen.findByRole('button', { name: '删除' });
    expect(confirm.disabled).toBe(false);
    await userEvent.click(confirm);
    await waitFor(() => expect(deletePool).toHaveBeenCalledWith('pool-data1'));
  });

  // 带空格的池名会被 ZFS 拒绝，提前拦住，免得任务几秒后以 zfs 原文失败。
  it('建池前拦住空名、带空格的名字和没选盘', async () => {
    const createPool = vi.fn(async () => ({ task_id: 'task-create_pool-1' }));
    stubApi({ createPool, listPools: async () => list([POOLS[0]]) });
    renderPage();
    await screen.findByText('tank');
    await userEvent.click(screen.getByRole('button', { name: /创建存储池/ }));

    const submit = await screen.findByRole('button', { name: '创建' });
    const nameBox = screen.getByPlaceholderText('tank2');

    // 每道护栏都在其它条件已满足时单独验证，否则拦下的可能是后面的护栏。
    await userEvent.type(nameBox, 'tank2');
    await userEvent.click(submit);
    expect(createPool).not.toHaveBeenCalled(); // 未选盘

    // 只能选还没有加入池的盘。
    await userEvent.click(screen.getByText(/sdc/));
    await userEvent.clear(nameBox);
    await userEvent.click(submit);
    expect(createPool).not.toHaveBeenCalled(); // 池名为空

    await userEvent.type(nameBox, 'my pool');
    await userEvent.click(submit);
    expect(createPool).not.toHaveBeenCalled(); // 含空格，ZFS 会拒绝


    await userEvent.clear(nameBox);
    await userEvent.type(nameBox, 'tank2');
    // 一块盘只能建条带；条带无冗余，建之前要说明。
    await userEvent.click(screen.getByRole('button', { name: /^条带/ }));

    // mirrors 是 zpool 保留字（name is reserved），提前提示。
    await userEvent.clear(nameBox);
    await userEvent.type(nameBox, 'mirrors');
    await userEvent.click(submit);
    expect(createPool).not.toHaveBeenCalled();
    expect(await screen.findByText(/保留名/)).toBeTruthy();
    await userEvent.clear(nameBox);
    await userEvent.type(nameBox, 'tank2');

    await userEvent.click(submit);
    expect(createPool).not.toHaveBeenCalled(); // 等待确认
    await userEvent.click(await screen.findByRole('button', { name: '仍要创建' }));
    await waitFor(() => expect(createPool).toHaveBeenCalledWith({ name: 'tank2', disks: ['/dev/sdc'], layout: 'stripe', group_width: 1, role: 'data' }));
    // 建数据池会让服务重启一次，要提前说明，免得被当成故障。
    expect(await screen.findByText(/建成后将自动重启服务以启用该数据池/)).toBeTruthy();
  });

  // 布局建后不可改，选盘时就按布局校验盘数、写明还差几块并预估容量。
  it('建池按布局校验盘数并预估容量', async () => {
    const createPool = vi.fn(async () => ({ task_id: 'task-create_pool-1' }));
    stubApi({ createPool, listPools: async () => list([POOLS[0]]) });
    renderPage();
    await screen.findByText('tank');
    await userEvent.click(screen.getByRole('button', { name: /创建存储池/ }));
    const submit = await screen.findByRole('button', { name: '创建' });
    await userEvent.type(screen.getByPlaceholderText('tank2'), 'tank2');

    // 默认镜像；三块盘凑不成整组。
    await userEvent.click(screen.getByText(/sdc/));
    await userEvent.click(screen.getByText(/sdg/));
    await userEvent.click(screen.getByText(/sdh/));
    expect(await screen.findByText(/镜像池按 2 块一组，请再选 1 块/)).toBeTruthy();
    expect(submit.disabled).toBe(true);

    // 第四块凑成两组，按选择顺序配对：min(500,500) + min(500,400) = 900e9 = 838.2 GiB。
    await userEvent.click(screen.getByText(/sdi/));
    expect(await screen.findByText(/镜像 2 组 ≈ 838\.2 GiB 可用 · 每组可坏 1 块/)).toBeTruthy();
    expect(submit.disabled).toBe(false);

    // 四块盘 raidz2：(4−2) × 400e9 × 0.9 = 720e9 = 670.6 GiB。
    await userEvent.click(screen.getByRole('button', { name: /^raidz2/ }));
    expect(await screen.findByText(/raidz2 ≈ 670\.6 GiB 可用 · 可坏 2 块/)).toBeTruthy();
    // 去掉一块：三块不够 raidz2。
    await userEvent.click(screen.getByText(/sdi/));
    expect(await screen.findByText(/raidz2 至少 4 块盘，请再选 1 块/)).toBeTruthy();
    expect(submit.disabled).toBe(true);

    await userEvent.click(screen.getByRole('button', { name: /^镜像/ }));
    await userEvent.click(screen.getByText(/sdi/));
    await userEvent.click(submit);
    await waitFor(() => expect(createPool).toHaveBeenCalledWith({ name: 'tank2', disks: ['/dev/sdc', '/dev/sdg', '/dev/sdh', '/dev/sdi'], layout: 'mirror', group_width: 2, role: 'data' }));
  });

  // 镜像池加盘须整组；单个成员也可以加一块自己的镜像盘。两者走同一接口，对话框按后端规则校验数量。
  it('镜像池加盘要成对，成员可加镜像盘', async () => {
    const addPoolDisk = vi.fn(async () => ({ task_id: 'task-add_disk-1' }));
    stubApi({ addPoolDisk });
    renderPage();
    await userEvent.click(await screen.findByText('cold'));
    await screen.findByText('mirror-0');

    // 扩容：分区的「添加」打开选盘框，一块盘不成组。
    const addButtons = screen.getAllByRole('button', { name: /添加/ });
    await userEvent.click(addButtons[0]);
    const confirm = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdc/));
    expect(await screen.findByText(/镜像池按 2 块一组加盘，请再选 1 块/)).toBeTruthy();
    expect(confirm.disabled).toBe(true);
    await userEvent.click(screen.getByText(/sdg/));
    expect(await screen.findByText(/将新增 1 组/)).toBeTruthy();
    await userEvent.click(confirm);
    await waitFor(() => expect(addPoolDisk).toHaveBeenCalledWith('pool-cold', { disks: ['/dev/sdc', '/dev/sdg'] }));

    // 附加：成员行自己的操作给这块盘加镜像。
    await userEvent.click(screen.getByRole('button', { name: '给 /dev/sdb 加 RAID1 盘' }));
    const confirm2 = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdh/));
    await userEvent.click(confirm2);
    await waitFor(() => expect(addPoolDisk).toHaveBeenCalledWith('pool-cold', { disks: ['/dev/sdh'], mode: 'attach', target: '/dev/sdb' }));
  });

  // raidz 没有镜像可加、只接受整组，对话框直接说明，不提供会被拒绝的操作。
  it('raidz 池整组加盘且没有加镜像盘', async () => {
    stubApi({ listPools: async () => list([...POOLS, RAIDZ_POOL]) });
    renderPage();
    await userEvent.click(await screen.findByText('backup'));
    await screen.findByText('raidz2-0');
    expect(screen.queryByRole('button', { name: /加 RAID1 盘/ })).toBeNull();

    await userEvent.click(screen.getAllByRole('button', { name: /添加/ })[0]);
    const confirm = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdc/));
    expect(await screen.findByText(/raidz2 池按整组加盘，每组 4 块，请再选 3 块/)).toBeTruthy();
    expect(confirm.disabled).toBe(true);
  });

  // raidz 池不能移除盘，坏盘只能换盘。
  it('raidz 池不能移盘，可以换盘', async () => {
    const replacePoolDisk = vi.fn(async () => ({ task_id: 'task-replace_disk-1' }));
    stubApi({ listPools: async () => list([...POOLS, RAIDZ_POOL]), replacePoolDisk });
    renderPage();
    await userEvent.click(await screen.findByText('backup'));
    await screen.findByText('raidz2-0');
    expect(screen.queryByRole('button', { name: /^移除 \/dev/ })).toBeNull();
    expect(screen.getByText(/raidz 不能移盘/)).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: '换掉 /dev/sdj' }));
    const confirm = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdc/));
    await userEvent.click(confirm);
    await waitFor(() => expect(replacePoolDisk).toHaveBeenCalledWith('pool-backup', { old_disk: '/dev/sdj', new_disk: '/dev/sdc' }));
  });

  // 从镜像摘一路要说明组会变窄；整组也可以移除，数据迁到其它组。
  it('镜像成员摘一路要确认，整组可移除', async () => {
    const removePoolDisk = vi.fn(async () => ({ task_id: 'task-detach_disk-1' }));
    stubApi({ removePoolDisk });
    renderPage();
    await userEvent.click(await screen.findByText('cold'));
    await screen.findByText('mirror-0');

    await userEvent.click(screen.getByRole('button', { name: '移除 /dev/sdb' }));
    expect((await screen.findByText(/mirror-0 将只剩 1 路/)).textContent).toContain('暂无冗余');
    await userEvent.click(screen.getByRole('button', { name: '摘除' }));
    await waitFor(() => expect(removePoolDisk).toHaveBeenCalledWith('pool-cold', { disk: '/dev/sdb' }));

    await userEvent.click(screen.getByRole('button', { name: '移除整组 mirror-1' }));
    await screen.findByText(/迁移到其他组/);
    await userEvent.click(screen.getByRole('button', { name: '移除整组' }));
    await waitFor(() => expect(removePoolDisk).toHaveBeenCalledWith('pool-cold', { disk: 'mirror-1' }));
  });

  // 多块盘的条带池通过向导升级为 RAID1：每块数据盘配一块空闲盘，一个任务全部附加，在线进行、不重建。
  // 「镜像」在本产品指系统镜像，磁盘镜像统一叫 RAID1。
  it('多盘条带池可一键升级为 RAID1', async () => {
    const mirrorUpgradePool = vi.fn(async () => ({ task_id: 'task-mirror_upgrade-1' }));
    stubApi({ mirrorUpgradePool, listPools: async () => list([{ ...POOLS[0], Disks: ['sda', 'sdb'] }]) });
    renderPage();
    await userEvent.click(await screen.findByText('tank'));
    await userEvent.click(await screen.findByRole('button', { name: /升级为 RAID1/ }));

    const start = await screen.findByRole('button', { name: '开始升级' });
    expect(start.disabled).toBe(true); // 尚未配对
    await userEvent.selectOptions(screen.getByLabelText('sda 的 RAID1 盘'), '/dev/sdc');
    expect(start.disabled).toBe(true); // 只配一半仍无冗余
    await userEvent.selectOptions(screen.getByLabelText('sdb 的 RAID1 盘'), '/dev/sdg');
    expect(start.disabled).toBe(false);
    await userEvent.click(start);
    await waitFor(() => expect(mirrorUpgradePool).toHaveBeenCalledWith('pool-tank',
      { pairs: [{ target: 'sda', disk: '/dev/sdc' }, { target: 'sdb', disk: '/dev/sdg' }] }));
  });

  // 只有一块数据盘时「升级」和这块盘的「加 RAID1 盘」是同一件事，只留后者。
  it('单盘条带池不显示升级，只给这块盘加 RAID1 盘', async () => {
    renderPage();
    await userEvent.click(await screen.findByText('tank'));
    expect(await screen.findByRole('button', { name: '给 sda 加 RAID1 盘' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /升级为 RAID1/ })).toBeNull();
  });

  // special vdev 存元数据，只能镜像添加；热备盘按单盘增删。
  it('元数据盘必须镜像，热备盘可增删', async () => {
    const addSpecial = vi.fn(async () => ({ task_id: 'task-add_special-1' }));
    const addSpare = vi.fn(async () => ({ task_id: 'task-add_spare-1' }));
    const removeSpare = vi.fn(async () => ({ task_id: 'task-remove_spare-1' }));
    stubApi({ addSpecial, addSpare, removeSpare });
    renderPage();
    await userEvent.click(await screen.findByText('cold'));
    await screen.findByText('mirror-0');

    const addButtons = screen.getAllByRole('button', { name: /添加/ });
    await userEvent.click(addButtons[1]); // 元数据盘
    const confirm = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdc/));
    expect(await screen.findByText(/元数据盘必须镜像，请选 2 或 3 块/)).toBeTruthy();
    expect(confirm.disabled).toBe(true);
    await userEvent.click(screen.getByText(/sdg/));
    await userEvent.click(confirm);
    await waitFor(() => expect(addSpecial).toHaveBeenCalledWith('pool-cold', { disks: ['/dev/sdc', '/dev/sdg'] }));

    await userEvent.click(screen.getAllByRole('button', { name: /添加/ })[2]); // 热备盘
    const confirm2 = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdi/));
    await userEvent.click(confirm2);
    await waitFor(() => expect(addSpare).toHaveBeenCalledWith('pool-cold', { disks: ['/dev/sdi'] }));

    await userEvent.click(screen.getByRole('button', { name: '移除热备盘 /dev/sdz' }));
    await userEvent.click(await screen.findByRole('button', { name: '移除' }));
    await waitFor(() => expect(removeSpare).toHaveBeenCalledWith('pool-cold', { disk: '/dev/sdz' }));
  });

  // raidz 扩一块盘只在节点支持时提供，否则在页面上说明原因，而不是给一个会失败的按钮。
  it('raidz 扩容按节点能力显示', async () => {
    const addPoolDisk = vi.fn(async () => ({ task_id: 'task-add_disk-1' }));
    stubApi({ addPoolDisk, listPools: async () => list([...POOLS, { ...RAIDZ_POOL, RaidzExpandable: false, RaidzExpandNote: '本机 ZFS 用户态版本 2.2.2 不支持 raidz 单盘扩容（需 ≥ 2.3）' }]) });
    const { unmount } = renderPage();
    await userEvent.click(await screen.findByText('backup'));
    await screen.findByText('raidz2-0');
    expect(screen.queryByRole('button', { name: /扩一块盘/ })).toBeNull();
    expect(screen.getByText(/不支持 raidz 单盘扩容/)).toBeTruthy();
    unmount();

    stubApi({ addPoolDisk, listPools: async () => list([...POOLS, { ...RAIDZ_POOL, RaidzExpandable: true }]) });
    renderPage();
    await userEvent.click(await screen.findByText('backup'));
    await userEvent.click(await screen.findByRole('button', { name: '给 raidz2-0 扩一块盘' }));
    const confirm = await screen.findByRole('button', { name: '确认' });
    await userEvent.click(screen.getByText(/sdc/));
    await userEvent.click(confirm);
    await waitFor(() => expect(addPoolDisk).toHaveBeenCalledWith('pool-backup', { disks: ['/dev/sdc'], mode: 'attach', target: 'raidz2-0' }));
  });

  // 表格显示布局，详情按组展示成员，降级的组点名掉线的成员。
  it('列表显示布局，详情按组展示成员', async () => {
    renderPage();
    expect(await screen.findByText('镜像（RAID1）×2')).toBeTruthy();
    expect(screen.getByText('条带')).toBeTruthy();

    await userEvent.click(screen.getByText('cold'));
    expect(await screen.findByText('mirror-0')).toBeTruthy();
    expect(screen.getByText('mirror-1')).toBeTruthy();
    const row = screen.getByText('/dev/sdd').closest('tr');
    // ZFS 原词翻译后显示，原文留在悬停里。
    expect(row.textContent).toContain('已离线');
    // 热备盘列在自己的标题下，不混在数据盘里。
    expect(screen.getAllByText('热备盘').length).toBeGreaterThan(0);
    expect(screen.getByText('/dev/sdz').closest('tr').textContent).toContain('热备盘');
  });
});

// 对别的节点的池加盘、换盘、加缓存，写操作发往那台，选盘也必须列那台自己的空闲盘。
describe('别的节点的池详情', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加盘时列的是池所在节点的空闲盘', async () => {
    const CLUSTER_NODES = [
      { id: 'n1', ip: '192.168.10.3', ha_state: 'active', online: true },
      { id: 'n2', ip: '192.168.10.4', ha_state: 'standby', online: true },
    ];
    const listClusterDisks = vi.fn(async (node) => list(node === 'n2'
      ? [{ Path: '/dev/sdx', Name: 'sdx', Size: 900e9, InUse: false }, { Path: '/dev/sdb', Name: 'sdb', Size: 900e9, InUse: true }]
      : DISKS));
    stubApi({
      listClusterNodes: async () => list(CLUSTER_NODES),
      listClusterPools: async () => ({
        nodes: [
          { node_id: 'n1', ip: '192.168.10.3', role: 'active', is_self: true, online: true, reachable: true, pools: [{ ...POOLS[0], ServerID: 'n1' }] },
          { node_id: 'n2', ip: '192.168.10.4', role: 'standby', online: true, reachable: true,
            pools: [{ ...POOLS[1], ID: 'pool-peer', ServerID: 'n2' }] },
        ],
        total_capacity: 600e9, total_used: 210e9, pool_count: 2, unreachable: 0,
      }),
      listClusterDisks,
    });
    renderPage();
    await userEvent.click(await screen.findByText('cold'));
    await screen.findByText('mirror-0');
    await waitFor(() => expect(listClusterDisks).toHaveBeenCalledWith('n2'));
    await userEvent.click(screen.getAllByRole('button', { name: /添加/ })[0]);
    await screen.findByRole('button', { name: '确认' });
    expect(await screen.findByText(/sdx/)).toBeTruthy();
    expect(screen.queryByText(/sdc/)).toBeNull();
  });
});
