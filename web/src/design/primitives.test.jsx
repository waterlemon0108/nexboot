import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Field, SegFilter, Tip } from './primitives.jsx';

// SegFilter 按鸭子类型读选项，计数字段是 `o.n`；用例钉住双方约定的结构，传错字段名时计数会静默不显示。
describe('分段过滤器', () => {
  const options = [
    { id: '', label: '全部', n: 12 },
    { id: 'active', label: '未处理', n: 3, color: 'rose' },
    { id: 'acked', label: '已确认' }, // 无计数：不渲染，不能显示 "undefined"
  ];

  it('把每项的计数显示出来', () => {
    render(<SegFilter value="" onChange={() => {}} options={options}/>);
    expect(screen.getByText('12')).toBeTruthy();
    expect(screen.getByText('3')).toBeTruthy();
  });

  it('没有计数的项不显示计数', () => {
    render(<SegFilter value="" onChange={() => {}} options={options}/>);
    expect(screen.getByText('已确认').textContent).toBe('已确认');
    expect(screen.queryByText('undefined')).toBeNull();
  });

  // 0 也是计数而非缺失：「未处理 0」必须显示，`o.n && ...` 会把它吞掉。
  it('计数为 0 时照样显示', () => {
    render(<SegFilter value="" onChange={() => {}} options={[{ id: 'x', label: '未处理', n: 0 }]}/>);
    expect(screen.getByText('0')).toBeTruthy();
  });

  it('点击回调选中的 id', async () => {
    const onChange = vi.fn();
    render(<SegFilter value="" onChange={onChange} options={options}/>);
    await userEvent.click(screen.getByText('未处理'));
    expect(onChange).toHaveBeenCalledWith('active');
  });
});

// ? 按钮把只需看一次的说明收起来：点开显示，再点收起。
describe('Field 帮助说明', () => {
  it('说明藏在 ? 后面，点开再点收起', async () => {
    render(<Field label="网关" help="没有路由器可留空"><input/></Field>);
    expect(screen.queryByText('没有路由器可留空')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: '网关说明' }));
    expect(screen.getByText('没有路由器可留空')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '网关说明' }));
    expect(screen.queryByText('没有路由器可留空')).toBeNull();
  });

  it('没有 help 时不渲染 ? 按钮', () => {
    render(<Field label="网关" hint="常显提示"><input/></Field>);
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.getByText('常显提示')).toBeTruthy();
  });
});

describe('Tip', () => {
  it('Esc 收起', async () => {
    render(<Tip label="限速说明" text="营业时段保持限速"/>);
    await userEvent.click(screen.getByRole('button', { name: '限速说明' }));
    expect(screen.getByText('营业时段保持限速')).toBeTruthy();
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByText('营业时段保持限速')).toBeNull();
  });

  it('浮层挂在 body 上、按视口坐标放：窄屏收窄，左右都留 16px，表格的滚动框裁不到它', async () => {
    const vw = 375;
    const spy = vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(vw);
    try {
      const { container } = render(<div style={{ overflow: 'auto' }}><Tip text="营业时段保持限速"/></div>);
      const btn = screen.getByRole('button', { name: '说明' });
      btn.getBoundingClientRect = () => ({ left: 350, right: 366, top: 100, bottom: 116, width: 16, height: 16 });
      await userEvent.click(btn);
      const pop = screen.getByText('营业时段保持限速');
      expect(container.contains(pop)).toBe(false);
      expect(pop.style.position).toBe('fixed');
      const left = parseInt(pop.style.left, 10), width = parseInt(pop.style.width, 10);
      expect(width).toBeLessThanOrEqual(vw - 32);
      expect(left).toBeGreaterThanOrEqual(16);
      expect(left + width).toBeLessThanOrEqual(vw - 16);
    } finally {
      spy.mockRestore();
    }
  });

  it('页面滚动时收起，免得浮层和按钮错开', async () => {
    render(<Tip text="营业时段保持限速"/>);
    await userEvent.click(screen.getByRole('button', { name: '说明' }));
    window.dispatchEvent(new Event('scroll'));
    await waitFor(() => expect(screen.queryByText('营业时段保持限速')).toBeNull());
  });
});
