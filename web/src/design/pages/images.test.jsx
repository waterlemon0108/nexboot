import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider, useStore } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageImages } from './images.jsx';

// 镜像页操作 镜像 → 配置 → 还原点 三级模型：守住选中变化时的级联加载，以及删除确认要点名受影响对象。

const list = (items) => ({ items, total: items.length });
const IMAGES = [
  { ID: 'win11', Name: 'Win 11', OSType: 'windows', State: 'normal', Size: 32 * 1024 ** 3, Used: 8.5 * 1024 ** 3, Purpose: 'system', Origin: 'imported', CreatedAt: '2026-07-30T10:00:00Z' },
  { ID: 'ubuntu', Name: 'Ubuntu', OSType: 'linux', State: 'normal', Size: 8 * 1024 ** 3, Used: null, Purpose: 'system', Origin: 'imported', CreatedAt: '2026-07-30T10:00:00Z' },
  { ID: 'games', Name: 'Games D', OSType: 'windows', State: 'normal', Size: 200 * 1024 ** 3, Used: 0, Purpose: 'data', Origin: 'blank', CreatedAt: '2026-08-18T10:00:00Z' },
];
const CONFIGS = {
  win11: [{ ID: 'win11_default', ImageID: 'win11', Name: 'default', DefaultReductionID: 'win11_default_0', CreatedAt: '2026-07-30T10:00:00Z' }],
  ubuntu: [{ ID: 'ubuntu_default', ImageID: 'ubuntu', Name: 'default', DefaultReductionID: null, CreatedAt: '2026-07-30T10:00:00Z' }],
};
const REDUCTIONS = {
  win11_default: [
    { ID: 'win11_default_0', ConfigID: 'win11_default', Name: '@0', DisplayName: '0', Status: 'ready', CreatedAt: '2026-07-30T10:00:00Z' },
    { ID: 'win11_default_office', ConfigID: 'win11_default', Name: '@office', DisplayName: '装完office', Status: 'ready', CreatedAt: '2026-07-30T11:00:00Z' },
  ],
  ubuntu_default: [],
};

function stubApi(overrides = {}) {
  const base = {
    listImages: async () => list(IMAGES),
    listConfigs: async (imageId) => list(CONFIGS[imageId] || []),
    listReductions: async (configId) => list(REDUCTIONS[configId] || []),
    listGroups: async () => list([]),
    listImportSources: async () => ({ items: [], dir: '/tank/imports' }),
    listTasks: async () => list([]),
    listAlarms: async () => list([]),
    listPools: async () => list([]),
    listTerminals: async () => list([]),
    getImageHealth: async () => ({ Level: 'ok', Items: [] }),
    listClusterNodes: async () => list([]),
    beginUpload: async ({ file_name, size_bytes }) =>
      ({ upload_id: 'nodeA-1', file_name, size_bytes, received: 0, node_id: 'nodeA' }),
    uploadChunk: async () => ({ upload_id: 'nodeA-1', received: 0, complete: false }),
    uploadStatus: async () => ({ upload_id: 'nodeA-1', received: 0 }),
    abortUpload: async () => null,
  };
  for (const [name, fn] of Object.entries({ ...base, ...overrides })) {
    vi.spyOn(api, name).mockImplementation(fn);
  }
}

const renderPage = () => render(
  <StoreProvider><ConfirmProvider><PageImages/></ConfirmProvider></StoreProvider>,
);

// 配置和还原点在镜像详情弹窗里，从卡片打开。
async function openDetail(name = 'Win 11') {
  await userEvent.click(await screen.findByText(name));
  return screen.findByText('配置概况');
}

describe('镜像页', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  // 浏览器下载用一次性票据换普通 URL；盘对盘导出到服务器目录也要在。
  it('导出只保留浏览器下载，走一次性票据', async () => {
    const exportImageTicket = vi.fn(async () => ({ token: 'tok1', url: '/api/images/export-download?token=tok1', expires_in: 60, file_name: 'Win 11.zfs' }));
    stubApi({ exportImageTicket });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderPage();
    await openDetail();

    await userEvent.click(screen.getByRole('button', { name: /导出镜像/ }));
    await userEvent.click(screen.getByRole('button', { name: /下载到本地/ }));
    await waitFor(() => expect(exportImageTicket).toHaveBeenCalledWith('win11', false));
    expect(click).toHaveBeenCalled();

    // 盘对盘导出也在，两条并存
    await userEvent.click(screen.getByRole('button', { name: /导出镜像/ }));
    expect(screen.getByRole('button', { name: /导出到服务器目录/ })).toBeTruthy();
  });

  // 勾选压缩必须带进票据请求，否则文件名是 .zfs.gz、内容却是未压缩流。
  it('勾选 gzip 后导出请求带上压缩', async () => {
    const exportImageTicket = vi.fn(async () => ({ token: 'tok1', url: '/api/images/export-download?token=tok1', expires_in: 60, file_name: 'Win 11.zfs.gz' }));
    stubApi({ exportImageTicket });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderPage();
    await openDetail();

    await userEvent.click(screen.getByRole('button', { name: /导出镜像/ }));
    await userEvent.click(screen.getByLabelText('gzip 压缩'));
    await userEvent.click(screen.getByRole('button', { name: /下载到本地/ }));
    await waitFor(() => expect(exportImageTicket).toHaveBeenCalledWith('win11', true));
  });

  // 卡片同时显示逻辑大小和实占；实占未知时显示未知，不显示 0。
  it('镜像卡片同时给出逻辑大小和实际占用', async () => {
    renderPage();
    expect(await screen.findByText('逻辑 32.0 GiB · 实占 8.5 GiB')).toBeTruthy();
    expect(screen.getByText('逻辑 8.0 GiB · 实占 —')).toBeTruthy();
  });

  // 导入弹窗显示池剩余空间，选中的文件放不下时当场提示，不等上传或导入失败。
  it('选中的文件超过存储池可用时，当场拦住而不是传完再说', async () => {
    const beginUpload = vi.fn();
    stubApi({
      beginUpload,
      listImportSources: async () => ({ import_dir: '/var/lib/ndiskless/imports', pool_available: 20 * 1024 ** 3, items: [] }),
    });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    expect(dlg.textContent).toMatch(/存储池可用 20\.0 GiB/);

    const huge = new File(['x'], 'huge.raw');
    Object.defineProperty(huge, 'size', { value: 300 * 1024 ** 3 });
    await userEvent.upload(dlg.querySelector('input[type="file"]'), huge);
    await waitFor(() => expect(dlg.textContent).toMatch(/放不下/));
    expect(beginUpload).not.toHaveBeenCalled();
  });

  // 用途和系统两个维度的筛选，替代原来的「系统盘 / 数据盘」标签页。
  it('镜像页按用途、系统筛选', async () => {
    renderPage();
    await screen.findByText('Games D');
    await userEvent.selectOptions(screen.getByLabelText('按用途筛选'), 'data');
    await waitFor(() => expect(screen.queryByText('Win 11')).toBeNull());
    expect(screen.getByText('Games D')).toBeTruthy();
    await userEvent.selectOptions(screen.getByLabelText('按用途筛选'), 'all');
    await userEvent.selectOptions(screen.getByLabelText('按系统筛选'), 'linux');
    await waitFor(() => expect(screen.queryByText('Win 11')).toBeNull());
    expect(screen.getByText('Ubuntu')).toBeTruthy();
  });

  // 详情弹窗只读地概括配置：当前用哪个还原点、去哪里管。增删改都在配置 / 还原点页。
  it('详情弹窗概括配置，不在这里增删改', async () => {
    renderPage();
    await userEvent.click(await screen.findByText('Win 11'));
    const dialog = (await screen.findByText('配置概况')).closest('.card');
    expect(await within(dialog).findByText('default')).toBeTruthy();
    expect(within(dialog).getByText(/当前还原点 0/)).toBeTruthy();
    expect(within(dialog).queryByRole('button', { name: /新建配置|派生|合并/ })).toBeNull();
    await userEvent.click(within(dialog).getByRole('button', { name: '管理还原点 default' }));
    expect((await screen.findByLabelText('按配置筛选')).value).toBe('win11_default');
    expect(screen.getByLabelText('按镜像筛选').value).toBe('win11');
  });

  // 页面可新建空数据盘：名称、大小、文件系统。
  it('新建空数据盘镜像', async () => {
    const createBlankImage = vi.fn(async () => ({ task_id: 'task-create_blank_image-1' }));
    stubApi({ createBlankImage });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /新建数据盘/ }));
    await userEvent.type(await screen.findByLabelText('数据盘名称'), '游戏盘');
    await userEvent.clear(screen.getByLabelText('大小'));
    await userEvent.type(screen.getByLabelText('大小'), '200');
    await userEvent.selectOptions(screen.getByLabelText('文件系统'), 'ntfs');
    // 卷标可选：留空则不提交（即用名称），填写则原样提交。
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    await waitFor(() => expect(createBlankImage).toHaveBeenCalledWith({ name: '游戏盘', size_bytes: 200 * 1024 ** 3, filesystem: 'ntfs' }));

    await userEvent.click(screen.getByRole('button', { name: /新建数据盘/ }));
    await userEvent.type(await screen.findByLabelText('数据盘名称'), 'games');
    await userEvent.type(screen.getByLabelText('卷标'), ' GAMES ');
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    await waitFor(() => expect(createBlankImage).toHaveBeenLastCalledWith({ name: 'games', size_bytes: 100 * 1024 ** 3, filesystem: 'ntfs', label: 'GAMES' }));
  });

  // 导入时也可声明为数据盘。
  it('导入时可选用途', async () => {
    const importImage = vi.fn(async () => ({ task_id: 'task-import_image-1' }));
    stubApi({
      importImage,
      uploadChunk: async (id, offset, blob) => ({
        upload_id: id, received: offset + blob.size, complete: true,
        path: '/var/lib/ndiskless/imports/d.vmdk',
      }),
    });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    await userEvent.upload(dlg.querySelector('input[type="file"]'), new File(['x'], 'd.vmdk'));
    await waitFor(() => expect(dlg.textContent).toMatch(/上传完成/));
    await userEvent.selectOptions(screen.getByLabelText('用途'), 'data');
    await userEvent.click(screen.getByRole('button', { name: '导入' }));
    await waitFor(() => expect(importImage).toHaveBeenCalledWith(expect.objectContaining({
      source_path: '/var/lib/ndiskless/imports/d.vmdk', purpose: 'data', delete_source_after: true,
    })));
  });

});

const openTab = async (name) => {
  await screen.findByText('Win 11');
  await userEvent.click(screen.getByRole('button', { name: new RegExp('^' + name) }));
};

describe('配置页', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  it('列出所有镜像的配置，标出当前还原点是否最新，点还原点数去还原点页', async () => {
    renderPage();
    await openTab('配置');
    const table = await screen.findByRole('table');
    await waitFor(() => expect(within(table).getAllByRole('row').some(r => r.textContent.includes('Win 11'))).toBe(true));
    const row = within(table).getAllByRole('row').find(r => r.textContent.includes('Win 11'));
    expect(within(row).getByText('Win 11')).toBeTruthy();
    expect(within(row).getByText('0')).toBeTruthy();
    expect(within(row).getByText('非最新')).toBeTruthy();
    await userEvent.click(within(row).getByRole('button', { name: /2 个/ }));
    expect(await screen.findByText('装完office')).toBeTruthy();
  });

  it('镜像只剩一个配置时删不了，并说明为什么', async () => {
    stubApi({ listConfigs: async (imageId) => list(imageId === 'win11'
      ? [...CONFIGS.win11, { ID: 'win11_work', ImageID: 'win11', Name: 'work', DefaultReductionID: null, CreatedAt: '2026-07-31T10:00:00Z' }]
      : CONFIGS[imageId] || []) });
    renderPage();
    await openTab('配置');
    const table = await screen.findByRole('table');
    await waitFor(() => expect(within(table).getAllByRole('row').some(r => r.textContent.includes('Ubuntu'))).toBe(true));
    const buttons = within(table).getAllByRole('button', { name: /删除配置/ });
    const ubuntu = buttons.find(b => b.closest('tr').textContent.includes('Ubuntu'));
    expect(ubuntu.disabled).toBe(true);
    expect(ubuntu.title).toContain('删除镜像');
    for (const b of buttons.filter(b => b.closest('tr').textContent.includes('Win 11'))) expect(b.disabled).toBe(false);
  });

  it('数据盘也算在用：列出用它当数据盘的分组和挂载点', async () => {
    stubApi({
      listGroups: async () => list([{ ID: 'g-1', Name: '教学一班', SystemConfigID: 'win11_default' }]),
      listGroupDisks: async (gid) => list(gid === 'g-1' ? [{ ID: 'gd-1', GroupID: 'g-1', MountTarget: '/data', ConfigID: 'ubuntu_default' }] : []),
    });
    renderPage();
    await openTab('配置');
    const table = await screen.findByRole('table');
    const ubuRow = await waitFor(() => {
      const r = within(table).getAllByRole('row').find(x => x.textContent.includes('Ubuntu') && x.textContent.includes('/data'));
      expect(r).toBeTruthy();
      return r;
    });
    expect(ubuRow.textContent).toContain('教学一班');
    const winRow = within(table).getAllByRole('row').find(r => r.textContent.includes('Win 11'));
    expect(within(winRow).getByText('教学一班')).toBeTruthy();
  });

  it('被很多分组使用时只列前两个，其余收成 +N，悬停看全部', async () => {
    const groups = ['一班', '二班', '三班', '四班'].map((n, i) => ({ ID: `g-${i}`, Name: n, SystemConfigID: 'win11_default' }));
    stubApi({ listGroups: async () => list(groups), listGroupDisks: async () => list([]) });
    renderPage();
    await openTab('配置');
    const table = await screen.findByRole('table');
    const row = await waitFor(() => {
      const r = within(table).getAllByRole('row').find(x => x.textContent.includes('Win 11') && x.textContent.includes('一班'));
      expect(r).toBeTruthy();
      return r;
    });
    expect(row.textContent).toContain('二班');
    expect(row.textContent).not.toContain('三班');
    const more = within(row).getByText('+2');
    expect(more.closest('[title]').getAttribute('title')).toContain('四班');
  });

  it('新建配置：起点可以是镜像原始内容，也可以是已有配置的还原点', async () => {
    const createConfigFromImage = vi.fn(async () => ({ task_id: 't1' }));
    const forkConfig = vi.fn(async () => ({ task_id: 't2' }));
    stubApi({ createConfigFromImage, forkConfig });
    renderPage();
    await openTab('配置');
    await userEvent.click(await screen.findByRole('button', { name: '新建配置' }));
    await userEvent.type(screen.getByLabelText('配置名称'), '办公版');
    await userEvent.selectOptions(screen.getByLabelText('所属镜像'), 'win11');
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    await waitFor(() => expect(createConfigFromImage).toHaveBeenCalledWith('win11', { name: '办公版' }));

    await userEvent.click(await screen.findByRole('button', { name: '新建配置' }));
    await userEvent.type(screen.getByLabelText('配置名称'), '游戏版');
    await userEvent.selectOptions(screen.getByLabelText('所属镜像'), 'win11');
    await userEvent.click(screen.getByLabelText('已有配置的某个还原点'));
    await userEvent.selectOptions(await screen.findByLabelText('起点还原点'), 'win11_default_office');
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    await waitFor(() => expect(forkConfig).toHaveBeenCalledWith('win11_default', { name: '游戏版', reduction_id: 'win11_default_office' }));
  });
});

describe('还原点页', () => {
  beforeEach(() => stubApi());
  afterEach(() => vi.restoreAllMocks());

  it('默认列出全部还原点，每行标明镜像和配置，可按镜像筛选', async () => {
    renderPage();
    await openTab('还原点');
    const table = await screen.findByRole('table');
    await waitFor(() => expect(within(table).getByText('装完office')).toBeTruthy());
    expect(within(table).getByRole('columnheader', { name: '镜像' })).toBeTruthy();
    expect(within(table).getByRole('columnheader', { name: '配置' })).toBeTruthy();
    const row = within(table).getByText('装完office').closest('tr');
    expect(within(row).getByText('Win 11')).toBeTruthy();
    expect(within(row).getByText('default')).toBeTruthy();
    await userEvent.selectOptions(screen.getByLabelText('按镜像筛选'), 'ubuntu');
    await waitFor(() => expect(within(table).queryByText('装完office')).toBeNull());
  });

  it('新建还原点把输入的名字原样交给后端，可勾选立即应用，名字为空不提交', async () => {
    const createReduction = vi.fn(async () => ({ task_id: 'task-create_reduction-1' }));
    stubApi({ createReduction });
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    await userEvent.click(screen.getByRole('button', { name: '新建还原点' }));
    await userEvent.type(await screen.findByPlaceholderText('2026Q1-stable'), '装完CAD');
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    expect(createReduction).not.toHaveBeenCalled(); // 全部视图下没选配置
    await userEvent.selectOptions(screen.getByLabelText('所属配置'), 'win11_default');
    await userEvent.clear(screen.getByPlaceholderText('2026Q1-stable'));
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    expect(createReduction).not.toHaveBeenCalled(); // 名字为空
    await userEvent.type(screen.getByPlaceholderText('2026Q1-stable'), '装完CAD');
    await userEvent.click(screen.getByLabelText('创建后立即应用'));
    await userEvent.click(screen.getByRole('button', { name: '创建' }));
    await waitFor(() => expect(createReduction).toHaveBeenCalledWith('win11_default', { name: '装完CAD', set_current: true }));
  });

  it('一键应用', async () => {
    const applyReduction = vi.fn(async () => ({}));
    stubApi({ applyReduction });
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    expect(screen.getByText('应用中')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '应用 装完office' }));
    await waitFor(() => expect(applyReduction).toHaveBeenCalledWith('win11_default_office'));
  });

  it('另存为新镜像：填名字后提交', async () => {
    const saveReductionAsImage = vi.fn(async () => ({ task_id: 'task-copy' }));
    stubApi({ saveReductionAsImage });
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '另存为新镜像 装完office' }));
    const name = await screen.findByLabelText('新镜像名称');
    await userEvent.clear(name);
    await userEvent.type(name, '办公版');
    await userEvent.click(screen.getByRole('button', { name: '另存' }));
    await waitFor(() => expect(saveReductionAsImage).toHaveBeenCalledWith('win11_default_office', { name: '办公版' }));
  });

  it('导出为镜像文件走一次性票据', async () => {
    const exportReductionTicket = vi.fn(async () => ({ url: '/api/images/export-download?token=t', file_name: 'x.zfs' }));
    stubApi({ exportReductionTicket });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '导出为镜像文件 装完office' }));
    await userEvent.click(await screen.findByRole('button', { name: /下载到本地/ }));
    await waitFor(() => expect(exportReductionTicket).toHaveBeenCalledWith('win11_default_office', false));
  });

  it('覆盖原镜像先说清后果再做', async () => {
    const overwriteImage = vi.fn(async () => ({ task_id: 'task-merge' }));
    stubApi({ overwriteImage });
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '更多操作 0' }));
    await userEvent.click(await screen.findByRole('button', { name: '覆盖原镜像' }));
    const dialog = (await screen.findByText(/替换镜像 Win 11 的原始内容/)).closest('.card');
    expect(dialog.textContent).toContain('装完office');
    await userEvent.click(within(dialog).getByRole('button', { name: '覆盖' }));
    await waitFor(() => expect(overwriteImage).toHaveBeenCalledWith('win11_default_0'));
  });

  it('勾选后批量删除，被拒的点名原因', async () => {
    const extra = { ID: 'win11_default_wps', ConfigID: 'win11_default', Name: '@wps', DisplayName: '装完wps', Status: 'ready', CreatedAt: '2026-07-30T12:00:00Z' };
    const deleteReduction = vi.fn(async (id) => {
      if (id === 'win11_default_office') throw new Error('该还原点被派生出的配置 work 依赖，请先删除 work');
      return {};
    });
    stubApi({ deleteReduction, listReductions: async (cid) => list(cid === 'win11_default' ? [...REDUCTIONS.win11_default, extra] : REDUCTIONS[cid] || []) });
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    await userEvent.click(screen.getByLabelText('选择还原点 装完office'));
    await userEvent.click(screen.getByLabelText('选择还原点 装完wps'));
    await userEvent.click(screen.getByRole('button', { name: /删除所选/ }));
    const dialog = (await screen.findByText(/删除 2 个还原点/)).closest('.card');
    await userEvent.click(within(dialog).getByRole('button', { name: '删除' }));
    await waitFor(() => expect(deleteReduction).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/work 依赖/)).toBeTruthy();
  });

  it('当前应用的还原点删不了、也勾不上，并说明先应用别的', async () => {
    stubApi({ deleteReduction: vi.fn(async () => ({})) });
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    const box = screen.getByLabelText('选择还原点 0');
    expect(box.disabled).toBe(true);
    await userEvent.click(screen.getByLabelText('选择本页全部还原点'));
    expect(screen.getByRole('button', { name: /删除所选 \(1\)/ })).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '更多操作 0' }));
    const del = await screen.findByRole('button', { name: '删除' });
    expect(del.disabled).toBe(true);
    expect(del.title).toContain('先应用');
  });

  it('勾选后把它应用了：它不再算在「删除所选」里', async () => {
    let current = 'win11_default_0';
    stubApi({
      listConfigs: async (imageId) => list(imageId === 'win11' ? [{ ...CONFIGS.win11[0], DefaultReductionID: current }] : CONFIGS[imageId] || []),
      applyReduction: vi.fn(async (id) => { current = id; return {}; }),
      deleteReduction: vi.fn(async () => ({})),
    });
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    await userEvent.click(screen.getByLabelText('选择还原点 装完office'));
    expect(screen.getByRole('button', { name: /删除所选 \(1\)/ })).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '应用 装完office' }));
    await waitFor(() => expect(screen.getByLabelText('选择还原点 装完office').disabled).toBe(true));
    expect(screen.getByLabelText('选择还原点 装完office').checked).toBe(false);
    expect(screen.getByRole('button', { name: /删除所选/ }).disabled).toBe(true);
  });

  it('应用过的勾选不会在它解锁后自己回来', async () => {
    let current = 'win11_default_0';
    stubApi({
      listConfigs: async (imageId) => list(imageId === 'win11' ? [{ ...CONFIGS.win11[0], DefaultReductionID: current }] : CONFIGS[imageId] || []),
      applyReduction: vi.fn(async (id) => { current = id; return {}; }),
    });
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    await userEvent.click(screen.getByLabelText('选择还原点 装完office'));
    await userEvent.click(screen.getByRole('button', { name: '应用 装完office' }));
    await waitFor(() => expect(screen.getByLabelText('选择还原点 装完office').disabled).toBe(true));
    await userEvent.click(screen.getByRole('button', { name: '应用 0' }));
    await waitFor(() => expect(screen.getByLabelText('选择还原点 装完office').disabled).toBe(false));
    expect(screen.getByLabelText('选择还原点 装完office').checked).toBe(false);
  });

  it('删除还原点前点名它的牵连', async () => {
    const groups = [{ ID: 'g-1', Name: '教学一班', SystemReductionID: 'win11_default_office' }];
    stubApi({ listGroups: async () => list(groups), deleteReduction: vi.fn(async () => ({})) });
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '更多操作 装完office' }));
    await userEvent.click(await screen.findByRole('button', { name: '删除' }));
    const dialog = await screen.findByText(/还原点 装完office/);
    expect(dialog.textContent).toContain('正被分组 教学一班');
  });
});

describe('镜像卡片', () => {
  afterEach(() => vi.restoreAllMocks());

  it('系统盘在前、数据盘在后，各自按创建时间先后', async () => {
    const at = (d) => `2026-09-${d}T10:00:00Z`;
    stubApi({ listImages: async () => list([
      { ID: 'd-old', Name: '旧数据盘', OSType: 'windows', State: 'normal', Purpose: 'data', CreatedAt: at('01') },
      { ID: 's-old', Name: '旧系统盘', OSType: 'windows', State: 'normal', Purpose: 'system', CreatedAt: at('02') },
      { ID: 'd-new', Name: '新数据盘', OSType: 'linux', State: 'normal', Purpose: 'data', CreatedAt: at('30') },
      { ID: 's-new', Name: '新系统盘', OSType: 'linux', State: 'normal', Purpose: 'system', CreatedAt: at('20') },
    ]) });
    renderPage();
    await screen.findByText('新系统盘');
    const names = [...document.querySelectorAll('.card .ellip')].map(e => e.textContent).filter(n => n.endsWith('盘'));
    expect(names).toEqual(['旧系统盘', '新系统盘', '旧数据盘', '新数据盘']);
  });

  it('数据盘用磁盘图标，系统盘按 Windows / Linux 用各自的图标', async () => {
    stubApi();
    renderPage();
    await screen.findByText('Games D');
    const icon = (name) => within(screen.getByText(name).closest('.card')).getByRole('img');
    expect(icon('Games D').getAttribute('aria-label')).toBe('数据盘镜像');
    expect(icon('Win 11').getAttribute('aria-label')).toBe('Windows 系统盘镜像');
    expect(icon('Ubuntu').getAttribute('aria-label')).toBe('Linux 系统盘镜像');
    const shapes = new Set(['Games D', 'Win 11', 'Ubuntu'].map(n => icon(n).innerHTML));
    expect(shapes.size).toBe(3);
  });
});

describe('从本机上传镜像', () => {
  afterEach(() => vi.restoreAllMocks());

  it('大文件分片上传，每片都说明自己从第几字节开始', async () => {
    const chunks = [];
    stubApi({
      uploadChunk: async (id, offset, blob) => {
        chunks.push({ offset, size: blob.size });
        const received = offset + blob.size;
        return { upload_id: id, received, complete: received >= 12 };
      },
    });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    // 切到「从本机上传」
    const picker = dlg.querySelector('input[type="file"]');
    expect(picker).toBeTruthy();
    const file = new File(['0123456789ab'], 'win11.zfs', { type: 'application/octet-stream' });
    await userEvent.upload(picker, file);
    await waitFor(() => expect(chunks.length).toBeGreaterThan(0), { timeout: 4000 });
    // 分片必须首尾相接：有洞的话文件长度对得上、内容是坏的
    let expected = 0;
    for (const c of chunks) {
      expect(c.offset).toBe(expected);
      expected += c.size;
    }
    expect(expected).toBe(12);
  });

  it('传到一半断了，重来时从断点续，而不是从头', async () => {
    let failNext = true;
    const offsets = [];
    stubApi({
      // 浏览器回来先问服务器收到哪了
      uploadStatus: async () => ({ upload_id: 'nodeA-1', received: 6 }),
      uploadChunk: async (id, offset, blob) => {
        offsets.push(offset);
        if (failNext) { failNext = false; throw new Error('network'); }
        const received = offset + blob.size;
        return { upload_id: id, received, complete: received >= 12 };
      },
    });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    await userEvent.upload(dlg.querySelector('input[type="file"]'),
      new File(['0123456789ab'], 'win11.zfs'));
    await waitFor(() => expect(offsets.length).toBeGreaterThanOrEqual(2), { timeout: 5000 });
    // 第一片失败之后，第二次不该再从 0 开始
    expect(offsets[1]).toBe(6);
  });

  // 浏览器上传和服务器目录两条导入路径并存，默认是从本机上传。
  it('两条导入路径并存，默认是从本机上传', async () => {
    stubApi();
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    expect(dlg.querySelector('input[type="file"]')).toBeTruthy();
    expect(within(dlg).queryByRole('button', { name: /服务器上的文件/ })).toBeNull();
    expect(dlg.textContent).not.toMatch(/扫描目录|手动输入/);
  });

  // 上传的源文件导完要删，留着白占池空间。
  it('导入时告诉后端：这份源文件是我传的，导完可以删', async () => {
    const importImage = vi.fn(async () => ({ task_id: 't1' }));
    stubApi({
      importImage,
      uploadChunk: async (id, offset, blob) => ({
        upload_id: id, received: offset + blob.size, complete: true,
        path: '/var/lib/ndiskless/imports/win11.zfs',
      }),
    });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    await userEvent.upload(dlg.querySelector('input[type="file"]'),
      new File(['abc'], 'win11.zfs'));
    await waitFor(() => expect(dlg.textContent).toMatch(/上传完成/));
    await userEvent.click(within(dlg).getByRole('button', { name: /^导入$/ }));
    await waitFor(() => expect(importImage).toHaveBeenCalled());
    expect(importImage.mock.calls[0][0].delete_source_after).toBe(true);
  });


  // source_path 要上传完成才有：没传完时导入按钮禁用并说明在等什么，提示里不出现主机地址。
  it('上传未完成时导入按钮点不下去，并说明在等什么', async () => {
    let release;
    const held = new Promise(r => { release = r; });
    stubApi({
      uploadChunk: async (id, offset, blob) => {
        await held;
        return { upload_id: id, received: offset + blob.size, complete: true,
                 path: '/var/lib/ndiskless/imports/d.zfs.gz' };
      },
    });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');

    // 一个文件都没选：不该能点
    expect(screen.getByRole('button', { name: /^导入$/ }).disabled).toBe(true);

    await userEvent.upload(dlg.querySelector('input[type="file"]'), new File(['xy'], 'd.zfs.gz'));
    // 传输中：按钮说明自己在等什么，而不是沉默地灰着
    await waitFor(() => expect(screen.getByRole('button', { name: /上传中/ }).disabled).toBe(true));

    release();
    await waitFor(() => expect(dlg.textContent).toMatch(/上传完成/));
    await waitFor(() => expect(screen.getByRole('button', { name: /^导入$/ }).disabled).toBe(false));
  });

  it('上传提示讲结果，不讲此刻哪台是主机', async () => {
    stubApi({
      listClusterNodes: async () => list([
        { id: 'n1', ip: '192.168.10.3', ha_state: 'active', online: true },
        { id: 'n2', ip: '192.168.10.4', ha_state: 'standby', online: true },
      ]),
    });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    const hint = within(dlg).getByText(/断点续传/);
    expect(hint.textContent).toMatch(/同步到集群所有节点/);
    expect(hint.textContent).not.toMatch(/主机|192\.168/);
  });
});

// 服务器目录进出不经网络，但文件只在当前服务的那台上，界面必须写清是哪台。
describe('服务器目录进出', () => {
  afterEach(() => vi.restoreAllMocks());

  const SOURCES = {
    items: [{ name: 'win11.zfs', path: '/var/lib/ndiskless/imports/win11.zfs', size: 12884901888, format: 'zfs-send' }],
    import_dir: '/var/lib/ndiskless/imports', node: '192.168.10.3', total: 1, pool_available: 500 * 1024 ** 3,
  };

  it('导入可以从服务器目录挑文件，并写明是哪台机器的目录', async () => {
    const importImage = vi.fn(async () => ({ task_id: 't1' }));
    stubApi({ importImage, listImportSources: async () => SOURCES });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');

    await userEvent.type(within(dlg).getByPlaceholderText('Win11-Pro-2026Q1'), 'from-server');
    await userEvent.click(within(dlg).getByRole('button', { name: /服务器目录/ }));
    // 目录连同它属于哪台机器一起说出来
    expect(await within(dlg).findByText(/192\.168\.10\.3/)).toBeTruthy();
    expect(within(dlg).getByText(/\/var\/lib\/ndiskless\/imports/)).toBeTruthy();

    await userEvent.click(await within(dlg).findByText('win11.zfs'));
    await userEvent.click(screen.getByRole('button', { name: '导入' }));
    await waitFor(() => expect(importImage).toHaveBeenCalledWith(expect.objectContaining({
      source_path: '/var/lib/ndiskless/imports/win11.zfs',
    })));
    // 服务器目录里的文件是运维放的，导完不能删
    expect(importImage.mock.calls[0][0].delete_source_after).toBeFalsy();
  });

  it('导出可以落到服务器目录，回来告诉你在哪台的哪个路径', async () => {
    const exportImage = vi.fn(async () => ({ task_id: 't2', path: '/var/lib/ndiskless/imports/Win 11.zfs', node: '192.168.10.3' }));
    stubApi({ exportImage, listImportSources: async () => SOURCES });
    renderPage();
    await openDetail();
    await userEvent.click(screen.getByRole('button', { name: /导出镜像/ }));
    await userEvent.click(screen.getByRole('button', { name: /导出到服务器目录/ }));
    await waitFor(() => expect(exportImage).toHaveBeenCalledWith('win11'));
  });
});

// 界面承诺「重选同一文件即可接着传」：重选时要带上修改时间让服务端认出同一份，进度条从断点起画。
describe('重新选文件要能接着传', () => {
  afterEach(() => vi.restoreAllMocks());

  it('报上改动时间，并从服务端给的断点续起', async () => {
    // 文件 10 字节、服务端已收 4 字节：续传要从第 4 字节接着发
    const beginUpload = vi.fn(async () => ({
      upload_id: 'nodeA-abc', received: 4, node_id: 'nodeA',
    }));
    const uploadChunk = vi.fn(async (id, offset, blob) => ({
      upload_id: id, received: offset + blob.size, complete: true,
      path: '/var/lib/ndiskless/imports/win11.zfs.gz',
    }));
    stubApi({ beginUpload, uploadChunk });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');

    const file = new File(['0123456789'], 'win11.zfs.gz');
    Object.defineProperty(file, 'lastModified', { value: 1724400000000 });
    await userEvent.upload(dlg.querySelector('input[type="file"]'), file);

    await waitFor(() => expect(beginUpload).toHaveBeenCalledWith(expect.objectContaining({
      file_name: 'win11.zfs.gz', last_modified: 1724400000000,
    })));
    // 续传：第一片从服务端说的那个偏移开始，而不是 0
    await waitFor(() => expect(uploadChunk).toHaveBeenCalled());
    expect(uploadChunk.mock.calls[0][1]).toBe(4);
  });
});

// 从 ⌘K 或总览深链进来的详情只自动打开一次，之后 ×、ESC 都要能关掉。
describe('深链打开镜像详情', () => {
  afterEach(() => vi.restoreAllMocks());

  function DeepLink() {
    const { goto } = useStore();
    React.useEffect(() => { goto('images', { imageId: 'win11' }); }, []); // eslint-disable-line
    return null;
  }

  it('深链打开的详情点 × 能关掉，按 ESC 也能关掉', async () => {
    stubApi();
    render(<StoreProvider><ConfirmProvider><DeepLink/><PageImages/></ConfirmProvider></StoreProvider>);
    const title = await screen.findByText('配置概况');
    const dlg = title.closest('.card');
    await userEvent.click(within(dlg.querySelector('.card-h')).getByRole('button'));
    await waitFor(() => expect(screen.queryByText('配置概况')).toBeNull());
    // 关掉后再等几轮渲染，不能被深链重新弹出来
    await new Promise(r => setTimeout(r, 50));
    expect(screen.queryByText('配置概况')).toBeNull();

    await openDetail();
    await userEvent.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByText('配置概况')).toBeNull());
  });
});

// 放弃或关掉导入弹窗后，上传循环必须停下，在途分片的失败也不能再冒出报错。
describe('放弃上传', () => {
  afterEach(() => vi.restoreAllMocks());

  const startUpload = async () => {
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    await userEvent.upload(dlg.querySelector('input[type="file"]'), new File(['x'.repeat(1000)], 'win11.zfs'));
    return dlg;
  };
  // 服务端每片只认 1 字节，让循环足够长，好观察它停没停。
  const slowChunk = (calls) => async (id, offset) => {
    calls.push(offset);
    await new Promise(r => setTimeout(r, 5));
    return { upload_id: id, received: offset + 1, complete: false };
  };
  const settle = () => new Promise(r => setTimeout(r, 80));

  it('点放弃后不再发分片，并通知服务端作废', async () => {
    const calls = [];
    const abortUpload = vi.fn(async () => null);
    stubApi({ uploadChunk: slowChunk(calls), abortUpload });
    await startUpload();
    await waitFor(() => expect(calls.length).toBeGreaterThan(2));
    await userEvent.click(screen.getByRole('button', { name: '放弃' }));
    await settle();
    const n = calls.length;
    await settle();
    expect(calls.length).toBe(n);
    expect(abortUpload).toHaveBeenCalledWith('nodeA-1');
  });

  it('关掉导入弹窗后不再发分片', async () => {
    const calls = [];
    stubApi({ uploadChunk: slowChunk(calls) });
    const dlg = await startUpload();
    await waitFor(() => expect(calls.length).toBeGreaterThan(2));
    await userEvent.click(within(dlg).getByRole('button', { name: '取消' }));
    await settle();
    const n = calls.length;
    await settle();
    expect(calls.length).toBe(n);
  });

  it('放弃时中止在途分片，之后它失败也不报错', async () => {
    const inflight = [];
    stubApi({
      uploadChunk: (id, offset, blob, signal) => new Promise((resolve, reject) => {
        inflight.push({ signal, reject });
      }),
      uploadStatus: async () => { throw new Error('上传会话已失效'); },
    });
    await startUpload();
    await waitFor(() => expect(inflight.length).toBe(1));
    await userEvent.click(screen.getByRole('button', { name: '放弃' }));
    expect(inflight[0].signal && inflight[0].signal.aborted).toBe(true);
    inflight[0].reject(new Error('network'));
    await settle();
    expect(inflight.length).toBe(1);
    expect(screen.queryByText(/上传会话已失效|network/)).toBeNull();
  });
});
