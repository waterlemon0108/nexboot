import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, clearSession, getStoredUser, getToken, onUnauthorized } from './api.js';

// 操作失败时操作者看到的提示都经这里：后端用中文说明拒绝原因，这里把响应转成页面提示的 Error。
// 它也负责会话：401 时清掉会话，避免失效令牌让页面全是空表。

function respond({ status = 200, body = '', contentType = 'application/json', headers = {} } = {}) {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (k) => ({ 'Content-Type': contentType, ...headers })[k] ?? null },
    text: () => Promise.resolve(typeof body === 'string' ? body : JSON.stringify(body)),
    json: () => Promise.resolve(typeof body === 'string' ? JSON.parse(body || 'null') : body),
  });
}

describe('API 客户端', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.stubGlobal('fetch', vi.fn(() => respond({ body: { items: [] } })));
  });
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

  it('带上 token 与 JSON 头', async () => {
    await api.login('admin', 'pw');
    globalThis.fetch.mockClear();
    globalThis.fetch.mockImplementation(() => respond({ body: { items: [] } }));

    await api.importImage({ name: 'win11' });
    const [path, init] = globalThis.fetch.mock.calls[0];
    expect(path).toBe('/api/images/import');
    expect(init.headers.Authorization).toMatch(/^Bearer /);
    expect(init.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(init.body)).toEqual({ name: 'win11' });
  });

  // 后端的拒绝文案是写给操作者的（「请先关闭正在使用的客户机…」），不能换成笼统的「操作失败」。
  it('把后端的中文拒绝原样抛出来', async () => {
    globalThis.fetch.mockImplementation(() => respond({
      status: 409, contentType: 'text/plain',
      body: '请先删除由该配置派生出的配置：forked',
    }));
    await expect(api.mergeConfig('cfg-1')).rejects.toThrow('请先删除由该配置派生出的配置：forked');
  });

  it('后端没给话时至少报出状态码', async () => {
    globalThis.fetch.mockImplementation(() => respond({ status: 500, contentType: 'text/plain', body: '   ' }));
    await expect(api.listImages()).rejects.toThrow('请求失败 (500)');
  });

  // 登录页的 401 是密码不对而非会话过期，必须展示后端原话，否则操作者会反复重登而不去查密码。
  it('登录失败说的是密码不对，不是会话过期', async () => {
    globalThis.fetch.mockImplementation(() => respond({
      status: 401, contentType: 'text/plain', body: '用户名或密码错误',
    }));
    await expect(api.login('admin', 'wrong')).rejects.toThrow('用户名或密码错误');
  });

  it('登录失败时后端没给话，也不能说成会话过期', async () => {
    globalThis.fetch.mockImplementation(() => respond({ status: 401, contentType: 'text/plain', body: '' }));
    await expect(api.login('admin', 'wrong')).rejects.toThrow(/用户名或密码/);
  });

  // 令牌失效必须清会话并通知外壳，否则操作者面对的空表格看起来像服务器没数据。
  it('401 时清掉会话并广播一次', async () => {
    await api.login('admin', 'pw');
    expect(getToken()).not.toBe('');

    const seen = vi.fn();
    const stop = onUnauthorized.subscribe(seen);
    globalThis.fetch.mockImplementation(() => respond({ status: 401, contentType: 'text/plain', body: 'token expired' }));

    await expect(api.listImages()).rejects.toThrow(/登录已失效/);
    expect(getToken()).toBe('');
    expect(getStoredUser()).toBeNull();
    expect(seen).toHaveBeenCalledTimes(1);
    stop();
  });

  it('登录成功后记住 token 与用户', async () => {
    globalThis.fetch.mockImplementation(() => respond({ body: { token: 'jwt-123', user: { username: 'admin' } } }));
    await api.login('admin', 'pw');
    expect(getToken()).toBe('jwt-123');
    expect(getStoredUser()).toEqual({ username: 'admin' });
    clearSession();
    expect(getToken()).toBe('');
  });

  // 204 没有 body 可解析，文件下载也不是 JSON，都不能在调用处变成解析错误。
  it('204 返回 null，非 JSON 响应返回文本', async () => {
    globalThis.fetch.mockImplementation(() => respond({ status: 204, contentType: '' }));
    expect(await api.deleteImage('img-1')).toBeNull();

    globalThis.fetch.mockImplementation(() => respond({ contentType: 'text/plain', body: '#!ipxe\nsanboot' }));
    expect(await api.getSettings()).toContain('#!ipxe');
  });

  // multipart 上传不能设 JSON content type，边界必须由浏览器设置，否则服务端解析不了表单。
  it('上传表单不覆盖 Content-Type', async () => {
    const fd = new FormData();
    fd.append('file', new File(['x'], 'pack.zip'));
    globalThis.fetch.mockImplementation(() => respond({ body: {} }));

    await api.uploadDriverPack(fd);
    const [, init] = globalThis.fetch.mock.calls[0];
    expect(init.headers['Content-Type']).toBeUndefined();
    expect(init.body).toBe(fd);
  });

  // id 原样进 URL 时要转义：美术教室 这样的配置名或带冒号的 MAC 不能破坏路径。
  it('路径参数经过编码', async () => {
    globalThis.fetch.mockImplementation(() => respond({ body: {} }));
    await api.getImage('教学一班/win11');
    expect(globalThis.fetch.mock.calls[0][0]).toBe(`/api/images/${encodeURIComponent('教学一班/win11')}`);
  });
});
