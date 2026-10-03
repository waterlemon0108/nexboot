import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../lib/api.js';
import { StoreProvider } from './store.jsx';
import { ConfirmProvider } from './overlay.jsx';
import { HACard, StandbyBanner } from './ha.jsx';

// HA 卡片：本节点角色、对端是否跟随和计划切换。备机横幅给开错节点的操作者指路，
// 在写入被后端 503 拒绝之前就告诉他去哪。

const HA_SINGLE = { node_id: 'b7ef', role: 'active', epoch: 0, peer_url: '', peer_reachable: false };
// 主机上的 peer_url 是 VIP（即它自己），其余节点从节点表列出。
const HA_ACTIVE = {
  node_id: 'b7ef', role: 'active', epoch: 2,
  peer_url: 'http://192.168.50.250:8080', peer_reachable: true, peer_role: 'active', peer_epoch: 2,
  rate_mbps: 100, rate_source: 'default',
};
const NODES = { items: [
  { id: 'b7ef', ip: '192.168.50.10', ha_state: 'active', online: true },
  { id: 'n2', ip: '192.168.50.11', ha_state: 'standby', online: true },
  { id: 'n3', ip: '192.168.50.12', ha_state: 'standby', online: false },
] };
const HA_STANDBY = {
  node_id: 'node-b', role: 'standby', epoch: 1,
  peer_url: 'http://192.168.50.10:8080', peer_reachable: true, peer_role: 'active', peer_epoch: 2,
};
const REPL = { targets: [{ target: 'http://192.168.50.11:8080', kind: 'primary-serve', last_snapshot: 'rep-9', lag_seconds: 12, last_error: '' }] };

function stubApi(overrides = {}) {
  const base = {
    getHA: async () => HA_SINGLE,
    getReplication: async () => ({ targets: [] }),
    plannedSwitch: async () => ({}),
    setHARate: async () => ({}),
    createCluster: async () => ({}),
    listClusterNodes: async () => NODES,
    listTasks: async () => ({ items: [], total: 0 }),
    listAlarms: async () => ({ items: [], total: 0 }),
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

const wrap = (ui) => render(
  <StoreProvider><ConfirmProvider>{ui}</ConfirmProvider></StoreProvider>
);

describe('HACard', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  it('读不到节点列表时说读不到，而不是「暂无」备机', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, listClusterNodes: async () => { throw new Error('超时'); } });
    wrap(<HACard/>);
    const others = await screen.findByTestId('ha-others');
    await waitFor(() => expect(others.textContent).toContain('读不到节点列表'));
    expect(others.textContent).not.toContain('暂无');
  });

  it('其余节点全部离线时不标「备机」', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, listClusterNodes: async () => ({ items: [NODES.items[0], { ...NODES.items[2] }] }) });
    wrap(<HACard/>);
    const others = await screen.findByTestId('ha-others');
    await waitFor(() => expect(others.textContent).toContain('离线'));
    expect(others.parentElement.textContent).not.toContain('备机');
  });

  it('单机：说明是独立节点、给出创建集群入口，不给切换按钮', async () => {
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText(/未加入任何集群/)).toBeTruthy());
    expect(screen.getByRole('button', { name: /创建集群/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /计划切换/ })).toBeNull();
  });

  it('主机：显示角色、对端与复制滞后，计划切换需确认后调用', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    const user = userEvent.setup();
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText('主机')).toBeTruthy());
    expect(screen.getByText(/192\.168\.50\.11/)).toBeTruthy();
    expect(screen.getByText(/12 秒前/)).toBeTruthy();

    await user.click(screen.getByRole('button', { name: '计划切换' }));
    // 确认弹窗用操作者的话说明后果
    expect(await screen.findByText(/运行中的普通客户机会重启一次/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: '切换' }));
    await waitFor(() => expect(api.plannedSwitch).toHaveBeenCalled());
  });

  // 同步结果跟在「目录同步」标题后面，同步限速放在这一列下面。
  it('主机：目录同步一列包含同步时间和同步限速', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    wrap(<HACard/>);
    const title = await screen.findByText('目录同步', { exact: false });
    const col = title.closest('div').parentElement;
    await waitFor(() => expect(title.textContent).toMatch(/12 秒前/));
    expect(within(col).getByLabelText('同步限速')).toBeTruthy();
  });

  it('主机：同步限速可改，0 表示不限（运维窗口摘掉上限）', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    const user = userEvent.setup();
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText('主机')).toBeTruthy());
    const input = screen.getByLabelText('同步限速');
    expect(input.value).toBe('100');
    await user.clear(input);
    await user.type(input, '0');
    await user.click(screen.getByRole('button', { name: '保存限速' }));
    await waitFor(() => expect(api.setHARate).toHaveBeenCalledWith(0));
  });

  // 本机和其余节点一样显示 IP；任期号、节点 ID 用于排查，不放在正文里。
  it('主机：本机显示 IP，不显示 epoch 和节点 ID，说明默认收起', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    const user = userEvent.setup();
    wrap(<HACard/>);
    const self = await screen.findByTestId('ha-self');
    await waitFor(() => expect(self.textContent).toContain('192.168.50.10'));
    expect(self.textContent).not.toMatch(/epoch|b7ef/);
    // 角色和计划切换紧跟本机 IP
    expect(within(self).getByText('主机')).toBeTruthy();
    expect(within(self).getByRole('button', { name: '计划切换' })).toBeTruthy();
    expect(screen.queryByText(/维护本机前/)).toBeNull();
    await user.click(screen.getByRole('button', { name: '计划切换说明' }));
    expect(screen.getByText(/交给一台备机/)).toBeTruthy();
  });

  it('主机：列出其余节点及状态，不把虚 IP 当成对端', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText(/192\.168\.50\.11/)).toBeTruthy());
    const col = screen.getByTestId('ha-others');
    expect(col.textContent).toContain('192.168.50.11');
    expect(col.textContent).toContain('192.168.50.12');
    expect(col.textContent).toContain('离线');
    expect(col.textContent).not.toContain('192.168.50.10');
    expect(screen.queryByText(/192\.168\.50\.250/)).toBeNull();
  });

  // 集群令牌是节点间内部接口的密钥，界面上不展示，「添加节点」也不需要它。
  it('主机：界面上不出现集群令牌', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText(/192\.168\.50\.11/)).toBeTruthy());
    expect(screen.queryByText('集群令牌')).toBeNull();
    expect(screen.queryByText(/tok-abcdef123456/)).toBeNull();
  });

  it('备机：点名主机地址，不给切换按钮', async () => {
    stubApi({ getHA: async () => HA_STANDBY });
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText('备机')).toBeTruthy());
    expect(screen.getAllByText(/192\.168\.50\.10/).length).toBeGreaterThan(0);
    expect(screen.queryByRole('button', { name: /计划切换/ })).toBeNull();
  });
});

// 单机节点从这里建集群；已配对的节点不提供该入口。
describe('HACard 加入集群', () => {
  afterEach(() => vi.restoreAllMocks());

  // 手动「加入集群」需要令牌，界面上不提供；其余机器一律走「添加节点」。
  it('独立节点只给「创建集群」，没有手动加入', async () => {
    stubApi();
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText(/未加入任何集群/)).toBeTruthy());
    expect(screen.getByRole('button', { name: /创建集群/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /加入集群/ })).toBeNull();
  });

  // 建集群只问 VIP，令牌由产品生成，不让操作者编造或转抄。
  it('创建集群只填虚 IP，表单里没有令牌', async () => {
    const create = vi.fn(async () => ({ status: 'creating' }));
    stubApi({ createCluster: create });
    const user = userEvent.setup();
    wrap(<HACard/>);

    await user.click(await screen.findByRole('button', { name: /创建集群/ }));
    // 要钉住的是「没有令牌输入框」，文案里说明「令牌由产品自动生成」是应该的。
    const form = screen.getByTestId('create-cluster-form');
    expect(form.querySelectorAll('input').length).toBe(1);
    expect(screen.queryByPlaceholderText(/cluster-token/)).toBeNull();
    // 没有池也能建，但要告诉操作者池在哪建、建之前备机无法接管
    expect(form.textContent).toContain('存储池管理');
    expect(form.textContent).toContain('接管');
    await user.type(screen.getByPlaceholderText(/192\.168/), '192.168.10.250');
    await user.click(screen.getByRole('button', { name: /^创建$/ }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({ vip: '192.168.10.250' }));
  });

  it('已配对的主机不显示加入入口', async () => {
    stubApi({ getHA: async () => HA_ACTIVE, getReplication: async () => REPL });
    wrap(<HACard/>);
    await waitFor(() => expect(screen.getByText('主机')).toBeTruthy());
    expect(screen.queryByRole('button', { name: /加入集群/ })).toBeNull();
  });
});

describe('StandbyBanner', () => {
  afterEach(() => vi.restoreAllMocks());

  it('备机渲染横幅并指向主机；主机与单机不渲染', async () => {
    stubApi({ getHA: async () => HA_STANDBY });
    const { unmount } = wrap(<StandbyBanner/>);
    await waitFor(() => expect(screen.getByText(/当前是备机/)).toBeTruthy());
    expect(screen.getByRole('link').getAttribute('href')).toBe('http://192.168.50.10:8080');
    unmount();
    vi.restoreAllMocks();

    stubApi({ getHA: async () => HA_ACTIVE });
    wrap(<StandbyBanner/>);
    await new Promise(r => setTimeout(r, 50));
    expect(screen.queryByText(/当前是备机/)).toBeNull();
  });

  // 操作者经 VIP 进入一台持有 VIP 但尚未接任的备机，此时「请到主机操作」是错的，
  // 要说明为何还没接任、接下来会怎样。
  it('持有虚 IP 而未接任：说明原因，不再让运维去找主机', async () => {
    stubApi({ getHA: async () => ({ ...HA_STANDBY, holds_vip: true,
      takeover: { phase: 'refused', reason: '节点 node-a 仍是健康的主机，本节点不接任，以免丢掉它刚确认的改动；虚 IP 已让出，由它接手' } }) });
    wrap(<StandbyBanner/>);
    await waitFor(() => expect(screen.getByText(/还没有接任/)).toBeTruthy());
    expect(screen.getByText(/node-a 仍是健康的主机/)).toBeTruthy();
    expect(screen.getByText(/几分钟内/)).toBeTruthy();
    expect(screen.queryByText(/管理操作请到主机/)).toBeNull();
  });

  it('正在追平目录：说明正在接任', async () => {
    stubApi({ getHA: async () => ({ ...HA_STANDBY, holds_vip: true,
      takeover: { phase: 'catching-up', reason: '正在从节点 node-a追平目录，追平后接任（最多 3 分钟）' } }) });
    wrap(<StandbyBanner/>);
    await waitFor(() => expect(screen.getByText(/正在接任/)).toBeTruthy());
    expect(screen.getByText(/追平目录/)).toBeTruthy();
  });
});
