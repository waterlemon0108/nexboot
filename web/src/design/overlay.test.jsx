import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ConfirmProvider, Modal, useConfirm } from './overlay.jsx';

// useConfirm 只返回 `ask`，弹窗由 provider 渲染；若要调用方自己渲染节点，漏放时删除按钮会毫无反应。
function DeleteButton({ onDone }) {
  const { ask } = useConfirm();
  return (
    <button onClick={async () => onDone(await ask({ message: '删除这条告警？' }))}>
      删除
    </button>
  );
}

describe('确认对话框', () => {
  it('点确认时兑现调用方等待的 Promise', async () => {
    const done = vi.fn();
    render(<ConfirmProvider><DeleteButton onDone={done}/></ConfirmProvider>);

    await userEvent.click(screen.getByText('删除'));
    await screen.findByText('删除这条告警？');
    await userEvent.click(screen.getByText('确认'));

    await waitFor(() => expect(done).toHaveBeenCalledWith(true));
  });

  it('点取消时兑现为 false，而不是悬着', async () => {
    const done = vi.fn();
    render(<ConfirmProvider><DeleteButton onDone={done}/></ConfirmProvider>);

    await userEvent.click(screen.getByText('删除'));
    await userEvent.click(await screen.findByText('取消'));

    await waitFor(() => expect(done).toHaveBeenCalledWith(false));
  });

  // 弹窗归 provider 管，页面不会漏渲染；在 provider 外使用 hook 要在渲染时就报错，而不是点击时静默失败。
  it('没有 Provider 时立刻报错，而不是点了才没反应', () => {
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => render(<DeleteButton onDone={() => {}}/>)).toThrow(/ConfirmProvider/);
    quiet.mockRestore();
  });

  it('typeToConfirm 未输对时按钮不可点', async () => {
    const done = vi.fn();
    function Danger() {
      const { ask } = useConfirm();
      return <button onClick={async () => done(await ask({ message: '销毁存储池', typeToConfirm: 'tank' }))}>销毁</button>;
    }
    render(<ConfirmProvider><Danger/></ConfirmProvider>);

    await userEvent.click(screen.getByText('销毁'));
    const confirm = await screen.findByText('确认');
    expect(confirm.disabled).toBe(true);

    await userEvent.type(screen.getByPlaceholderText('tank'), 'tank');
    expect(confirm.disabled).toBe(false);
    await userEvent.click(confirm);
    await waitFor(() => expect(done).toHaveBeenCalledWith(true));
  });
});

// 调用方几乎都传内联 onClose；外层父组件重渲染不能改变弹层的叠放次序，也不能弄丢滚动锁的原值。
describe('叠放的弹层', () => {
  function Inner() {
    const [open, setOpen] = React.useState(true);
    return (
      <Modal open={open} onClose={() => setOpen(false)} title="内层">
        <button onClick={() => setOpen(false)}>关内层</button>
      </Modal>
    );
  }
  const MemoInner = React.memo(Inner);
  function Outer() {
    const [n, setN] = React.useState(0);
    const [open, setOpen] = React.useState(true);
    return (
      <>
        <Modal open={open} onClose={() => setOpen(false)} title="外层">
          <button onClick={() => setN(n + 1)}>刷新外层 {n}</button>
          <button onClick={() => setOpen(false)}>关外层</button>
        </Modal>
        <MemoInner/>
      </>
    );
  }

  it('外层重渲染后 ESC 仍只关最上层', async () => {
    render(<Outer/>);
    fireEvent.click(screen.getByText(/刷新外层/));
    fireEvent.keyDown(document, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByText('内层')).toBeNull());
    expect(screen.getByText('外层')).toBeTruthy();
  });

  it('两层都关掉后恢复页面滚动', async () => {
    document.body.style.overflow = '';
    render(<Outer/>);
    expect(document.body.style.overflow).toBe('hidden');
    fireEvent.click(screen.getByText(/刷新外层/));
    fireEvent.click(screen.getByText('关内层'));
    fireEvent.click(screen.getByText('关外层'));
    await waitFor(() => expect(screen.queryByText('外层')).toBeNull());
    expect(document.body.style.overflow).toBe('');
  });
});
