import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageTerminals } from './terminals.jsx';

// 客户机页：后端部分接口失败时列表仍可用，筛选结果正确，保存超管机必须填还原点名。

const list = (items) => ({ items, total: items.length });
const TERMINALS = [
  { ID: 'terminal-A', Name: '一号机', MAC: 'AABBCCDDEE01', IP: '192.168.50.101', GroupID: 'g-1', IsSuper: false, State: 'online' },
  { ID: 'terminal-B', Name: '二号机', MAC: 'AABBCCDDEE02', IP: '192.168.50.102', GroupID: 'g-1', IsSuper: true, State: 'offline' },
  { ID: 'terminal-C', Name: '美术教室机', MAC: 'AABBCCDDEE03', IP: '192.168.60.101', GroupID: 'g-2', IsSuper: false, State: 'offline' },
  { ID: 'terminal-D', Name: '还开着的超管机', MAC: 'AABBCCDDEE04', IP: '192.168.50.104', GroupID: 'g-1', IsSuper: true, State: 'online' },
];
const GROUPS = [
  { ID: 'g-1', Name: '教学一班', SystemImageID: 'win11' },
  { ID: 'g-2', Name: '美术教室', SystemImageID: 'win11' },
];

function stubApi(overrides = {}) {
  const base = {
    listTerminals: async () => list(TERMINALS),
    listGroups: async () => list(GROUPS),
    listImages: async () => list([{ ID: 'win11', Name: 'Win 11', OSType: 'windows', State: 'normal' }]),
    listDriverBundles: async () => list([]),
    listTasks: async () => list([]),
    listAlarms: async () => list([]),
    listPools: async () => list([]),
    listClusterNodes: async () => list([]),
    superDisks: async () => list([]),
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageTerminals/></ConfirmProvider></StoreProvider>,
);

// 多节点时显示客户机落在哪台服务器（StorageServerID），单机时不显示这一列。
describe('终端所在节点', () => {
  const CLUSTER = [
    { id: 'n1', ip: '10.0.0.3', ha_state: 'active', online: true },
    { id: 'n2', ip: '10.0.0.4', ha_state: 'standby', online: true },
  ];
  afterEach(() => vi.restoreAllMocks());

  it('多节点时列出客户机落在哪台', async () => {
    stubApi({
      listClusterNodes: async () => list(CLUSTER),
      listTerminals: async () => list([
        { ...TERMINALS[0], StorageServerID: 'n2' },
        { ...TERMINALS[1], StorageServerID: null },
      ]),
    });
    renderPage();
    const row = (await screen.findByText('一号机')).closest('tr');
    expect(row.textContent).toContain('10.0.0.4');
    expect(screen.getAllByText('节点').length).toBeGreaterThan(0);
  });

  it('单机不加这一列', async () => {
    stubApi({ listClusterNodes: async () => list([{ id: 'n1', ip: '10.0.0.3', ha_state: 'active', online: true }]) });
    renderPage();
    await screen.findByText('一号机');
    expect(screen.queryByText('节点')).toBeNull();
  });
});

describe('终端页', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  it('列出终端及其分组', async () => {
    renderPage();
    expect(await screen.findByText('一号机')).toBeTruthy();
    expect(screen.getByText('美术教室机')).toBeTruthy();
    expect(screen.getAllByText('教学一班').length).toBeGreaterThan(0);
  });

  // 只有在线、离线两种：旧数据里的 unknown 按离线算，也没有「未知」筛选。
  it('没有「未知」状态，旧数据按离线显示', async () => {
    stubApi({ listTerminals: async () => list([...TERMINALS,
      { ID: 'terminal-E', Name: '旧数据机', MAC: 'AABBCCDDEE05', IP: '192.168.50.105', GroupID: 'g-1', IsSuper: false, State: 'unknown' }]) });
    renderPage();
    expect(await screen.findByText('旧数据机')).toBeTruthy();
    expect(screen.queryByText('未知')).toBeNull();
    expect(screen.getByRole('button', { name: /离线\s*3/ })).toBeTruthy();
  });

  // 四个列表一起取、分开 settle：分组列表失败只丢分组名，不能丢客户机。
  it('分组接口挂了也照样显示终端', async () => {
    stubApi({ listGroups: async () => { throw new Error('500'); } });
    renderPage();
    expect(await screen.findByText('一号机')).toBeTruthy();
    expect(screen.getByText('二号机')).toBeTruthy();
  });

  // 客户机列表本身失败时要明说，不能显示成空列表。
  it('终端接口挂了要报错，而不是显示成没有终端', async () => {
    stubApi({ listTerminals: async () => { throw new Error('后端不可用'); } });
    renderPage();
    expect(await screen.findByText(/后端不可用|加载失败/)).toBeTruthy();
  });

  it('搜索按名称与 MAC 收窄', async () => {
    renderPage();
    await screen.findByText('一号机');

    const box = screen.getByPlaceholderText(/搜索/);
    await userEvent.type(box, '美术');
    await waitFor(() => expect(screen.queryByText('一号机')).toBeNull());
    expect(screen.getByText('美术教室机')).toBeTruthy();

    await userEvent.clear(box);
    await userEvent.type(box, 'AABBCCDDEE01');
    await waitFor(() => expect(screen.queryByText('美术教室机')).toBeNull());
    expect(screen.getByText('一号机')).toBeTruthy();
  });

  // 保存超管机即生成还原点，必须先填名称，不能等机器已被要求关机后再被后端拒绝。
  it('超管停机必须先填还原点名称', async () => {
    const stopSuper = vi.fn(async () => ({ task_id: 'task-super_stop-1' }));
    stubApi({ stopSuper });
    renderPage();
    await screen.findByText('二号机');

    await userEvent.click(screen.getByText('二号机'));
    await userEvent.click(await screen.findByRole('button', { name: /存还原点|超管停机/ }));

    await userEvent.click(await screen.findByRole('button', { name: /保存还原点|存盘停机/ }));
    expect(stopSuper).not.toHaveBeenCalled();

    await userEvent.type(screen.getByPlaceholderText('2026Q2-update'), '  装完office  ');
    await userEvent.click(screen.getByRole('button', { name: /保存还原点|存盘停机/ }));
    // 名称要去掉首尾空格，否则会存成另一个还原点名。
    await waitFor(() => expect(stopSuper).toHaveBeenCalledWith('terminal-B', { reduction_name: '装完office' }));
  });

  // 有数据盘的超管机：逐盘列出，只提交勾选且填了名称的盘。
  it('超管停机可逐块勾选数据盘存还原点', async () => {
    const stopSuper = vi.fn(async () => ({ task_id: 'task-super_stop-1' }));
    stubApi({
      stopSuper,
      listGroupDisks: async () => list([
        { ID: 'disk-d', GroupID: 'g-1', MountTarget: 'D:', ImageID: 'games', ConfigID: 'games_default' },
        { ID: 'disk-e', GroupID: 'g-1', MountTarget: 'E:', ImageID: 'docs', ConfigID: 'docs_default' },
      ]),
    });
    renderPage();
    await screen.findByText('二号机');
    await userEvent.click(screen.getByText('二号机'));
    await userEvent.click(await screen.findByRole('button', { name: /存还原点|超管停机/ }));

    await userEvent.type(await screen.findByPlaceholderText('2026Q2-update'), 'sys-v2');
    // 两块盘都列出；勾选 E: 并命名，D: 不勾。
    expect(await screen.findByLabelText('保存数据盘 D:')).toBeTruthy();
    await userEvent.click(screen.getByLabelText('保存数据盘 E:'));
    await userEvent.type(screen.getByLabelText('数据盘 E: 还原点名称'), ' 装完游戏 ');
    await userEvent.click(screen.getByRole('button', { name: /保存还原点|存盘停机/ }));
    await waitFor(() => expect(stopSuper).toHaveBeenCalledWith('terminal-B', {
      reduction_name: 'sys-v2',
      data_disks: [{ disk_id: 'disk-e', reduction_name: '装完游戏' }],
    }));
  });

  // 勾选数据盘却没填还原点名，提交前就拦下，与系统盘一致。
  it('勾选的数据盘必须填名称', async () => {
    const stopSuper = vi.fn(async () => ({ task_id: 'task-super_stop-1' }));
    stubApi({
      stopSuper,
      listGroupDisks: async () => list([{ ID: 'disk-d', GroupID: 'g-1', MountTarget: 'D:', ImageID: 'games', ConfigID: 'games_default' }]),
    });
    renderPage();
    await screen.findByText('二号机');
    await userEvent.click(screen.getByText('二号机'));
    await userEvent.click(await screen.findByRole('button', { name: /存还原点|超管停机/ }));
    await userEvent.type(await screen.findByPlaceholderText('2026Q2-update'), 'sys-v2');
    await userEvent.click(await screen.findByLabelText('保存数据盘 D:'));
    await userEvent.click(screen.getByRole('button', { name: /保存还原点|存盘停机/ }));
    expect(stopSuper).not.toHaveBeenCalled();
  });
});

// 机器在线时保存，后端要等满两分钟才失败。所以在线时超管相关操作直接禁用，并说明何时可做。
it('在线的超管机，超管操作直接禁用', async () => {
  const stopSuper = vi.fn(async () => ({ task_id: 'task-super_stop-1' }));
  stubApi({ stopSuper });
  renderPage();
  await screen.findByText('还开着的超管机');
  await userEvent.click(screen.getByText('还开着的超管机'));

  const save = await screen.findByRole('button', { name: /存还原点/ });
  expect(save.disabled).toBe(true);
  expect(screen.getByRole('button', { name: '取消超管' }).disabled).toBe(true);
  expect(screen.getByText(/关机后可/)).toBeTruthy();
  await userEvent.click(save);
  expect(stopSuper).not.toHaveBeenCalled();
});

it('在线的客户机，编辑表单里的终端类型开关也禁用', async () => {
  stubApi({});
  renderPage();
  const row = (await screen.findByText('一号机')).closest('tr');   // terminal-A: 在线、普通
  await userEvent.click(within(row).getByRole('button', { name: '编辑' }));
  expect((await screen.findByRole('button', { name: '超管机' })).disabled).toBe(true);
  expect(screen.getByRole('button', { name: '普通终端' }).disabled).toBe(true);
  expect(screen.getByText(/关机后/)).toBeTruthy();
});

// 「取消超管」会当场删掉超管盘，未保存的改动无法找回，必须先确认后果。
it('取消超管要先确认，说清楚会丢弃没保存的改动', async () => {
  const disableSuper = vi.fn(async () => ({ ID: 'terminal-B', IsSuper: false }));
  stubApi({ disableSuper });
  renderPage();
  await screen.findByText('二号机');

  await userEvent.click(screen.getByText('二号机'));
  await userEvent.click(await screen.findByRole('button', { name: '取消超管' }));
  let dialog = (await screen.findByText(/丢弃.*没保存|无法恢复/)).closest('.card');
  await userEvent.click(within(dialog).getByRole('button', { name: '取消' }));
  expect(disableSuper).not.toHaveBeenCalled();

  await userEvent.click(screen.getByRole('button', { name: '取消超管' }));
  dialog = (await screen.findByText(/丢弃.*没保存|无法恢复/)).closest('.card');
  await userEvent.click(within(dialog).getByRole('button', { name: /丢弃|确认/ }));
  await waitFor(() => expect(disableSuper).toHaveBeenCalledWith('terminal-B'));
});

// 编辑表单里把「终端类型」从超管改回普通，和「取消超管」是同一件事，同样会删盘。
it('编辑时把超管改回普通终端也要先确认', async () => {
  const updateTerminal = vi.fn(async () => ({ ID: 'terminal-B', IsSuper: false }));
  stubApi({ updateTerminal });
  renderPage();
  const row = (await screen.findByText('二号机')).closest('tr');
  await userEvent.click(within(row).getByRole('button', { name: '编辑' }));
  await userEvent.click(await screen.findByRole('button', { name: '普通终端' }));
  await userEvent.click(screen.getByRole('button', { name: '保存' }));

  const dialog = (await screen.findByText(/无法恢复/)).closest('.card');
  await userEvent.click(within(dialog).getByRole('button', { name: '取消' }));
  expect(updateTerminal).not.toHaveBeenCalled();

  await userEvent.click(screen.getByRole('button', { name: '保存' }));
  await userEvent.click(within((await screen.findByText(/无法恢复/)).closest('.card')).getByRole('button', { name: /丢弃|确认/ }));
  await waitFor(() => expect(updateTerminal).toHaveBeenCalledWith('terminal-B', expect.objectContaining({ is_super: false })));
});

it('换到网段不含原 IP 的分组时清空 IP，交给后端在新分组里分配', async () => {
  const updateTerminal = vi.fn(async () => ({ ID: 'terminal-C' }));
  stubApi({
    updateTerminal,
    listGroups: async () => list([
      { ID: 'g-1', Name: '教学一班', SystemImageID: 'win11', StartIP: '192.168.50.100', ClientMax: 20 },
      { ID: 'g-2', Name: '美术教室', SystemImageID: 'win11', StartIP: '192.168.60.100', ClientMax: 20 },
    ]),
  });
  renderPage();
  const row = (await screen.findByText('美术教室机')).closest('tr');
  await userEvent.click(within(row).getByRole('button', { name: '编辑' }));
  const ip = await screen.findByDisplayValue('192.168.60.101');
  const dialog = ip.closest('.card');
  await userEvent.selectOptions(within(dialog).getByDisplayValue('美术教室'), 'g-1');
  expect(ip.value).toBe('');
  expect(within(dialog).getByText(/留空将在新分组网段内自动分配/)).toBeTruthy();

  await userEvent.selectOptions(within(dialog).getByDisplayValue('教学一班'), 'g-2');
  expect(ip.value).toBe('192.168.60.101');

  await userEvent.selectOptions(within(dialog).getByDisplayValue('美术教室'), 'g-1');
  await userEvent.click(within(dialog).getByRole('button', { name: '保存' }));
  await waitFor(() => expect(updateTerminal).toHaveBeenCalledWith('terminal-C', expect.objectContaining({ ip: '', group_id: 'g-1' })));
});

it('表格默认每页 10 台，能切换条数', async () => {
  const many = Array.from({ length: 12 }, (_, i) => ({ ID: `t-${i}`, Name: `机器${String(i).padStart(2, '0')}`, MAC: `AABBCCDDEE${String(i).padStart(2, '0')}`, IP: `192.168.50.${110 + i}`, GroupID: 'g-1', IsSuper: false, State: 'offline' }));
  stubApi({ listTerminals: async () => list(many) });
  renderPage();
  expect(await screen.findByText('共 12 条')).toBeTruthy();
  expect(screen.getByText('机器09')).toBeTruthy();
  expect(screen.queryByText('机器10')).toBeNull();
  await userEvent.selectOptions(screen.getByLabelText('每页条数'), '20');
  expect(screen.getByText('机器11')).toBeTruthy();
});

// 不碰超管开关的编辑（改个名）不弹框。
it('编辑超管机但不改终端类型时不打扰', async () => {
  const updateTerminal = vi.fn(async () => ({ ID: 'terminal-B', IsSuper: true }));
  stubApi({ updateTerminal });
  renderPage();
  const row = (await screen.findByText('二号机')).closest('tr');
  await userEvent.click(within(row).getByRole('button', { name: '编辑' }));
  await userEvent.click(screen.getByRole('button', { name: '保存' }));
  await waitFor(() => expect(updateTerminal).toHaveBeenCalledWith('terminal-B', expect.objectContaining({ is_super: true })));
  expect(screen.queryByText(/无法恢复/)).toBeNull();
});


// 超管机数据盘可在线「保存并发布」，要列出未发布的改动量，一眼看出有没有东西要发布。
it('超管机的数据盘能在线发布', async () => {
  const publishDataDisk = vi.fn(async () => ({ task_id: 'task-publish-1' }));
  stubApi({
    publishDataDisk,
    superDisks: async () => list([
      { disk_id: 'gd-1', lun: 1, mount_target: 'D:', config_name: '游戏盘', current_name: '2026-09-14', written: 13100000000, ready: true },
    ]),
  });
  renderPage();
  await screen.findByText('还开着的超管机');
  await userEvent.click(screen.getByText('还开着的超管机'));

  expect(await screen.findByText('2026-09-14')).toBeTruthy();   // 当前发布点
  expect(screen.getByText(/1[23]\.\d+ GiB/)).toBeTruthy(); // 未发布改动
  await userEvent.click(screen.getByRole('button', { name: /保存并发布/ }));

  await userEvent.type(await screen.findByPlaceholderText(/永劫无间|2026/), '装了永劫无间');
  await userEvent.click(screen.getByRole('button', { name: /^发布$|确认发布/ }));
  await waitFor(() => expect(publishDataDisk).toHaveBeenCalledWith('terminal-D', { disk_id: 'gd-1', name: '装了永劫无间' }));
});

it('没有改动、或者还没开过机，就不让发布', async () => {
  stubApi({
    superDisks: async () => list([
      { disk_id: 'gd-1', lun: 1, mount_target: 'D:', config_name: '游戏盘', current_name: '2026-09-14', written: 0, ready: true },
      { disk_id: 'gd-2', lun: 2, mount_target: 'E:', config_name: '资料盘', current_name: '初始', written: 0, ready: false },
    ]),
  });
  renderPage();
  await screen.findByText('还开着的超管机');
  await userEvent.click(screen.getByText('还开着的超管机'));

  const rows = await screen.findAllByRole('row');
  const d = rows.find(r => within(r).queryByText('D:'));
  const e = rows.find(r => within(r).queryByText('E:'));
  expect(within(d).getByRole('button', { name: /保存并发布/ }).disabled).toBe(true);
  expect(within(d).getByText(/无改动/)).toBeTruthy();
  expect(within(e).getByRole('button', { name: /保存并发布/ }).disabled).toBe(true);
  expect(within(e).getByText(/开机后/)).toBeTruthy();
});

// 弹窗里要写明：存还原点不会结束超管。
it('存还原点的弹窗要说清楚保存后还是超管机', async () => {
  stubApi({});
  renderPage();
  await screen.findByText('二号机');
  await userEvent.click(screen.getByText('二号机'));
  await userEvent.click(await screen.findByRole('button', { name: /存还原点/ }));
  expect(await screen.findByText(/仍为超管机/)).toBeTruthy();
});

// 超管机的用途说明收在问号后面；常用约束（同一配置只能一台、关机后才能切换）一直显示。
it('终端类型：约束一直显示，超管机的说明收在问号后面', async () => {
  stubApi({});
  renderPage();
  const row = (await screen.findByText('美术教室机')).closest('tr');  // terminal-C：离线普通终端
  await userEvent.click(within(row).getByRole('button', { name: '编辑' }));

  expect(await screen.findByText(/同一配置同时只能有一台超管机/)).toBeTruthy();
  expect(screen.queryByText(/超管机用于修改镜像内容/)).toBeNull();

  await userEvent.click(screen.getByRole('button', { name: '终端类型说明' }));
  expect(await screen.findByText(/超管机用于修改镜像内容/)).toBeTruthy();
});

// 勾选只对眼前这一页有效：筛选、换视图后清空，批量删除不会删到看不见的行。
it('筛选或切换视图后清空勾选', async () => {
  stubApi();
  renderPage();
  const row = (await screen.findByText('一号机')).closest('tr');
  await userEvent.click(row.querySelector('input[type="checkbox"]'));
  expect(screen.getByText('已选 1')).toBeTruthy();

  await userEvent.type(screen.getByPlaceholderText(/搜索/), '美术');
  await waitFor(() => expect(screen.queryByText(/已选/)).toBeNull());

  await userEvent.clear(screen.getByPlaceholderText(/搜索/));
  const row2 = (await screen.findByText('一号机')).closest('tr');
  await userEvent.click(row2.querySelector('input[type="checkbox"]'));
  expect(screen.getByText('已选 1')).toBeTruthy();
  await userEvent.click(screen.getByRole('button', { name: '矩阵' }));
  await waitFor(() => expect(screen.queryByText(/已选/)).toBeNull());
});
