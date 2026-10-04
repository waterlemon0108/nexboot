// Go 后端 API 的轻量 fetch 封装。
// 领域结构体没有 json tag，实体字段为 PascalCase（ID、Name、OSType、MAC、IP、State、IsSuper、Capacity…）。
// 列表接口返回 { items, total }。

const TOKEN_KEY = 'nd_token';
const USER_KEY = 'nd_user';

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || '';
}
export function getStoredUser() {
  try {
    return JSON.parse(localStorage.getItem(USER_KEY) || 'null');
  } catch {
    return null;
  }
}
function setSession(token, user) {
  localStorage.setItem(TOKEN_KEY, token);
  if (user) localStorage.setItem(USER_KEY, JSON.stringify(user));
}
export function clearSession() {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
}

// 请求返回 401 时触发，外壳据此回到登录页。
export const onUnauthorized = (() => {
  const target = new EventTarget();
  return {
    emit: () => target.dispatchEvent(new Event('unauthorized')),
    subscribe: (fn) => {
      target.addEventListener('unauthorized', fn);
      return () => target.removeEventListener('unauthorized', fn);
    },
  };
})();

async function request(path, { method = 'GET', body, formData, raw } = {}) {
  const headers = {};
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  let payload;
  if (formData !== undefined) {
    payload = formData; // multipart 边界由浏览器设置
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  const res = await fetch(path, { method, headers, body: payload });
  if (res.status === 401) {
    // 登录接口的 401 是密码不对而非会话过期，原样展示后端的说法。
    if (path === '/api/login') {
      const text = (await res.text().catch(() => '')).trim();
      throw new Error(text || '用户名或密码错误');
    }
    clearSession();
    onUnauthorized.emit();
    throw new Error('登录已失效，请重新登录');
  }
  if (!res.ok) {
    const text = (await res.text().catch(() => '')).trim();
    throw new Error(text || `请求失败 (${res.status})`);
  }
  if (raw) return res;
  if (res.status === 204) return null;
  const ct = res.headers.get('Content-Type') || '';
  if (!ct.includes('application/json')) return res.text();
  return res.json();
}

export const api = {
  // --- 认证 ---
  async login(username, password) {
    const data = await request('/api/login', { method: 'POST', body: { username, password } });
    setSession(data.token, data.user);
    return data;
  },

  // --- 镜像 / 配置 / 还原点 ---
  listImages: () => request('/api/images'),
  getImage: (id) => request(`/api/images/${encodeURIComponent(id)}`),
  importImage: (body) => request('/api/images/import', { method: 'POST', body }),
  createBlankImage: (body) => request('/api/images/blank', { method: 'POST', body }),
  setImagePurpose: (id, body) => request(`/api/images/${encodeURIComponent(id)}`, { method: 'PATCH', body }),
  listImportSources: () => request('/api/images/import-sources'),
  exportImage: (id) => request(`/api/images/${encodeURIComponent(id)}/export`, { method: 'POST' }),
  exportReduction: (id) => request(`/api/reductions/${encodeURIComponent(id)}/export`, { method: 'POST' }),
  deleteImage: (id) => request(`/api/images/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  // 从本机上传镜像：分片 + 断点续传，几十 G 的文件不能一次 POST。
  beginUpload: (body) => request('/api/images/uploads', { method: 'POST', body }),
  uploadStatus: (id) => request(`/api/images/uploads/${encodeURIComponent(id)}`),
  abortUpload: (id) => request(`/api/images/uploads/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  // 分片用原生 fetch：request() 按 JSON 处理 body，而分片是二进制且须带 Content-Range。
  uploadChunk: async (id, offset, blob, signal) => {
    const res = await fetch(`/api/images/uploads/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      signal,
      headers: {
        'Authorization': 'Bearer ' + (getToken() || ''),
        'Content-Range': `bytes ${offset}-${offset + blob.size - 1}/${blob.size + offset}`,
      },
      body: blob,
    });
    if (!res.ok) throw new Error((await res.text()) || `上传失败 (${res.status})`);
    return res.json();
  },
  exportImageTicket: (id, compress) =>
    request(`/api/images/${encodeURIComponent(id)}/export-ticket${compress ? '?compress=gzip' : ''}`, { method: 'POST' }),
  getImageHealth: (id) => request(`/api/images/${encodeURIComponent(id)}/health`),
  runImageHealthCheck: (id) =>
    request(`/api/images/${encodeURIComponent(id)}/health-check`, { method: 'POST' }),
  getRuntime: () => request('/api/runtime'),
  getSettings: () => request('/api/settings'),
  saveSettings: (body) => request('/api/settings', { method: 'PUT', body }),
  // 客户机网络：面向客户机的网卡、是否允许跨网段分组、clientMax 台机器的建议地址窗口。
  getHA: () => request('/api/ha'),
  getReplication: () => request('/api/replication'),
  plannedSwitch: () => request('/api/ha/planned-switch', { method: 'POST' }),
  setHARate: (mbps) => request('/api/ha/rate', { method: 'POST', body: { mbps } }),
  // 建集群只需一个 VIP，集群令牌由产品生成。
  createCluster: (body) => request('/api/cluster/create', { method: 'POST', body }),
  // 同网段内尚未配置的节点，供控制台列出可加入的机器。
  discoverNodes: () => request('/api/cluster/discover'),
  adoptNode: (body) => request('/api/cluster/adopt', { method: 'POST', body }),
  listClusterNodes: () => request('/api/cluster/nodes'),
  listClusterServices: () => request('/api/cluster/services'),
  // 把报废机器移出节点表，不可逆，界面上须先核对地址。
  forgetClusterNode: (node) =>
    request(`/api/cluster/nodes/${encodeURIComponent(node)}`, { method: 'DELETE' }),
  listClusterPools: () => request('/api/cluster/pools'),
  clusterServiceAction: (node, key, action) =>
    request(`/api/cluster/nodes/${encodeURIComponent(node)}/services/${encodeURIComponent(key)}/action`,
      { method: 'POST', body: { action } }),
  // 池写操作在盘所在节点执行，须点名节点；不点名即本机。
  clusterPoolPath: (node, suffix = '') =>
    `/api/cluster/nodes/${encodeURIComponent(node)}/pools${suffix}`,
  // 建池要列目标机器自己的盘：同一个 /dev/sdb 在每台机器上是不同的盘。
  listClusterDisks: (node) => request(`/api/cluster/nodes/${encodeURIComponent(node)}/disks`),
  clusterDestroyPool: (node, id) =>
    request(`/api/cluster/nodes/${encodeURIComponent(node)}/pools/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  clusterPoolAction: (node, id, suffix, body) =>
    request(`/api/cluster/nodes/${encodeURIComponent(node)}/pools/${encodeURIComponent(id)}${suffix}`,
      { method: 'POST', body }),
  clusterCreatePool: (node, body) =>
    request(`/api/cluster/nodes/${encodeURIComponent(node)}/pools`, { method: 'POST', body }),
  clusterServiceLogs: (node, key, lines = 200) =>
    request(`/api/cluster/nodes/${encodeURIComponent(node)}/services/${encodeURIComponent(key)}/logs?lines=${lines}`),
  getNetwork: (clientMax) => request(`/api/network${clientMax ? `?client_max=${encodeURIComponent(clientMax)}` : ''}`),
  saveNetwork: (body) => request('/api/network', { method: 'PUT', body }),
  probeNetwork: (ip) => request('/api/network/probe', { method: 'POST', body: { ip } }),
  previewGroupNetwork: (id, body) => request(`/api/groups/${encodeURIComponent(id)}/network-preview`, { method: 'POST', body }),

  listConfigs: (imageId) => request(`/api/images/${encodeURIComponent(imageId)}/configs`),
  createConfigFromImage: (imageId, body) =>
    request(`/api/images/${encodeURIComponent(imageId)}/configs`, { method: 'POST', body }),
  forkConfig: (configId, body) =>
    request(`/api/configs/${encodeURIComponent(configId)}/fork`, { method: 'POST', body }),
  mergeConfig: (configId) =>
    request(`/api/configs/${encodeURIComponent(configId)}/merge`, { method: 'POST' }),
  deleteConfig: (configId) =>
    request(`/api/configs/${encodeURIComponent(configId)}`, { method: 'DELETE' }),

  listReductions: (configId) => request(`/api/configs/${encodeURIComponent(configId)}/reductions`),
  applyReduction: (id) => request(`/api/reductions/${encodeURIComponent(id)}/apply`, { method: 'POST' }),
  exportReductionTicket: (id, compress) =>
    request(`/api/reductions/${encodeURIComponent(id)}/export-ticket${compress ? '?compress=gzip' : ''}`, { method: 'POST' }),
  saveReductionAsImage: (id, body) =>
    request(`/api/reductions/${encodeURIComponent(id)}/save-as-image`, { method: 'POST', body }),
  overwriteImage: (id) => request(`/api/reductions/${encodeURIComponent(id)}/overwrite-image`, { method: 'POST' }),
  mergeReductions: (configId, body) =>
    request(`/api/configs/${encodeURIComponent(configId)}/reductions/merge`, { method: 'POST', body }),
  deleteReduction: (reductionId) =>
    request(`/api/reductions/${encodeURIComponent(reductionId)}`, { method: 'DELETE' }),

  // --- 分组 ---
  listGroups: () => request('/api/groups'),
  getGroup: (id) => request(`/api/groups/${encodeURIComponent(id)}`),
  defaultGroup: () => request('/api/groups/default'),
  createGroup: (body) => request('/api/groups', { method: 'POST', body }),
  updateGroup: (id, body) => request(`/api/groups/${encodeURIComponent(id)}`, { method: 'PUT', body }),
  deleteGroup: (id) => request(`/api/groups/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  setDefaultGroup: (id) => request(`/api/groups/${encodeURIComponent(id)}/default`, { method: 'POST' }),
  listGroupDisks: (groupId) => request(`/api/groups/${encodeURIComponent(groupId)}/disks`),
  createGroupDisk: (groupId, body) => request(`/api/groups/${encodeURIComponent(groupId)}/disks`, { method: 'POST', body }),
  deleteGroupDisk: (id) => request(`/api/group-disks/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  // --- 客户机 ---
  listTerminals: () => request('/api/terminals'),
  getTerminal: (id) => request(`/api/terminals/${encodeURIComponent(id)}`),
  createTerminal: (body) => request('/api/terminals', { method: 'POST', body }),
  updateTerminal: (id, body) => request(`/api/terminals/${encodeURIComponent(id)}`, { method: 'PUT', body }),
  deleteTerminal: (id) => request(`/api/terminals/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  moveTerminals: (body) => request('/api/terminals/move', { method: 'POST', body }),
  importTerminals: (formData) => request('/api/terminals/import', { method: 'POST', formData }),
  exportTerminals: (groupId) => request(`/api/terminals/export${groupId ? `?group_id=${encodeURIComponent(groupId)}` : ''}`, { raw: true }),
  terminalTemplate: () => request('/api/terminals/template', { raw: true }),
  enableSuper: (id) => request(`/api/terminals/${encodeURIComponent(id)}/super`, { method: 'POST' }),
  disableSuper: (id) => request(`/api/terminals/${encodeURIComponent(id)}/super`, { method: 'DELETE' }),
  stopSuper: (id, body) => request(`/api/terminals/${encodeURIComponent(id)}/super/stop`, { method: 'POST', body }),
  // 数据盘在线发布：机器不关机，只有这块盘脱机几秒
  superDisks: (id) => request(`/api/terminals/${encodeURIComponent(id)}/super/disks`),
  publishDataDisk: (id, body) => request(`/api/terminals/${encodeURIComponent(id)}/super/publish`, { method: 'POST', body }),
  injectDriver: (id, body) => request(`/api/terminals/${encodeURIComponent(id)}/inject-driver`, { method: 'POST', body }),
  injectResult: (id) => request(`/api/terminals/${encodeURIComponent(id)}/inject-result`),

  // --- 存储池 ---
  listPools: () => request('/api/pools'),
  listDisks: () => request('/api/storage/disks'),
  getPool: (id) => request(`/api/pools/${encodeURIComponent(id)}`),
  createPool: (body) => request('/api/pools', { method: 'POST', body }),
  deletePool: (id) => request(`/api/pools/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  addPoolDisk: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/disks`, { method: 'POST', body }),
  removePoolDisk: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/disks/remove`, { method: 'POST', body }),
  replacePoolDisk: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/disks/replace`, { method: 'POST', body }),
  mirrorUpgradePool: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/mirror-upgrade`, { method: 'POST', body }),
  addSpecial: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/special`, { method: 'POST', body }),
  removeSpecial: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/special/remove`, { method: 'POST', body }),
  addSpare: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/spares`, { method: 'POST', body }),
  removeSpare: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/spares/remove`, { method: 'POST', body }),
  addReadCache: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/read-cache`, { method: 'POST', body }),
  removeReadCache: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/read-cache/remove`, { method: 'POST', body }),
  addWriteCache: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/write-cache`, { method: 'POST', body }),
  removeWriteCache: (id, body) => request(`/api/pools/${encodeURIComponent(id)}/write-cache/remove`, { method: 'POST', body }),
  flushWriteCache: (id) => request(`/api/pools/${encodeURIComponent(id)}/write-cache/flush`, { method: 'POST' }),

  // --- 驱动 ---
  listDriverPacks: () => request('/api/driver-packs'),
  uploadDriverPack: (formData) => request('/api/driver-packs', { method: 'POST', formData }),
  setDriverPackStatus: (id, status) =>
    request(`/api/driver-packs/${encodeURIComponent(id)}/status`, { method: 'POST', body: { status } }),
  recommendDriverPack: (id) =>
    request(`/api/driver-packs/${encodeURIComponent(id)}/recommend`, { method: 'POST' }),
  deleteDriverPack: (id) => request(`/api/driver-packs/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  listDriverBundles: () => request('/api/driver-bundles'),
  createDriverBundle: (body) => request('/api/driver-bundles', { method: 'POST', body }),
  deleteDriverBundle: (id) => request(`/api/driver-bundles/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  downloadDriverBundle: (id) =>
    request(`/api/driver-bundles/${encodeURIComponent(id)}/archive`, { raw: true }),

  // --- 告警 ---
  listAlarms: ({ status = '', severity = '', page = 1, size = 50 } = {}) => {
    const p = new URLSearchParams({ page: String(page), size: String(size) });
    if (status) p.set('status', status);
    if (severity) p.set('severity', severity);
    return request(`/api/alarms?${p.toString()}`);
  },
  ackAlarm: (id) => request(`/api/alarms/${encodeURIComponent(id)}/ack`, { method: 'POST' }),
  deleteAlarm: (id) => request(`/api/alarms/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  // --- 审计 / 日志 ---
  listLogs: ({ type = '', user = '', module = '', status = '', q = '', from = '', to = '', page = 1, size = 20 } = {}) => {
    const p = new URLSearchParams({ page: String(page), size: String(size) });
    if (type) p.set('type', type);
    if (user) p.set('user', user);
    if (module) p.set('module', module);
    if (status) p.set('status', status);
    if (q) p.set('q', q);
    if (from) p.set('from', from);
    if (to) p.set('to', to);
    return request(`/api/logs?${p.toString()}`);
  },

  // --- 服务器与服务 ---
  getServer: () => request('/api/server'),
  listServices: () => request('/api/services'),
  serviceAction: (key, action) =>
    request(`/api/services/${encodeURIComponent(key)}/action`, { method: 'POST', body: { action } }),
  serviceLogs: (key, lines = 200) =>
    request(`/api/services/${encodeURIComponent(key)}/logs?lines=${lines}`),

  // --- 备份 ---
  getBackupStatus: () => request('/api/backup/status'),
  listClusterBackups: () => request('/api/cluster/backups'),
  saveBackupConfig: (body) => request('/api/backup/config', { method: 'PUT', body }),
  runBackup: () => request('/api/backup/run', { method: 'POST' }),

  // --- 用户 ---
  listUsers: () => request('/api/users'),
  createUser: (body) => request('/api/users', { method: 'POST', body }),
  updateUser: (id, body) => request(`/api/users/${encodeURIComponent(id)}`, { method: 'PUT', body }),
  deleteUser: (id) => request(`/api/users/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  changeUserPassword: (id, body) => request(`/api/users/${encodeURIComponent(id)}/password`, { method: 'POST', body }),


  // --- 硬件档案 ---

  // --- 任务 ---
  getTask: (id) => request(`/api/tasks/${encodeURIComponent(id)}`),
  listTasks: () => request('/api/tasks'),
  listTaskHistory: ({ page = 1, size = 20, status = '' } = {}) => {
    const q = new URLSearchParams({ page: String(page), size: String(size) });
    if (status) q.set('status', status);
    return request(`/api/tasks/history?${q.toString()}`);
  },
};
