import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageGroups } from './groups.jsx';

// 分组绑定 镜像 → 配置（还原点用配置的应用点）。<select> 值为空时仍显示第一项，
// 防止表单值为空而下一级报「无可用配置」。

const list = (items) => ({ items, total: items.length });
const IMAGES = [
  { ID: 'isharedisk', Name: 'isharedisk', OSType: 'windows', State: 'normal', Purpose: 'system', CreatedAt: '2026-07-30T10:00:00Z' },
  { ID: 'games', Name: 'games', OSType: 'windows', State: 'normal', Purpose: 'data', CreatedAt: '2026-08-18T10:00:00Z' },
  { ID: 'linuxdata', Name: 'linuxdata', OSType: 'linux', State: 'normal', Purpose: 'data', CreatedAt: '2026-08-18T10:00:00Z' },
];
const CONFIGS = [{ ID: 'isharedisk_default', ImageID: 'isharedisk', Name: 'default', DefaultReductionID: 'isharedisk_default_0' }];
const REDUCTIONS = [{ ID: 'isharedisk_default_0', ConfigID: 'isharedisk_default', Name: '@0', DisplayName: '0', Status: 'ready' }];
// 服务器的客户机网段：网络字段的判定依据和新分组默认值的来源。
const NETWORK = {
  interfaces: [{ name: 'ndbr0', addrs: ['192.168.50.1/24'], up: true, client: true }],
  client_iface: 'ndbr0', client_networks: ['192.168.50.1/24'], server_addrs: ['192.168.50.1'],
  boot_host: '192.168.50.1', allow_cross_subnet: false, known: true,
  suggest: { start_ip: '192.168.50.10', client_max: 30, netmask: '255.255.255.0', gateway: '' },
  groups: [],
};

function stubApi(overrides = {}) {
  const base = {
    listGroups: async () => list([]),
    listImages: async () => list(IMAGES),
    listTerminals: async () => list([]),
    listConfigs: async () => list(CONFIGS),
    listReductions: async () => list(REDUCTIONS),
    listGroupDisks: async () => list([]),
    listPools: async () => list([]),
    listTasks: async () => list([]),
    listAlarms: async () => list([]),
    getNetwork: async () => NETWORK,
    listClusterNodes: async () => list([]),
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageGroups/></ConfirmProvider></StoreProvider>,
);

describe('分组页', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  it('新建分组时三级联动都有值，而不是「无可用配置」', async () => {
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));

    // 下拉显示的镜像必须就是表单里的值，否则下一级查不到配置。
    await waitFor(() => expect(api.listConfigs).toHaveBeenCalledWith('isharedisk'));

    expect(screen.queryByText('无可用配置')).toBeNull();
    expect(await screen.findByText('default')).toBeTruthy();
  });

  it('保存时带上镜像 / 配置', async () => {
    const createGroup = vi.fn(async () => ({ ID: 'g-1' }));
    stubApi({ createGroup });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
    await waitFor(() => expect(api.listConfigs).toHaveBeenCalled());

    // 名称框没有 placeholder，Field 把标签渲染成兄弟 div，只能经共同父节点找输入框。
    const nameField = screen.getByText(/分组名称/).parentElement.querySelector('input');
    await userEvent.type(nameField, '教学一班');
    await userEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(createGroup).toHaveBeenCalled(), { timeout: 3000 });
    const body = createGroup.mock.calls[0][0];
    expect(body.system_image_id).toBe('isharedisk');
    expect(body.system_config_id).toBe('isharedisk_default');
    // 还原点在配置上「应用」决定，不按分组选。
    expect(body.system_reduction_id).toBeUndefined();
  });

  // 系统盘只列系统镜像，数据盘只列同系统的数据盘镜像。
  it('系统盘与数据盘各自只列出对应用途的镜像', async () => {
    stubApi({ listGroups: async () => list([
      { ID: 'g-1', Name: '教学一班', StartIP: '192.168.10.10', ClientMax: 30, Gateway: '192.168.10.1', Netmask: '255.255.255.0', SystemImageID: 'isharedisk', SystemConfigID: 'isharedisk_default', SystemReductionID: 'isharedisk_default_0' },
    ]) });
    renderPage();
    await screen.findByText('教学一班');
    await userEvent.click(screen.getByRole('button', { name: /新建分组/ }));
    const sys = await screen.findByLabelText('系统镜像');
    const sysOptions = [...sys.querySelectorAll('option')].map(o => o.value);
    expect(sysOptions).toContain('isharedisk');
    expect(sysOptions).not.toContain('games');

    await userEvent.click(screen.getByRole('button', { name: /添加数据盘/ }));
    const data = await screen.findByLabelText('新数据盘 1 的镜像');
    const dataOptions = [...data.querySelectorAll('option')].map(o => o.value);
    expect(dataOptions).toEqual(['games']); // 只有 Windows 数据盘
  });

  // 还没导入镜像时表单要明说无可绑定，不能看起来像可以提交。
  it('真的没有镜像时才显示「无可用镜像」', async () => {
    stubApi({ listImages: async () => list([]), listConfigs: async () => list([]), listReductions: async () => list([]) });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
    expect(await screen.findByText('无可用镜像')).toBeTruthy();
  });

  // 表单默认值取自服务器网段，不能是代码里写死的地址。
  it('新建分组的网络默认值来自服务器客户机网卡', async () => {
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
    expect((await screen.findByLabelText('起始 IP')).value).toBe('192.168.50.10');
    expect(screen.getByLabelText('子网掩码').value).toBe('255.255.255.0');
    expect(screen.getByLabelText('网关').value).toBe('');
    expect(await screen.findByText(/同网段/)).toBeTruthy();
  });

  // 区间不在客户机网段内时：状态行点名网卡、保存置灰，一键填入可用区间。
  it('网段不在服务器客户机网卡时拦住并给出出路', async () => {
    const getNetwork = vi.fn(async () => NETWORK);
    const createGroup = vi.fn(async () => ({ ID: 'g-1' }));
    stubApi({ getNetwork, createGroup });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
    const start = await screen.findByLabelText('起始 IP');
    await userEvent.clear(start);
    await userEvent.type(start, '192.168.10.50');
    const status = await screen.findByText(/不在服务器的客户机网段/);
    expect(status.parentElement.textContent).toContain('ndbr0');
    expect(screen.getByRole('button', { name: '保存' }).disabled).toBe(true);

    await userEvent.click(screen.getByRole('button', { name: /填入 192\.168\.50\.10/ }));
    await waitFor(() => expect(screen.getByLabelText('起始 IP').value).toBe('192.168.50.10'));
    expect(await screen.findByText(/同网段/)).toBeTruthy();
    expect(screen.getByRole('button', { name: '保存' }).disabled).toBe(false);
  });

  // 允许跨网段时同样的区间算中继分组：网关栏标明是中继地址，并可探测。
  it('允许跨网段时按中继提示，网关可检测', async () => {
    const probeNetwork = vi.fn(async () => ({ ip: '192.168.10.1', reachable: true, rtt_ms: 0.4 }));
    stubApi({ getNetwork: async () => ({ ...NETWORK, allow_cross_subnet: true }), probeNetwork });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
    const start = await screen.findByLabelText('起始 IP');
    await userEvent.clear(start);
    await userEvent.type(start, '192.168.10.50');
    expect(await screen.findByText(/跨网段 · DHCP 中继/)).toBeTruthy();
    const gw = screen.getByLabelText('网关（中继地址）');
    await userEvent.type(gw, '192.168.10.1');
    await userEvent.click(screen.getByRole('button', { name: '检测' }));
    await waitFor(() => expect(probeNetwork).toHaveBeenCalledWith('192.168.10.1'));
    expect(await screen.findByText(/通/)).toBeTruthy();
    expect(screen.getByRole('button', { name: '保存' }).disabled).toBe(false);
  });

  // 已落在错误网段的分组要标出来并提供迁移，预览每台机器的新地址。
  it('不在服务器网段的存量分组可一键改到服务器网段', async () => {
    const group = { ID: 'g-1', Name: 'vmdk', StartIP: '192.168.10.50', ClientMax: 30, Gateway: '192.168.10.1', Netmask: '255.255.255.0', DNS1: '223.5.5.5', SystemImageID: 'isharedisk', SystemConfigID: 'isharedisk_default' };
    const previewGroupNetwork = vi.fn(async () => ({ status: 'same', terminals: [{ id: 't1', name: '001', mac: '00505625442C', old_ip: '192.168.10.50', new_ip: '192.168.50.50' }] }));
    const updateGroup = vi.fn(async () => group);
    stubApi({
      listGroups: async () => list([group]),
      getNetwork: async (clientMax) => ({ ...NETWORK, suggest: { start_ip: '192.168.50.50', client_max: clientMax || 30, netmask: '255.255.255.0', gateway: '' }, groups: [{ id: 'g-1', name: 'vmdk', start_ip: '192.168.10.50', client_max: 30, status: 'blocked' }] }),
      previewGroupNetwork, updateGroup,
    });
    renderPage();
    expect(await screen.findByText('不通')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: /改到服务器网段/ }));
    await waitFor(() => expect(previewGroupNetwork).toHaveBeenCalledWith('g-1', expect.objectContaining({ start_ip: '192.168.50.50', client_max: 30 })));
    expect(await screen.findByText('192.168.50.50')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '确认修改' }));
    await waitFor(() => expect(updateGroup).toHaveBeenCalledWith('g-1', expect.objectContaining({ start_ip: '192.168.50.50', gateway: '', system_image_id: 'isharedisk' })));
  });

  it('多节点时新建分组出现「存储节点」选择，提交带 storage_server_id', async () => {
    let sent = null;
    stubApi({
      listClusterNodes: async () => list([
        { id: 'node-a', name: '主机A', online: true, ha_state: 'active' },
        { id: 'node-b', name: '备机B', online: true, ha_state: 'standby' },
      ]),
      createGroup: async (body) => { sent = body; return { ID: 'g1', ...body }; },
    });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: '新建分组' }));
    const picker = await screen.findByLabelText('存储节点');
    // 默认自动均衡，运维不必先规划分组
    expect(picker.value).toBe('');
    expect(Array.from(picker.options).some(o => /自动均衡/.test(o.text))).toBe(true);
    await user.selectOptions(picker, 'node-b');
    await user.type(screen.getByLabelText('分组名称'), '放置组');
    await user.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(sent).not.toBeNull());
    expect(sent.storage_server_id).toBe('node-b');
  });

  it('单节点时不出现「存储节点」选择', async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: '新建分组' }));
    await screen.findByLabelText('分组名称');
    expect(screen.queryByLabelText('存储节点')).toBeNull();
  });

  // 新建分组预填两个公共 DNS（可改），否则客户机有网关却解析不了域名。
  it('新建分组预填两个 DNS，主备顺序按国内网络排', async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: '新建分组' }));
    await screen.findByLabelText('分组名称');

    const dns1 = screen.getByLabelText('DNS1');
    const dns2 = screen.getByLabelText('DNS2');
    expect(dns1.value).toBe('114.114.114.114');
    expect(dns2.value).toBe('223.5.5.5');
  });

  // 改网段时要如实提示：运行中的客户机续租时会换地址，Windows 的 IP 一变 iSCSI 就断。
  it('改网段的预览标出正在运行的机器，并说清后果', async () => {
    const user = userEvent.setup();
    // 分组要在客户机网段内，否则 save() 先被网段校验拦下
    stubApi({
      listGroups: async () => list([
        { ID: 'g-1', Name: '教学一班', StartIP: '192.168.50.100', ClientMax: 10, Gateway: '192.168.50.1', Netmask: '255.255.255.0', SystemImageID: 'isharedisk', SystemConfigID: 'isharedisk_default' },
      ]),
    });
    vi.spyOn(api, 'previewGroupNetwork').mockResolvedValue({
      status: 'ok', online_count: 1,
      terminals: [
        { id: 't1', name: '01号机', mac: 'AABBCCDDEE01', old_ip: '192.168.50.100', new_ip: '192.168.50.150', online: true },
        { id: 't2', name: '02号机', mac: 'AABBCCDDEE02', old_ip: '192.168.50.101', new_ip: '192.168.50.151', online: false },
      ],
    });
    renderPage();
    await user.click((await screen.findByText('教学一班')).closest('tr'));
    await user.click(await screen.findByRole('button', { name: /改网段|编辑/ }));
    await screen.findByLabelText('分组名称');
    const start = screen.getByLabelText('起始 IP');
    await user.clear(start);
    await user.type(start, '192.168.50.150');
    await user.click(screen.getByRole('button', { name: '保存' }));

    const dlg = await screen.findByText(/台终端的地址跟着换/);
    const card = dlg.closest('.card');
    // 在线那台要看得出来
    const row = Array.from(card.querySelectorAll('tbody tr')).find(tr => tr.textContent.includes('AABBCCDDEE01'));
    expect(within(row).getByText(/运行中|在线/)).toBeTruthy();
    // 后果要如实写，不能只说「下次开机生效」
    expect(card.textContent).toMatch(/续租|断线|掉线/);
  });

  // 数据盘在新建 / 编辑弹窗里一起设置，不用建完分组再单独配一遍。
  describe('数据盘随分组一起设置', () => {
    const DATA_CONFIGS = { games: [{ ID: 'games_default', ImageID: 'games', Name: 'default' }] };
    const configsOf = async (img) => list(DATA_CONFIGS[img] || CONFIGS);
    const GROUP = { ID: 'g-1', Name: '教学一班', StartIP: '192.168.50.100', ClientMax: 10, Gateway: '192.168.50.1', Netmask: '255.255.255.0', SystemImageID: 'isharedisk', SystemConfigID: 'isharedisk_default' };

    it('新建分组时一起添加数据盘，盘符默认填下一个空闲的', async () => {
      const createGroup = vi.fn(async () => ({ ID: 'g-new' }));
      const createGroupDisk = vi.fn(async () => ({}));
      stubApi({ createGroup, createGroupDisk, listConfigs: configsOf });
      renderPage();
      await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
      await userEvent.type(await screen.findByLabelText('分组名称'), '电竞区');
      await userEvent.click(screen.getByRole('button', { name: /添加数据盘/ }));
      expect(screen.getByLabelText('新数据盘 1 的挂载目标').value).toBe('D:');
      await waitFor(() => expect(screen.getByLabelText('新数据盘 1 的配置').value).toBe('games_default'));
      await userEvent.click(screen.getByRole('button', { name: '保存' }));
      await waitFor(() => expect(createGroupDisk).toHaveBeenCalledWith('g-new', { mount_target: 'D:', image_id: 'games', config_id: 'games_default' }));
      expect(createGroup).toHaveBeenCalledTimes(1);
    });

    it('编辑时列出已有数据盘，可移除旧的、添加新的', async () => {
      const updateGroup = vi.fn(async () => ({}));
      const deleteGroupDisk = vi.fn(async () => null);
      const createGroupDisk = vi.fn(async () => ({}));
      stubApi({
        listGroups: async () => list([GROUP]), listConfigs: configsOf, updateGroup, deleteGroupDisk, createGroupDisk,
        listGroupDisks: async () => list([{ ID: 'gd-1', GroupID: 'g-1', MountTarget: 'D:', ImageID: 'games', ConfigID: 'games_default' }]),
      });
      renderPage();
      await screen.findByText('教学一班');
      // 卡片直接显示数据盘，不再要单独的「数据盘」按钮。
      await waitFor(() => expect(screen.getByText('D:')).toBeTruthy());
      expect(screen.queryByRole('button', { name: /^数据盘$/ })).toBeNull();

      await userEvent.click(screen.getByRole('button', { name: /编辑/ }));
      await screen.findByLabelText('分组名称');
      await userEvent.click(screen.getByRole('button', { name: /添加数据盘/ }));
      expect(screen.getByLabelText('新数据盘 1 的挂载目标').value).toBe('E:');
      await userEvent.click(screen.getByRole('button', { name: '移除数据盘 D:' }));
      await waitFor(() => expect(screen.getByLabelText('新数据盘 1 的配置').value).toBe('games_default'));
      await userEvent.click(screen.getByRole('button', { name: '保存' }));
      await waitFor(() => expect(createGroupDisk).toHaveBeenCalledWith('g-1', { mount_target: 'E:', image_id: 'games', config_id: 'games_default' }));
      expect(updateGroup).toHaveBeenCalled();
      expect(deleteGroupDisk).toHaveBeenCalledWith('gd-1');
    });

    it('卡片只列系统盘和数据盘，网络细节留在弹窗；数据盘说明点问号才展开', async () => {
      stubApi({
        listGroups: async () => list([GROUP]), listConfigs: configsOf,
        listGroupDisks: async () => list([{ ID: 'gd-1', GroupID: 'g-1', MountTarget: 'D:', ImageID: 'games', ConfigID: 'games_default' }]),
      });
      renderPage();
      await screen.findByText('教学一班');
      await waitFor(() => expect(screen.getByText('D:')).toBeTruthy());
      expect(screen.queryByText('子网掩码')).toBeNull();
      await userEvent.click(screen.getByRole('button', { name: /编辑/ }));
      await screen.findByLabelText('分组名称');
      expect(screen.queryByText(/随系统盘一起挂载的附加盘/)).toBeNull();
      await userEvent.click(screen.getByRole('button', { name: '数据盘说明' }));
      expect(screen.getByText(/随系统盘一起挂载的附加盘/)).toBeTruthy();
    });

    // 右上角三个数各带标签：在线 / 已分配 / 容量；按钮不再带数字，免得和它们混淆。
    it('右上角是在线 / 已分配 / 容量，满员时容量标出来', async () => {
      stubApi({
        listGroups: async () => list([{ ...GROUP, ClientMax: 2 }]), listConfigs: configsOf,
        listTerminals: async () => list([
          { ID: 't1', Name: '01', MAC: 'AA', GroupID: 'g-1', State: 'online' },
          { ID: 't2', Name: '02', MAC: 'BB', GroupID: 'g-1', State: 'offline' },
        ]),
      });
      renderPage();
      const card = (await screen.findByText('教学一班')).closest('.card');
      const stat = (label) => within(card).getByText(label).parentElement.textContent;
      await waitFor(() => expect(stat('在线')).toBe('1在线'));
      expect(stat('已分配')).toBe('2已分配');
      expect(stat('容量')).toBe('2容量');
      expect(within(card).getByText('容量').parentElement.getAttribute('title')).toMatch(/已满/);
      expect(within(card).getByRole('button', { name: /查看终端/ })).toBeTruthy();
    });

    it('分组建好了但数据盘没加上：弹窗转为编辑，再保存只补加数据盘，不重复建分组', async () => {
      const createGroup = vi.fn(async () => ({ ID: 'g-new' }));
      const updateGroup = vi.fn(async () => ({}));
      let fail = true;
      const createGroupDisk = vi.fn(async () => { if (fail) throw new Error('挂载目标重复'); return {}; });
      stubApi({ createGroup, updateGroup, createGroupDisk, listConfigs: configsOf });
      renderPage();
      await userEvent.click(await screen.findByRole('button', { name: /新建分组/ }));
      await userEvent.type(await screen.findByLabelText('分组名称'), '电竞区');
      await userEvent.click(screen.getByRole('button', { name: /添加数据盘/ }));
      await waitFor(() => expect(screen.getByLabelText('新数据盘 1 的配置').value).toBe('games_default'));
      await userEvent.click(screen.getByRole('button', { name: '保存' }));
      expect(await screen.findByText(/分组已创建，数据盘 D: 未添加/)).toBeTruthy();
      expect(screen.getByText('编辑分组')).toBeTruthy();

      fail = false;
      await userEvent.click(screen.getByRole('button', { name: '保存' }));
      await waitFor(() => expect(createGroupDisk).toHaveBeenCalledTimes(2));
      expect(createGroup).toHaveBeenCalledTimes(1);
      expect(updateGroup).toHaveBeenCalled();
    });
  });
});
