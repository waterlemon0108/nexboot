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
    listInstallations: async () => list([]),
    listInstallationSources: async () => ({ items: [{name:'ubuntu.iso',path:'/tank/imports/ubuntu.iso',size_bytes:5*1024**3}], import_dir:'/tank/imports' }),
    inspectInstallationISO: async () => ({path:'/tank/imports/ubuntu.iso',fingerprint:'sha',os_type:'linux',distribution:'ubuntu-desktop',version:'24.04',architecture:'amd64',editions:[],recommended_size_gib:64}),
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

  // 安装中的镜像就是镜像列表里的一张卡片，失败时卡片上直接给出处理按钮。
  const FAILED = {id:'install-old', task_id:'task-install-old', name:'win11-lab', phase:'failed', os_type:'windows',
    error:'准备安装环境失败：无法提取 WinRE', source_path:'/tank/imports/win11.iso', source_sha256:'old-sha',
    pool_id:'pool-1', size_gib:128, edition_index:6, username:'tester',
    mac:'0050562EF64E', ip:'192.168.50.10', netmask:'255.255.255.0', gateway:'192.168.50.1', dns1:'192.168.50.1'};
  const WAITING = {id:'install-1', task_id:'task-install-1', name:'Ubuntu 新镜像', phase:'waiting', os_type:'linux',
    mac:'7070FC0587A2', progress:0, message:'安装资源已准备好，请启动客户机'};
  const cardOf = async (name) => (await screen.findByText(name)).closest('[data-install-card]');

  it('安装中的镜像以卡片出现在镜像列表，已完成和已取消的不出现，没有单独的安装任务入口', async () => {
    stubApi({listInstallations:async()=>list([FAILED, WAITING,
      {id:'install-ready',name:'已完成安装',phase:'ready'},
      {id:'install-cancelled',name:'已取消安装',phase:'cancelled'}])});
    renderPage();
    expect(await screen.findByText('Win 11')).toBeTruthy();
    const failed = await cardOf(FAILED.name);
    expect(within(failed).getByText(FAILED.error)).toBeTruthy();
    expect(within(failed).getByRole('button',{name:'重试'})).toBeTruthy();
    expect(within(failed).getByRole('button',{name:'放弃'})).toBeTruthy();
    const waiting = await cardOf(WAITING.name);
    expect(within(waiting).getByText(WAITING.message)).toBeTruthy();
    expect(within(waiting).queryByRole('button',{name:'重试'})).toBeNull();
    expect(screen.queryByText('已完成安装')).toBeNull();
    expect(screen.queryByText('已取消安装')).toBeNull();
    expect(screen.queryByRole('button',{name:/安装任务/})).toBeNull();
  });

  it('安装卡片跟随用途、系统筛选和搜索', async () => {
    stubApi({listInstallations:async()=>list([WAITING])});
    renderPage();
    await cardOf(WAITING.name);
    await userEvent.selectOptions(screen.getByLabelText('按系统筛选'), 'windows');
    expect(screen.queryByText(WAITING.name)).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText('按系统筛选'), 'all');
    await userEvent.selectOptions(screen.getByLabelText('按用途筛选'), 'data');
    expect(screen.queryByText(WAITING.name)).toBeNull();
  });

  it('安装进度读取失败时在列表上方提示并可重试', async () => {
    let failed = true;
    stubApi({listInstallations:async()=>{if (failed) throw Error('节点暂时不可达'); return list([WAITING]);}});
    renderPage();
    expect((await screen.findByRole('alert',{name:'安装进度读取失败'})).textContent).toContain('节点暂时不可达');
    failed = false;
    await userEvent.click(within(screen.getByRole('alert',{name:'安装进度读取失败'})).getByRole('button',{name:'重试'}));
    expect(await cardOf(WAITING.name)).toBeTruthy();
    expect(screen.queryByRole('alert',{name:'安装进度读取失败'})).toBeNull();
  });

  it('空环境可以从 ISO 创建系统镜像，不要求已有镜像或分组', async () => {
    let created = false;
    const createInstallation=vi.fn(async()=>{created = true; return {session_id:'install-1',task_id:'task-install-1',image_id:'iso-1'};});
    stubApi({listImages:async()=>list([]),listPools:async()=>list([{ID:'pool-1',Name:'tank',ServerID:'node-a',Role:'data'},{ID:'backup',Name:'备份池',Role:'backup'}]),createInstallation,
      listInstallations:async()=>list(created ? [{id:'install-1',name:'ubuntu',phase:'waiting',os_type:'linux',message:'请启动所选客户机进行 UEFI 网络启动'}] : [])});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    expect(await screen.findByText(/^Ubuntu 24.04/)).toBeTruthy();
    expect(screen.queryByLabelText('存储池')).toBeNull();
    await userEvent.clear(screen.getByLabelText('初始账户'));
    await userEvent.type(screen.getByLabelText('初始账户'),'tester');
    await userEvent.type(screen.getByLabelText('初始密码'),'Password123!');
    await userEvent.click(screen.getByRole('button',{name:'下一步'}));
    await userEvent.type(screen.getByLabelText('客户机 MAC'),'70:70:FC:05:87:A2');
    await userEvent.type(screen.getByLabelText('安装 IP'),'192.168.50.10');
    await userEvent.click(screen.getByRole('button',{name:'创建安装任务'}));
    await waitFor(()=>expect(createInstallation).toHaveBeenCalledWith(expect.objectContaining({source_path:'/tank/imports/ubuntu.iso',source_fingerprint:'sha',mac:'70:70:FC:05:87:A2',ip:'192.168.50.10',username:'tester',password:'Password123!'})));
    expect(within(await cardOf('ubuntu')).getByText('请启动所选客户机进行 UEFI 网络启动')).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryByRole('button',{name:'创建安装任务'})).toBeNull();
  });

  it('初始密码不够 8 位时在输入框下直接说差几位，不提交', async () => {
    const createInstallation=vi.fn();
    stubApi({listImages:async()=>list([]),listPools:async()=>list([{ID:'pool-1',Name:'tank',ServerID:'node-a',Role:'data'}]),createInstallation,listInstallations:async()=>list([])});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    expect(await screen.findByText(/^Ubuntu 24.04/)).toBeTruthy();
    expect(screen.getByText(/8 至 127 位/)).toBeTruthy();
    await userEvent.type(screen.getByLabelText('初始密码'),'123456');
    expect(screen.getByText('至少 8 位，现在 6 位')).toBeTruthy();
    await userEvent.click(screen.getByRole('button',{name:'下一步'}));
    expect(await screen.findByText('初始密码至少 8 位，现在 6 位')).toBeTruthy();
    expect(screen.queryByLabelText('客户机 MAC')).toBeNull();
    expect(createInstallation).not.toHaveBeenCalled();
  });

  it('点安装卡片打开详情：阶段条标出当前阶段，不显示虚假的 0% 或镜像就绪', async () => {
    stubApi({listImages:async()=>list([]),listInstallations:async()=>list([WAITING]),getInstallation:async()=>WAITING});
    renderPage();
    await userEvent.click(await screen.findByText(WAITING.name));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText(WAITING.message)).toBeTruthy();
    expect(within(dialog).getByText('等待开机').closest('[aria-current]').getAttribute('aria-current')).toBe('step');
    expect(within(dialog).queryByText('0%')).toBeNull();
    expect(within(dialog).queryByText('就绪')).toBeNull();
  });

  it('失败卡片点重试：只要求重新输入密码，原地清理后沿用原设置重新安装，不进向导', async () => {
    const calls = [];
    const cancel = vi.fn(async (id) => { calls.push('cancel'); return {...FAILED, id, phase:'cancelled'}; });
    const create = vi.fn(async () => { calls.push('create'); return {session_id:'install-new',task_id:'task-install-new'}; });
    const NEW = {...FAILED, id:'install-new', phase:'preparing', error:'', message:'正在准备安装资源'};
    stubApi({listInstallations:async()=>list(calls.includes('create') ? [NEW] : [FAILED]), cancelInstallation:cancel, createInstallation:create,
      listInstallationSources:async()=>({items:[{name:'win11.iso',path:FAILED.source_path,size_bytes:5*1024**3}], import_dir:'/tank/imports'}),
      inspectInstallationISO:async(path)=>({path, fingerprint:'fp-now', os_type:'windows', editions:[{index:6,name:'Windows 11 Pro'}]}),
      getInstallation:async(id)=>id === 'install-new' ? NEW : FAILED});
    renderPage();
    await userEvent.click(within(await cardOf(FAILED.name)).getByRole('button',{name:'重试'}));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/win11\.iso/)).toBeTruthy();
    expect(within(dialog).getByText(/00:50:56:2E:F6:4E/)).toBeTruthy();
    const start = within(dialog).getByRole('button',{name:'开始安装'});
    expect(start.disabled).toBe(true);
    await userEvent.type(within(dialog).getByLabelText('初始密码'), 'pw-retry-0001');
    await userEvent.click(start);
    // 重新识别拿指纹（几秒）；原会话的校验值一并带上，复制 ISO 时核对文件没被换掉。
    await waitFor(()=>expect(create).toHaveBeenCalledWith({source_path:FAILED.source_path, source_fingerprint:'fp-now', source_sha256:'old-sha', edition_index:6,
      name:'win11-lab', size_gib:128, mac:FAILED.mac, ip:FAILED.ip, netmask:FAILED.netmask,
      gateway:FAILED.gateway, dns1:FAILED.dns1, username:'tester', password:'pw-retry-0001'}));
    expect(cancel).toHaveBeenCalledWith('install-old');
    expect(calls).toEqual(['cancel','create']);
    // 重新开始后关掉详情，进度回到卡片上看，不再弹出新的详情框。
    await waitFor(()=>expect(screen.queryByRole('dialog')).toBeNull());
    expect(within(await cardOf(FAILED.name)).getByText('正在准备安装资源')).toBeTruthy();
    expect(screen.queryByRole('button',{name:'识别并继续'})).toBeNull();
  });

  it('重试前发现原安装文件不在了：不清理原安装，提示换文件', async () => {
    const cancel = vi.fn(async () => ({...FAILED, phase:'cancelled'}));
    const create = vi.fn();
    stubApi({listInstallations:async()=>list([FAILED]), cancelInstallation:cancel, createInstallation:create, getInstallation:async()=>FAILED,
      listInstallationSources:async()=>({items:[], import_dir:'/tank/imports'})});
    renderPage();
    await userEvent.click(within(await cardOf(FAILED.name)).getByRole('button',{name:'重试'}));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText('初始密码'), 'pw-retry-0001');
    await userEvent.click(within(dialog).getByRole('button',{name:'开始安装'}));
    const alert = await within(dialog).findByRole('alert');
    expect(alert.textContent).toContain('/tank/imports/win11.iso');
    expect(alert.textContent).toContain('修改设置');
    expect(cancel).not.toHaveBeenCalled();
    expect(create).not.toHaveBeenCalled();
  });

  it('重试时客户机还开着：停在清理这一步并点名客户机，不创建新安装', async () => {
    const create = vi.fn();
    stubApi({listInstallations:async()=>list([FAILED]), createInstallation:create, getInstallation:async()=>FAILED,
      cancelInstallation:async()=>({...FAILED, phase:'cancelling'}),
      listInstallationSources:async()=>({items:[{name:'win11.iso',path:FAILED.source_path,size_bytes:1}]})});
    renderPage();
    await userEvent.click(within(await cardOf(FAILED.name)).getByRole('button',{name:'重试'}));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText('初始密码'), 'pw-retry-0001');
    await userEvent.click(within(dialog).getByRole('button',{name:'开始安装'}));
    expect((await within(dialog).findByRole('alert')).textContent).toContain('请先关闭客户机 00:50:56:2E:F6:4E');
    expect(create).not.toHaveBeenCalled();
  });

  it.each([false, true])('修改设置进入向导第 2 步沿用原设置，跨节点=%s 也不用管存储池，创建前先清理原安装', async (moved) => {
    const session = {...FAILED, pool_id:moved ? 'old-pool' : 'pool-1'};
    const calls = [];
    const cancel = vi.fn(async () => { calls.push('cancel'); return {...session, phase:'cancelled'}; });
    const create = vi.fn(async () => { calls.push('create'); return {session_id:'install-new',task_id:'task-install-new'}; });
    stubApi({listInstallations:async()=>list([session]), cancelInstallation:cancel, createInstallation:create, getInstallation:async()=>session,
      listClusterNodes:async()=>list([{id:'old',ha_state:'active'},{id:'current',is_self:true,ha_state:'active'}]),
      listPools:async()=>list([{ID:'pool-1',Name:'tank',Role:'data',ServerID:'current'},{ID:'old-pool',Name:'old-tank',Role:'data',ServerID:'old'}]),
      listInstallationSources:async()=>({items:[{name:'win11.iso',path:session.source_path,size_bytes:5*1024**3}]}),
      inspectInstallationISO:async()=>({fingerprint:'new-sha',os_type:'windows',version:'11',recommended_size_gib:64,
        editions:[{index:1,name:'Home'},{index:6,name:'Pro'}]})});
    renderPage();
    await userEvent.click(within(await cardOf(session.name)).getByRole('button',{name:'重试'}));
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button',{name:'修改设置'}));
    expect(await screen.findByLabelText('镜像名称')).toBeTruthy();
    expect(screen.queryByRole('button',{name:'识别并继续'})).toBeNull();
    expect(screen.getByLabelText('镜像名称').value).toBe('win11-lab');
    expect(screen.getByLabelText('系统盘容量').value).toBe('128');
    expect(screen.getByLabelText('系统版本').value).toBe('6');
    expect(screen.getByLabelText('初始账户').value).toBe('tester');
    expect(screen.getByLabelText('初始密码').value).toBe('');
    expect(cancel).not.toHaveBeenCalled();
    await userEvent.type(screen.getByLabelText('初始密码'),'pw-retry-0002');
    await userEvent.click(screen.getByRole('button',{name:'下一步'}));
    expect(screen.getByLabelText('客户机 MAC').value).toBe('0050562EF64E');
    expect(screen.getByLabelText('安装网关').value).toBe('192.168.50.1');
    await userEvent.click(screen.getByRole('button',{name:'创建安装任务'}));
    await waitFor(()=>expect(create).toHaveBeenCalledWith(expect.objectContaining({source_path:session.source_path,source_fingerprint:'new-sha',name:'win11-lab',size_gib:128,edition_index:6,mac:session.mac,ip:session.ip,netmask:session.netmask,gateway:session.gateway,dns1:session.dns1,username:'tester',password:'pw-retry-0002'})));
    expect(calls).toEqual(['cancel','create']);
    expect(create.mock.calls[0][0]).not.toHaveProperty('pool_id');
  });

  it('修改设置直接进第 2 步：重新识别期间说明在识别，识别完显示原设置', async () => {
    let finish;
    stubApi({listInstallations:async()=>list([FAILED]), getInstallation:async()=>FAILED,
      listInstallationSources:async()=>({items:[{name:'win11.iso',path:FAILED.source_path,size_bytes:5*1024**3}]}),
      inspectInstallationISO:()=>new Promise(r=>{finish=()=>r({fingerprint:'sha2',os_type:'windows',version:'11',recommended_size_gib:64,editions:[{index:1,name:'Home'}]});})});
    renderPage();
    await userEvent.click(within(await cardOf(FAILED.name)).getByRole('button',{name:'重试'}));
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button',{name:'修改设置'}));
    const status = await screen.findByRole('status');
    expect(status.textContent).toMatch(/正在重新识别安装文件/);
    expect(within(screen.getByRole('list',{name:'创建步骤'})).getByText(/设置系统/).getAttribute('aria-current')).toBe('step');
    expect(screen.queryByLabelText('镜像名称')).toBeNull();
    finish();
    expect(await screen.findByLabelText('镜像名称')).toBeTruthy();
    expect(screen.queryByText(/正在重新识别安装文件/)).toBeNull();
  });

  it('修改设置时原安装文件识别不了：停在第 1 步说明原因', async () => {
    stubApi({listInstallations:async()=>list([FAILED]), getInstallation:async()=>FAILED,
      inspectInstallationISO:async()=>{throw Error('找不到安装文件');}});
    renderPage();
    await userEvent.click(within(await cardOf(FAILED.name)).getByRole('button',{name:'重试'}));
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button',{name:'修改设置'}));
    expect((await screen.findByRole('alert')).textContent).toContain('找不到安装文件');
    expect(screen.getByRole('button',{name:'识别并继续'})).toBeTruthy();
  });

  it('放弃安装在同一个详情里确认，不再叠一层弹窗', async () => {
    const cancel = vi.fn(async () => ({...FAILED, phase:'cancelled'}));
    stubApi({listInstallations:async()=>list([FAILED]), cancelInstallation:cancel, getInstallation:async()=>FAILED});
    renderPage();
    await userEvent.click(within(await cardOf(FAILED.name)).getByRole('button',{name:'放弃'}));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/原 ISO 文件保留/)).toBeTruthy();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await userEvent.click(within(dialog).getByRole('button',{name:'确认放弃'}));
    await waitFor(()=>expect(cancel).toHaveBeenCalledWith('install-old'));
    await waitFor(()=>expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('服务器目录里选中的文件要标出来，别的不标', async () => {
    stubApi({listInstallationSources:async()=>({items:[
      {name:'ubuntu.iso',path:'/tank/imports/ubuntu.iso',size_bytes:5*1024**3},
      {name:'win11.iso',path:'/tank/imports/win11.iso',size_bytes:7*1024**3}], import_dir:'/tank/imports'})});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    const ubuntu = await screen.findByRole('radio',{name:/ubuntu\.iso/});
    const win = screen.getByRole('radio',{name:/win11\.iso/});
    expect(ubuntu.getAttribute('aria-checked')).toBe('false');
    await userEvent.click(win);
    expect(win.getAttribute('aria-checked')).toBe('true');
    expect(ubuntu.getAttribute('aria-checked')).toBe('false');
  });

  it('不让选存储池：只说明装到当前主机的数据池，别台同名池不算', async () => {
    const createInstallation = vi.fn(async () => ({session_id:'install-1',task_id:'task-install-1'}));
    stubApi({createInstallation,
      listClusterNodes:async()=>list([{id:'node-3',ha_state:'active'},{id:'node-4',ha_state:'standby'},{id:'node-5',is_self:true,ha_state:'active'}]),
      listPools:async()=>list([{ID:'pool-node-4--tank',Name:'tank',Role:'data',ServerID:'node-4',Capacity:900*1024**3,Used:0},{ID:'pool-node-5--tank',Name:'tank',Role:'data',ServerID:'node-5',Capacity:500*1024**3,Used:100*1024**3}])});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    await screen.findByLabelText('镜像名称');
    expect(screen.queryByLabelText('存储池')).toBeNull();
    expect(screen.getByText(/安装到当前主机的数据池「tank」，可用 400\.0 GiB/)).toBeTruthy();
  });

  it('当前主机还没有数据池时直接说明，不让往下走', async () => {
    stubApi({listPools:async()=>list([{ID:'backup',Name:'backup',Role:'backup'}])});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    expect(await screen.findByText(/当前主机还没有数据池/)).toBeTruthy();
    await userEvent.type(screen.getByLabelText('初始密码'),'pw-nopool-001');
    await userEvent.click(screen.getByRole('button',{name:'下一步'}));
    expect((await screen.findByRole('alert')).textContent).toContain('存储池管理');
  });

  it('创建失败的报错显示在按钮附近，不在滚出视野的弹窗顶部', async () => {
    stubApi({listPools:async()=>list([{ID:'pool-1',Name:'tank',Role:'data'}]),
      createInstallation:async()=>{throw Error('请先关闭客户机并退出超管，再开始安装：7070FC0587A2');}});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    await userEvent.type(await screen.findByLabelText('初始密码'),'pw-create-0001');
    await userEvent.click(screen.getByRole('button',{name:'下一步'}));
    await userEvent.type(screen.getByLabelText('客户机 MAC'),'70:70:FC:05:87:A2');
    await userEvent.type(screen.getByLabelText('安装 IP'),'192.168.50.10');
    await userEvent.click(screen.getByRole('button',{name:'创建安装任务'}));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('请先关闭客户机');
    const dns = screen.getByLabelText('安装 DNS');
    expect(dns.compareDocumentPosition(alert) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('Windows Server ISO 可以装：标题写系统名不重复，Server Core 版本标出无图形界面', async () => {
    stubApi({listPools:async()=>list([{ID:'pool-1',Name:'tank',Role:'data'}]),
      inspectInstallationISO:async()=>({fingerprint:'srv',os_type:'windows',version:'Windows Server 2022',architecture:'amd64',recommended_size_gib:64,
        editions:[{index:1,name:'Windows Server 2022 Standard',core:true},{index:2,name:'Windows Server 2022 Standard (Desktop Experience)'}]})});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    expect(screen.getByText(/Windows Server 2016 及以上/)).toBeTruthy();
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    expect(await screen.findByText('Windows Server 2022 · x64')).toBeTruthy();
    expect(screen.queryByText(/Windows Windows/)).toBeNull();
    const labels = within(screen.getByLabelText('系统版本')).getAllByRole('option').map(o => o.textContent);
    expect(labels[0]).toContain('无图形界面');
    expect(labels[1]).not.toContain('无图形界面');
    // 默认选带图形界面的版本：Server Core 要运维主动选。
    expect(screen.getByLabelText('系统版本').value).toBe('2');
  });

  it('ISO 识别失败保留选择步骤并展示服务端可执行提示', async () => {
    stubApi({inspectInstallationISO:async()=>{throw Error('首期仅支持 Ubuntu 24.04 LTS，请重新选择官方 ISO');}});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent','首期仅支持 Ubuntu 24.04 LTS，请重新选择官方 ISO');
    expect(screen.getByRole('button',{name:'识别并继续'})).toBeTruthy();
    expect(screen.queryByLabelText('初始密码')).toBeNull();
  });

  it('Windows ISO 兼容性提示随版本切换且不阻止提交，失败时保留表单', async () => {
    const createInstallation=vi.fn(async()=>{throw Error('请先关闭客户机');});
    const warning='检测到 Windows 11 版本 26100.7704，可能存在 iSCSI 启动兼容性问题；仍可继续安装，并以实际开机验证为准。';
    stubApi({listPools:async()=>list([{ID:'pool-1',Name:'tank',Role:'data'}]),createInstallation,
      inspectInstallationISO:async()=>({fingerprint:'win-hash',os_type:'windows',version:'Windows 11',editions:[{index:1,name:'Windows 11 Home'},{index:6,name:'Windows 11 Pro',warning}],recommended_size_gib:64})});
    renderPage();
    await userEvent.click(await screen.findByRole('button',{name:'从 ISO 创建'}));
    await userEvent.click(screen.getByRole('button',{name:'从服务器目录'}));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button',{name:'识别并继续'}));
    await userEvent.selectOptions(await screen.findByLabelText('系统版本'),'6');
    expect(screen.getByRole('status',{name:'ISO 兼容性提示'}).textContent).toContain(warning);
    await userEvent.selectOptions(screen.getByLabelText('系统版本'),'1');
    expect(screen.queryByRole('status',{name:'ISO 兼容性提示'})).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText('系统版本'),'6');
    await userEvent.type(screen.getByLabelText('初始密码'),'Password123!');
    await userEvent.click(screen.getByRole('button',{name:'下一步'}));
    expect(screen.getByRole('status',{name:'ISO 兼容性提示'}).textContent).toContain(warning);
    await userEvent.type(screen.getByLabelText('客户机 MAC'),'00:11:22:33:44:55');
    await userEvent.type(screen.getByLabelText('安装 IP'),'192.168.50.10');
    await userEvent.click(screen.getByRole('button',{name:'创建安装任务'}));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent','请先关闭客户机');
    expect(createInstallation).toHaveBeenCalledWith(expect.objectContaining({edition_index:6,source_fingerprint:'win-hash'}));
    expect(screen.getByLabelText('客户机 MAC').value).toBe('00:11:22:33:44:55');
  });

  // 浏览器下载用一次性票据换普通 URL；盘对盘导出到服务器目录也要在。
  it('导出只保留浏览器下载，走一次性票据', async () => {
    const exportImageTicket = vi.fn(async () => ({ token: 'tok1', url: '/api/images/export-download?token=tok1', expires_in: 60, file_name: 'Win 11.zfs' }));
    stubApi({ exportImageTicket });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderPage();
    await openDetail();

    await userEvent.click(screen.getByRole('button', { name: /导出镜像/ }));
    expect(screen.getByText(/导出内容为镜像导入时的初始数据/)).toBeTruthy();
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

  it('导入目录放不下时上传前就拦住，说明哪个目录、差多少、要清理存储池', async () => {
    const beginUpload = vi.fn();
    stubApi({
      beginUpload,
      listImportSources: async () => ({ import_dir: '/var/lib/ndiskless/imports', import_dir_free: 10 * 1024 ** 3, pool_available: 95 * 1024 ** 3, items: [] }),
    });
    renderPage();
    await screen.findByText('Win 11');
    await userEvent.click(screen.getByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    // 两个空间各自标明是什么，免得看起来矛盾
    expect(dlg.textContent).toMatch(/导入目录 \/var\/lib\/ndiskless\/imports 可用 10\.0 GiB · 存储池可用 95\.0 GiB/);
    const big = new File(['x'], 'big.zfs.gz');
    Object.defineProperty(big, 'size', { value: 11 * 1024 ** 3 });
    await userEvent.upload(dlg.querySelector('input[type="file"]'), big);
    await waitFor(() => expect(dlg.textContent).toMatch(/导入目录空间不足：\/var\/lib\/ndiskless\/imports 剩余 10\.0 GiB，本次上传需要 11\.0 GiB/));
    expect(dlg.textContent).toMatch(/请先清理存储池/);
    // 导入目录已不可设，不再叫人去系统参数改
    expect(dlg.textContent).not.toMatch(/系统参数/);
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

  it('镜像被分组当系统盘时，删除只说明原因，不给确认按钮', async () => {
    const deleteImage = vi.fn();
    stubApi({ deleteImage, listGroups: async () => list([{ ID: 'g-1', Name: '教学一班', SystemImageID: 'win11', SystemConfigID: 'win11_default' }]) });
    renderPage();
    await openDetail();
    await userEvent.click(screen.getByRole('button', { name: /删除镜像/ }));
    expect(await screen.findByText(/正被分组 教学一班 用作系统盘/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: '确认' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: '知道了' }));
    expect(deleteImage).not.toHaveBeenCalled();
  });

  // 删除后选中会自动落到列表里下一个镜像，详情弹窗不关就会变成别的镜像的详情。
  it('删除镜像成功后关闭详情，不跳到别的镜像', async () => {
    let gone = false;
    const deleteImage = vi.fn(async () => { gone = true; return null; });
    stubApi({ deleteImage, listImages: async () => list(gone ? IMAGES.filter(i => i.ID !== 'win11') : IMAGES) });
    renderPage();
    await openDetail();
    await userEvent.click(screen.getByRole('button', { name: /删除镜像/ }));
    await userEvent.click(await screen.findByRole('button', { name: '确认' }));
    await waitFor(() => expect(deleteImage).toHaveBeenCalledWith('win11'));
    await waitFor(() => expect(screen.queryByRole('button', { name: /删除镜像/ })).toBeNull());
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

  it('还原点详情列出机型记录并写明来源，不把记录说成已适配', async () => {
    stubApi({ reductionModels: async () => [
      { id: 'h-1', name: 'VMware E1000', nic_label: '8086:100F', source: 'adapted' },
      { id: 'h-2', name: 'VMware vmxnet3', nic_label: '15AD:07B0', source: 'observed' },
      { id: 'h-3', name: 'VMware E1000E', nic_label: '8086:10D3', source: 'inherited' },
    ] });
    renderPage();
    await openTab('还原点');
    const table = await screen.findByRole('table');
    const row = (await within(table).findByText('装完office')).closest('tr');
    await userEvent.click(within(row).getByRole('button', { name: '详情' }));
    expect(await screen.findByText('机型记录')).toBeTruthy();
    expect(screen.queryByText('已适配机型')).toBeNull();
    expect(screen.getByText(/完整适配和沿用只说明写过网卡配置/)).toBeTruthy();
    expect(screen.getByText('VMware E1000').closest('p').textContent).toContain('完整适配');
    expect(screen.getByText('VMware vmxnet3').closest('p').textContent).toContain('开机成功');
    expect(screen.getByText('VMware E1000E').closest('p').textContent).toContain('沿用上一个还原点');
  });

  // 配置内容只随超管机保存或数据盘发布变化，二者都会自动生成还原点；手动新建只会复制出最新还原点的同内容副本，
  // 还容易被误认为保存了「当前应用的还原点」，所以界面不再提供入口（接口保留给测试工具）。
  it('配置页和还原点页都不提供「新建还原点」', async () => {
    renderPage();
    await openTab('还原点');
    await screen.findByText('装完office');
    expect(screen.queryByRole('button', { name: /新建还原点/ })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: /^配置/ }));
    await screen.findByRole('button', { name: /新建配置/ });
    expect(screen.queryByRole('button', { name: /新建还原点/ })).toBeNull();
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

  it('另存和导出收在「更多」里，行上只留应用和详情', async () => {
    renderPage();
    await openTab('还原点');
    const row = (await screen.findByRole('button', { name: '更多操作 装完office' })).closest('tr');
    expect(within(row).queryByRole('button', { name: /另存为新镜像/ })).toBeNull();
    expect(within(row).queryByRole('button', { name: /导出为镜像文件/ })).toBeNull();
    expect(within(row).getByRole('button', { name: '详情' })).toBeTruthy();
    await userEvent.click(within(row).getByRole('button', { name: '更多操作 装完office' }));
    for (const name of ['另存为新镜像', '导出为镜像文件', '覆盖原镜像', '删除']) {
      expect(screen.getByRole('button', { name })).toBeTruthy();
    }
  });

  it('另存为新镜像：填名字后提交', async () => {
    const saveReductionAsImage = vi.fn(async () => ({ task_id: 'task-copy' }));
    stubApi({ saveReductionAsImage });
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '更多操作 装完office' }));
    await userEvent.click(await screen.findByRole('button', { name: '另存为新镜像' }));
    const name = await screen.findByLabelText('新镜像名称');
    await userEvent.clear(name);
    await userEvent.type(name, '办公版');
    await userEvent.click(screen.getByRole('button', { name: '另存' }));
    await waitFor(() => expect(saveReductionAsImage).toHaveBeenCalledWith('win11_default_office', { name: '办公版' }));
  });

  // 和镜像导出共用同一个弹窗，只是导出对象不同。
  it('导出为镜像文件走一次性票据，可选 gzip', async () => {
    const exportReductionTicket = vi.fn(async () => ({ url: '/api/images/export-download?token=t', file_name: 'x.zfs.gz' }));
    stubApi({ exportReductionTicket });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '更多操作 装完office' }));
    await userEvent.click(await screen.findByRole('button', { name: '导出为镜像文件' }));
    expect(await screen.findByText(/导出内容为该还原点的完整磁盘数据/)).toBeTruthy();
    await userEvent.click(screen.getByLabelText('gzip 压缩'));
    await userEvent.click(screen.getByRole('button', { name: /下载到本地/ }));
    await waitFor(() => expect(exportReductionTicket).toHaveBeenCalledWith('win11_default_office', true));
  });

  it('还原点也可以导出到服务器目录', async () => {
    const exportReduction = vi.fn(async () => ({ task_id: 't3', path: '/var/lib/ndiskless/imports/Win 11-装完office.zfs', node: '192.168.10.3' }));
    stubApi({ exportReduction });
    renderPage();
    await openTab('还原点');
    await userEvent.click(await screen.findByRole('button', { name: '更多操作 装完office' }));
    await userEvent.click(await screen.findByRole('button', { name: '导出为镜像文件' }));
    await userEvent.click(await screen.findByRole('button', { name: /导出到服务器目录/ }));
    await waitFor(() => expect(exportReduction).toHaveBeenCalledWith('win11_default_office'));
    expect(await screen.findByText(/目标文件：192\.168\.10\.3 上的/)).toBeTruthy();
  });

  it('镜像详情不再放「分配到分组」：它只是跳到分组页，分组绑定在分组页里做', async () => {
    renderPage();
    await openDetail();
    expect(screen.queryByRole('button', { name: '分配到分组' })).toBeNull();
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

  it('「更多」菜单挂在 body 上：最后一行也不会被表格的滚动框裁掉，下方放不下就向上展开', async () => {
    const spy = vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(600);
    try {
      renderPage();
      await openTab('还原点');
      const btn = await screen.findByRole('button', { name: '更多操作 0' });
      btn.getBoundingClientRect = () => ({ left: 900, right: 960, top: 560, bottom: 584, width: 60, height: 24 });
      await userEvent.click(btn);
      const menu = screen.getByRole('button', { name: '覆盖原镜像' }).parentElement;
      expect(btn.closest('table').contains(menu)).toBe(false);
      expect(menu.style.position).toBe('fixed');
      expect(menu.style.bottom).not.toBe('');
      expect(menu.style.top).toBe('');
    } finally {
      spy.mockRestore();
    }
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

  // 截图工具常用 Esc 退出、误点遮罩也很常见：几 G 的上传不能因此悄悄停掉。
  it('上传中按 Esc、点取消都先问一句，确认后才关窗；只停本地上传，服务端留着续传', async () => {
    const abortUpload = vi.fn(async () => null);
    stubApi({
      abortUpload,
      uploadChunk: (id, offset, blob, signal) => new Promise((_, reject) => signal?.addEventListener('abort', () => reject(new Error('aborted')))),
    });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    await userEvent.upload(dlg.querySelector('input[type="file"]'), new File(['0123456789ab'], 'win11.vmdk'));
    await screen.findByRole('button', { name: '上传中…' });

    await userEvent.keyboard('{Escape}');
    const ask = (await screen.findByText(/上传会停下/)).closest('.card');
    await userEvent.click(within(ask).getByRole('button', { name: '取消' }));
    expect(screen.getByText('镜像名称')).toBeTruthy();
    expect(screen.getByRole('button', { name: '上传中…' })).toBeTruthy();

    await userEvent.click(within(dlg).getAllByRole('button', { name: '取消' }).at(-1));
    await userEvent.click(within((await screen.findByText(/上传会停下/)).closest('.card')).getByRole('button', { name: '中断上传' }));
    await waitFor(() => expect(screen.queryByText('镜像名称')).toBeNull());
    expect(abortUpload).not.toHaveBeenCalled();
  });

  it('文件名很长时省略显示，百分比始终看得到', async () => {
    const long = 'zh-cn_windows_11_business_editions_version_23h2_updated_aug_2024_x64_dvd_6ca91c94.vmdk';
    stubApi({ uploadChunk: () => new Promise(() => {}) });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: /导入镜像/ }));
    const dlg = (await screen.findByText('镜像名称')).closest('.card');
    await userEvent.upload(dlg.querySelector('input[type="file"]'), new File(['0123456789ab'], long));
    const name = await within(dlg).findByTitle(long);
    expect(name.style.textOverflow).toBe('ellipsis');
    expect(name.style.whiteSpace).toBe('nowrap');
    expect(name.style.overflow).toBe('hidden');
    expect(within(name.parentElement).getByText('0%').style.flexShrink).toBe('0');
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
    // 文件要放到写入节点（VIP 所在）上，之后自动同步到备用节点
    expect(within(dlg).getByText(/写入节点（VIP 所在/)).toBeTruthy();

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
    expect(await screen.findByText(/导出任务已提交，目标文件：192\.168\.10\.3 上的 \/var\/lib\/ndiskless\/imports\/Win 11\.zfs/)).toBeTruthy();
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

  it('从机型记录进入还原点时定位到指定镜像和配置，保留普通镜像深链', async () => {
    function ReductionLink() {
      const { goto } = useStore();
      React.useEffect(() => { goto('images', { tab: 'reductions', imageId: 'win11', configId: 'win11_default' }); }, []); // eslint-disable-line
      return null;
    }
    stubApi();
    render(<StoreProvider><ConfirmProvider><ReductionLink/><PageImages/></ConfirmProvider></StoreProvider>);
    expect(await screen.findByRole('combobox', { name: '按配置筛选' })).toBeTruthy();
    expect(screen.getByRole('combobox', { name: '按配置筛选' }).value).toBe('win11_default');
    expect(screen.getByRole('combobox', { name: '按镜像筛选' }).value).toBe('win11');
    expect(await screen.findByText('装完office')).toBeTruthy();
    expect(screen.queryByText('配置概况')).toBeNull();
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
    await userEvent.click(within((await screen.findByText(/上传会停下/)).closest('.card')).getByRole('button', { name: '中断上传' }));
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

// ISO 安装借用已登记的客户机：安装卡片要点名是哪台、能跳过去；向导里不能借的客户机提前置灰。
describe('ISO 安装与客户机的对应', () => {
  afterEach(() => vi.restoreAllMocks());
  const CLIENT = { ID: 'terminal-0050562EF64E', Name: '11', MAC: '0050562EF64E', IP: '192.168.10.10', GroupID: 'g-1', IsSuper: false, State: 'offline' };
  const BUSY = { id: 'install-busy', task_id: 'task-busy', name: 'win11-lab', phase: 'failed', os_type: 'windows', error: '安装未完成',
    mac: '0050562EF64E', ip: '192.168.10.10', source_path: '/tank/imports/win11.iso', size_gib: 128, edition_index: 6, username: 'tester', netmask: '255.255.255.0' };
  function RouteProbe() {
    const { route } = useStore();
    return <output aria-label="当前路由">{JSON.stringify(route)}</output>;
  }

  it('安装卡片写出客户机名称和 MAC，点名称打开那台客户机；没登记的只写 MAC', async () => {
    stubApi({
      listTerminals: async () => list([CLIENT]),
      listInstallations: async () => list([BUSY, { id: 'install-free', name: '手填的安装', phase: 'waiting', os_type: 'linux', mac: '7070FC0587A2' }]),
    });
    const getInstallation = vi.spyOn(api, 'getInstallation').mockImplementation(async () => BUSY);
    render(<StoreProvider><ConfirmProvider><PageImages/><RouteProbe/></ConfirmProvider></StoreProvider>);
    const card = (await screen.findByText('win11-lab')).closest('[data-install-card]');
    const link = await within(card).findByRole('button', { name: /客户机 11/ });
    expect(card.textContent).toContain('00:50:56:2E:F6:4E');
    await userEvent.click(link);
    const route = JSON.parse(screen.getByLabelText('当前路由').textContent);
    expect(route.page).toBe('terminals');
    expect(route.params.terminalId).toBe(CLIENT.ID);
    expect(getInstallation).not.toHaveBeenCalled();

    const free = (await screen.findByText('手填的安装')).closest('[data-install-card]');
    expect(free.textContent).toContain('70:70:FC:05:87:A2');
    expect(within(free).queryByRole('button', { name: /客户机/ })).toBeNull();
  });

  it('向导里运行中、超管机、正在安装的客户机置灰并写明原因', async () => {
    stubApi({
      listPools: async () => list([{ ID: 'pool-1', Name: 'tank', ServerID: 'node-a', Role: 'data' }]),
      listTerminals: async () => list([
        CLIENT,
        { ...CLIENT, ID: 'terminal-on', Name: '在跑的', MAC: 'AABBCCDDEE01', State: 'online' },
        { ...CLIENT, ID: 'terminal-su', Name: '超管', MAC: 'AABBCCDDEE02', IsSuper: true },
        { ...CLIENT, ID: 'terminal-ok', Name: '空闲', MAC: 'AABBCCDDEE03' },
      ]),
      listInstallations: async () => list([BUSY]),
    });
    renderPage();
    await userEvent.click(await screen.findByRole('button', { name: '从 ISO 创建' }));
    await userEvent.click(screen.getByRole('button', { name: '从服务器目录' }));
    await userEvent.click(await screen.findByText('ubuntu.iso'));
    await userEvent.click(screen.getByRole('button', { name: '识别并继续' }));
    await userEvent.type(await screen.findByLabelText('初始密码'), 'Password123!');
    await userEvent.click(screen.getByRole('button', { name: '下一步' }));
    const select = await screen.findByLabelText('安装客户机');
    const option = (text) => within(select).getAllByRole('option').find(o => o.textContent.includes(text));
    expect(option('11').disabled).toBe(true);
    expect(option('11').textContent).toContain('正在安装「win11-lab」');
    expect(option('在跑的').disabled).toBe(true);
    expect(option('在跑的').textContent).toContain('运行中');
    expect(option('超管').disabled).toBe(true);
    expect(option('超管').textContent).toContain('超管机');
    expect(option('空闲').disabled).toBe(false);
    expect(option('空闲').textContent).toContain('AA:BB:CC:DD:EE:03');
  });

  it('修改原安装的设置时，原安装用的客户机仍可选', async () => {
    stubApi({
      listPools: async () => list([{ ID: 'pool-1', Name: 'tank', ServerID: 'node-a', Role: 'data' }]),
      listTerminals: async () => list([CLIENT]),
      listInstallations: async () => list([BUSY]),
      getInstallation: async () => BUSY,
      listInstallationSources: async () => ({ items: [{ name: 'win11.iso', path: BUSY.source_path, size_bytes: 5 * 1024 ** 3 }] }),
      inspectInstallationISO: async () => ({ fingerprint: 'sha', os_type: 'windows', version: '11', recommended_size_gib: 64, editions: [{ index: 6, name: 'Pro' }] }),
    });
    renderPage();
    await userEvent.click(within((await screen.findByText('win11-lab')).closest('[data-install-card]')).getByRole('button', { name: '重试' }));
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '修改设置' }));
    await userEvent.type(await screen.findByLabelText('初始密码'), 'Password123!');
    await userEvent.click(screen.getByRole('button', { name: '下一步' }));
    const select = await screen.findByLabelText('安装客户机');
    const own = within(select).getAllByRole('option').find(o => o.textContent.includes('11'));
    expect(own.disabled).toBe(false);
  });
});
