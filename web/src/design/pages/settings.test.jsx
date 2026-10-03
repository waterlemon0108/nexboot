import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageSystemParams } from './settings.jsx';

// 客户机网络卡片：打开跨网段要经确认；仍有分组依赖时关闭会被拒，显示服务端的信息。

const list = (items) => ({ items, total: items.length });
const NETWORK = {
  interfaces: [
    { name: 'ndbr0', addrs: ['192.168.50.1/24'], up: true, client: true },
    { name: 'wlo1', addrs: ['192.168.124.56/24'], up: true, client: false },
  ],
  client_iface: 'ndbr0', client_iface_setting: '', client_networks: ['192.168.50.1/24'], server_addrs: ['192.168.50.1', '192.168.124.56'],
  boot_host: '192.168.50.1', allow_cross_subnet: false, known: true,
  suggest: { start_ip: '192.168.50.10', client_max: 30, netmask: '255.255.255.0', gateway: '' },
  groups: [
    { id: 'g-1', name: '一班', start_ip: '192.168.50.50', client_max: 30, status: 'same' },
    { id: 'g-2', name: 'vmdk', start_ip: '192.168.10.50', client_max: 30, status: 'blocked' },
  ],
};

function stubApi(overrides = {}) {
  const base = {
    getSettings: async () => ({ import_dir: '/tank/imports', default_import_dir: '/tank/imports' }),
    getNetwork: async () => NETWORK,
    saveNetwork: async (body) => ({ ...NETWORK, ...body }),
    listTasks: async () => list([]),
    listAlarms: async () => list([]),
    listPools: async () => list([]),
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageSystemParams/></ConfirmProvider></StoreProvider>,
);

describe('系统参数 · 客户机网络', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  it('显示客户机网卡、网段与各分组的网段状态', async () => {
    renderPage();
    expect(await screen.findByText('客户机网络')).toBeTruthy();
    const select = screen.getByLabelText('客户机网卡');
    // 未选择即自动探测，并当场显示探测到的网卡。
    expect(select.value).toBe('');
    expect(select.selectedOptions[0].textContent).toContain('ndbr0');
    expect(screen.getAllByText(/192\.168\.50\.1\/24/).length).toBeGreaterThan(0);
    expect(screen.getByText('vmdk')).toBeTruthy();
    expect(screen.getByText('不通')).toBeTruthy();
  });

  // 三项的说明跟在标题后面，默认收起，点「?」才展开。
  it('客户机网络的说明默认收起，点问号展开', async () => {
    renderPage();
    expect(await screen.findByText('客户机网络')).toBeTruthy();
    expect(screen.queryByText(/dnsmasq 给客户机发地址/)).toBeNull();
    const q = screen.getByRole('button', { name: '客户机网卡说明' });
    await userEvent.click(q);
    // 说明紧跟在标题和问号后面，不是掉到输入框下面
    expect(q.parentElement.textContent).toMatch(/dnsmasq 给客户机发地址/);
    expect(screen.getByRole('button', { name: '客户机网段说明' })).toBeTruthy();
    expect(screen.getByRole('button', { name: '跨网段分组说明' })).toBeTruthy();
  });

  it('打开跨网段分组要先确认', async () => {
    const saveNetwork = vi.fn(async (body) => ({ ...NETWORK, ...body }));
    stubApi({ saveNetwork });
    renderPage();
    await screen.findByText('客户机网络');
    await userEvent.click(screen.getByLabelText('允许跨网段分组（DHCP 中继）'));
    // 还没保存：确认框里写着交换机需要的配置。
    expect(saveNetwork).not.toHaveBeenCalled();
    expect(await screen.findByText(/分组网段可以和服务器不在一个网段/)).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '打开' }));
    await waitFor(() => expect(saveNetwork).toHaveBeenCalledWith({ client_iface: '', allow_cross_subnet: true }));
  });

  it('关不掉时显示服务器的原话', async () => {
    const saveNetwork = vi.fn(async () => { throw new Error('还有 1 个跨网段分组：vmdk。先把它们改到服务器网段，再关闭跨网段分组'); });
    stubApi({ getNetwork: async () => ({ ...NETWORK, allow_cross_subnet: true }), saveNetwork });
    renderPage();
    await screen.findByText('客户机网络');
    await userEvent.click(screen.getByLabelText('允许跨网段分组（DHCP 中继）'));
    await waitFor(() => expect(saveNetwork).toHaveBeenCalledWith({ client_iface: '', allow_cross_subnet: false }));
    expect((await screen.findAllByText(/还有 1 个跨网段分组：vmdk/)).length).toBeGreaterThan(0);
  });
});
