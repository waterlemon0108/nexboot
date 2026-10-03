import React from 'react';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from './api.js';
import { useMutation, useResource, onMutation } from './hooks.js';

// 异步写入只返回 task_id，失败原因只记在任务上；这些用例覆盖把原因展示到界面的路径。
function Screen({ toast, call, reload }) {
  const { busy, run } = useMutation(toast);
  return (
    <button disabled={busy} onClick={() => run(call, '任务已提交', { reload, errMsg: '合并失败' })}>
      合并
    </button>
  );
}

describe('异步任务回执', () => {
  beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

  it('任务失败时把任务记录的原因说出来', async () => {
    const toast = vi.fn();
    const reload = vi.fn();
    vi.spyOn(api, 'getTask')
      .mockResolvedValueOnce({ Status: 'running' })
      .mockResolvedValue({ Status: 'failed', Error: '配置已被派生使用，请先删除由该配置派生出的配置：forked' });

    render(<Screen toast={toast} reload={reload} call={async () => ({ task_id: 'task-merge-1' })}/>);
    await userEvent.click(screen.getByText('合并'));

    expect(toast).toHaveBeenCalledWith('任务已提交', 'ok');
    await vi.advanceTimersByTimeAsync(5000);
    await waitFor(() => expect(toast).toHaveBeenCalledWith(
      '配置已被派生使用，请先删除由该配置派生出的配置：forked', 'err'));
    expect(reload).toHaveBeenCalled();
  });

  it('任务成功后刷新列表，而不是让操作者自己再点一次', async () => {
    const toast = vi.fn();
    const reload = vi.fn();
    vi.spyOn(api, 'getTask').mockResolvedValue({ Status: 'success' });

    render(<Screen toast={toast} reload={reload} call={async () => ({ task_id: 'task-merge-2' })}/>);
    await userEvent.click(screen.getByText('合并'));

    await vi.advanceTimersByTimeAsync(3000);
    await waitFor(() => expect(reload).toHaveBeenCalled());
    expect(toast).not.toHaveBeenCalledWith(expect.anything(), 'err');
  });

  // 单次轮询失败是网络抖动而非任务失败，不能就此放弃把正常导入报成失败。
  it('轮询中途失败一次不算任务失败，仍然等到结果', async () => {
    const toast = vi.fn();
    const reload = vi.fn();
    vi.spyOn(api, 'getTask')
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValue({ Status: 'success' });

    render(<Screen toast={toast} reload={reload} call={async () => ({ task_id: 'task-import-1' })}/>);
    await userEvent.click(screen.getByText('合并'));

    await vi.advanceTimersByTimeAsync(5000);
    // 越过抖动继续轮询直到任务结束。
    await waitFor(() => expect(reload).toHaveBeenCalled());
    expect(toast).not.toHaveBeenCalledWith(expect.anything(), 'err');
  });

  // 同步应答的写入无任务可跟踪，仍须在返回前刷新。
  it('同步接口不轮询，但照样刷新', async () => {
    const toast = vi.fn();
    const reload = vi.fn();
    const getTask = vi.spyOn(api, 'getTask');

    render(<Screen toast={toast} reload={reload} call={async () => ({ ID: 'group-1' })}/>);
    await userEvent.click(screen.getByText('合并'));

    await waitFor(() => expect(reload).toHaveBeenCalled());
    expect(getTask).not.toHaveBeenCalled();
  });

  // 每次成功写入都要广播，让全局 store（侧栏徽标、⌘K）立即重读；失败的调用不广播。
  it('成功后广播变更，失败不广播', async () => {
    const heard = vi.fn();
    const unsubscribe = onMutation.subscribe(heard);
    const toast = vi.fn();
    render(<Screen toast={toast} reload={vi.fn()} call={async () => ({ ID: 'group-1' })}/>);
    await userEvent.click(screen.getByText('合并'));
    await waitFor(() => expect(heard).toHaveBeenCalledTimes(1));

    render(<Screen toast={toast} reload={vi.fn()} call={async () => { throw new Error('nope'); }}/>);
    await userEvent.click(screen.getAllByText('合并')[1]);
    await waitFor(() => expect(toast).toHaveBeenCalled());
    expect(heard).toHaveBeenCalledTimes(1);
    unsubscribe();
  });

  it('调用本身失败时报错，不假装已提交', async () => {
    const toast = vi.fn();
    render(<Screen toast={toast} reload={vi.fn()} call={async () => { throw new Error('还原点名称必填'); }}/>);
    await userEvent.click(screen.getByText('合并'));

    await waitFor(() => expect(toast).toHaveBeenCalledWith('还原点名称必填', 'err'));
    expect(toast).not.toHaveBeenCalledWith('任务已提交', 'ok');
  });
});

// 快速切换筛选或翻页时，慢的旧请求后到不能盖掉新数据；切换期间要显示加载中。
describe('useResource 切换参数', () => {
  function deferred() {
    let resolve;
    const promise = new Promise(r => { resolve = r; });
    return { promise, resolve };
  }
  function List({ k, pending }) {
    const { data, loading } = useResource(() => pending[k].promise, [k]);
    return <div>{`${loading ? '加载中' : '就绪'}:${data || '空'}`}</div>;
  }

  it('慢的旧请求后到时丢弃，不覆盖新数据', async () => {
    const pending = { a: deferred(), b: deferred() };
    const { rerender } = render(<List k="a" pending={pending}/>);
    rerender(<List k="b" pending={pending}/>);
    await act(async () => { pending.b.resolve('第二页'); });
    await screen.findByText('就绪:第二页');
    await act(async () => { pending.a.resolve('第一页'); });
    expect(screen.getByText('就绪:第二页')).toBeTruthy();
  });

  it('参数变化时回到加载中', async () => {
    const pending = { a: deferred(), b: deferred() };
    const { rerender } = render(<List k="a" pending={pending}/>);
    await act(async () => { pending.a.resolve('第一页'); });
    await screen.findByText('就绪:第一页');
    rerender(<List k="b" pending={pending}/>);
    expect(await screen.findByText('加载中:第一页')).toBeTruthy();
  });
});
