import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageTasks } from './tasks.jsx';

const page = (items) => ({ items, total: items.length });

function stubApi(tasks) {
  const base = {
    listTaskHistory: async () => page(tasks),
    listTerminals: async () => page([{ ID: 'terminal-S', Name: '超管机', MAC: '00505623AAFF' }]),
    listImages: async () => page([]),
  };
  for (const [name, fn] of Object.entries(base)) vi.spyOn(api, name).mockImplementation(fn);
}

const renderPage = () => render(<StoreProvider><ConfirmProvider><PageTasks/></ConfirmProvider></StoreProvider>);

afterEach(() => vi.restoreAllMocks());

// 失败原因要直接显示在列表行上，只显示「失败」和 0% 会被误认为卡住。
it('失败的任务直接显示原因，不用点开', async () => {
  stubApi([
    { ID: 'task-1', Type: 'publish_data_disk', TargetRef: '00505623AAFF', Status: 'failed', Progress: 0,
      Error: 'D: 正在写入，等它写完再发布', CreatedAt: new Date().toISOString(), FinishedAt: new Date().toISOString() },
  ]);
  renderPage();
  expect(await screen.findByText(/D: 正在写入/)).toBeTruthy();
});

// 任务名要和运维在界面上点的操作同名，比如「关机后存还原点」不能显示成「超管停止」。
it('任务名和界面上的操作对得上', async () => {
  stubApi([
    { ID: 'task-1', Type: 'super_stop', TargetRef: '00505623AAFF', Status: 'success', Progress: 100, CreatedAt: new Date().toISOString() },
    { ID: 'task-2', Type: 'publish_data_disk', TargetRef: '00505623AAFF', Status: 'success', Progress: 100, CreatedAt: new Date().toISOString() },
    { ID: 'task-3', Type: 'migrate_disk', TargetRef: 'pool-tank', Status: 'success', Progress: 100, CreatedAt: new Date().toISOString() },
  ]);
  renderPage();
  await screen.findByText('task-1');
  const rows = screen.getAllByRole('row');
  const typeOf = (id) => {
    const row = rows.find(r => within(r).queryByText(id));
    return row.querySelectorAll('td')[1].textContent;
  };
  await waitFor(() => expect(typeOf('task-1')).toBe('关机存还原点'));
  expect(typeOf('task-2')).toBe('发布数据盘');
  expect(typeOf('task-3')).toBe('移除磁盘');
});

// 类型已有单独一列，ID 列去掉类型前缀免得挤坏类型列；完整 ID 保留在 title 里。
it('任务ID不重复显示类型，完整 ID 留在悬浮提示里', async () => {
  stubApi([
    { ID: 'task-publish_data_disk-1789898065236948317', Type: 'publish_data_disk', TargetRef: '0E2EC1000B01',
      Status: 'failed', Progress: 0, Error: '配置上存在比该超管机更新的还原点 @e2e-newer，保存会将其删除，已终止',
      CreatedAt: new Date().toISOString(), FinishedAt: new Date().toISOString() },
  ]);
  renderPage();
  const cell = await screen.findByText('1789898065236948317');
  expect(cell.textContent).not.toContain('publish_data_disk');
  expect(cell.getAttribute('title')).toBe('task-publish_data_disk-1789898065236948317');
});

// 每条任务标出执行的服务器（池操作在池所在节点，其余在写入者）；读不到的节点要点名，
// 否则和「那台没任务」分不清。
it('任务列表有服务器一列，读不到的节点要说出来', async () => {
  const now = new Date().toISOString();
  const items = [
    { ID: 'task-create_pool-1', Type: 'create_pool', TargetRef: 'data', Status: 'failed', Progress: 0, Node: '192.168.10.5',
      Error: '挂载目录 /ndiskless/data 已存在且不为空，请换一个池名，或先清空该目录', CreatedAt: now },
    { ID: 'task-destroy_pool-2', Type: 'destroy_pool', TargetRef: 'pool-2f21d1f9cbfd--e2egone', Status: 'success', Progress: 100, Node: '192.168.10.4', CreatedAt: now },
    { ID: 'task-create_config-3', Type: 'create_config', TargetRef: 'vmdk_default', Status: 'success', Progress: 100, CreatedAt: now },
  ];
  vi.spyOn(api, 'listTaskHistory').mockImplementation(async () => ({ items, total: 3, unreachable: ['192.168.10.9'] }));
  vi.spyOn(api, 'listTerminals').mockImplementation(async () => page([]));
  vi.spyOn(api, 'listImages').mockImplementation(async () => page([]));
  renderPage();
  expect(await screen.findByRole('columnheader', { name: '服务器' })).toBeTruthy();
  const row = (id) => screen.getByText(id).closest('tr');
  expect(within(row('1')).getByText('192.168.10.5')).toBeTruthy();
  expect(within(row('2')).getByText('192.168.10.4')).toBeTruthy();
  expect(within(row('2')).getByText('e2egone')).toBeTruthy();   // 池的内部 ID 换成池名
  expect(row('3').querySelectorAll('td')[3].textContent).toBe('—'); // 记录节点之前的旧任务
  expect(screen.queryByText(/上执行/)).toBeNull();
  expect(screen.getByText(/192\.168\.10\.9 读不到，它上面的任务没有列出/)).toBeTruthy();
});

it('任务对象显示名称，数据集 ID 留在悬浮提示里', async () => {
  stubApi([
    { ID: 'task-x', Type: 'reduction_create', TargetRef: 'win11-_default_____179', TargetName: 'Win11 电竞版 / default / 装完显卡驱动',
      Status: 'success', Progress: 100, CreatedAt: new Date().toISOString() },
  ]);
  renderPage();
  const cell = await screen.findByText('Win11 电竞版 / default / 装完显卡驱动');
  expect(cell.getAttribute('title')).toBe('win11-_default_____179');
});
