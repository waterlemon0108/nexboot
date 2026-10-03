import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LoginPage } from './login.jsx';

// 直接赋值的 globalThis.fetch 不归 restoreAllMocks 管，必须自己还原，否则会漏进
// 同批次其它测试文件，让它们超时。
let realFetch;
beforeEach(() => { realFetch = globalThis.fetch; });
afterEach(() => {
  globalThis.fetch = realFetch;
  vi.restoreAllMocks();
});

// 登录页脚只能写登录前确知的两件事：运维输入的地址和服务是否健康（/healthz 公开），
// 不得出现编造的节点名或「单节点」。
describe('登录页页脚', () => {
  it('显示运维实际连接的地址，而不是编出来的主机名', async () => {
    globalThis.fetch = vi.fn(async () => ({ ok: true, status: 200 }));
    render(<LoginPage onLogin={() => {}}/>);
    await waitFor(() => expect(screen.getByText(new RegExp(window.location.host))).toBeTruthy());
    expect(screen.queryByText(/srv-master-01/)).toBeNull();
    expect(screen.queryByText(/10\.0\.0\.10/)).toBeNull();
  });

  it('不谎称单节点——集群规模要登录后才知道', async () => {
    globalThis.fetch = vi.fn(async () => ({ ok: true, status: 200 }));
    render(<LoginPage onLogin={() => {}}/>);
    await waitFor(() => expect(screen.getByText(new RegExp(window.location.host))).toBeTruthy());
    expect(screen.queryByText(/单节点/)).toBeNull();
  });

  it('服务不健康时如实说，不硬画一个绿点', async () => {
    globalThis.fetch = vi.fn(async () => { throw new Error('connection refused'); });
    render(<LoginPage onLogin={() => {}}/>);
    await waitFor(() => expect(screen.getByText(/连不上|不可用/)).toBeTruthy());
  });
});

describe('登录页的安全提示', () => {
  // 服务端只有 ListenAndServe，登录框不得出现写死的加密承诺。
  it('明文 HTTP 打开时不承诺加密，而是提示这是内网专用', async () => {
    globalThis.fetch = vi.fn(async () => ({ ok: true, status: 200 }));
    render(<LoginPage onLogin={() => {}}/>);
    expect(screen.queryByText(/TLS/)).toBeNull();
    expect(screen.queryByText(/ENCRYPTED/i)).toBeNull();
    expect(screen.getByText(/明文\s*HTTP/)).toBeTruthy();
  });

  it('不放不通的入口：没有忘记密码死链，没有不进请求的记住设备', async () => {
    globalThis.fetch = vi.fn(async () => ({ ok: true, status: 200 }));
    render(<LoginPage onLogin={() => {}}/>);
    expect(screen.queryByText(/忘记密码/)).toBeNull();
    expect(screen.queryByText(/记住设备/)).toBeNull();
  });
});
