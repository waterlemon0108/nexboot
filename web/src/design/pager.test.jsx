import React from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Pager } from './primitives.jsx';
import { usePaged } from '../lib/hooks.js';

describe('Pager', () => {
  it('显示总数、每页条数可选 10/20/50，翻页与切换条数都回调', async () => {
    const onPage = vi.fn(), onSize = vi.fn();
    render(<Pager page={1} size={10} total={35} onPage={onPage} onSize={onSize}/>);
    expect(screen.getByText('共 35 条')).toBeTruthy();
    expect(screen.getByText('1 / 4')).toBeTruthy();
    const sizes = screen.getByLabelText('每页条数');
    expect([...sizes.options].map(o => o.value)).toEqual(['10', '20', '50']);
    await userEvent.selectOptions(sizes, '20');
    expect(onSize).toHaveBeenCalledWith(20);
    await userEvent.click(screen.getByRole('button', { name: '下一页' }));
    expect(onPage).toHaveBeenCalledWith(2);
    expect(screen.getByRole('button', { name: '上一页' }).disabled).toBe(true);
  });

  it('不超过一页最小条数时不显示', () => {
    const { container } = render(<Pager page={1} size={10} total={10} onPage={() => {}} onSize={() => {}}/>);
    expect(container.textContent).toBe('');
  });

  it('条数调大后只剩一页也要能调回来', () => {
    render(<Pager page={1} size={50} total={30} onPage={() => {}} onSize={() => {}}/>);
    expect(screen.getByLabelText('每页条数')).toBeTruthy();
  });
});

function List({ items }) {
  const paged = usePaged(items);
  return <><ul>{paged.rows.map(r => <li key={r}>{r}</li>)}</ul><Pager {...paged.pager}/></>;
}

describe('usePaged', () => {
  it('默认每页 10 条，切到 20 条回到第一页', async () => {
    const items = Array.from({ length: 25 }, (_, i) => `row-${i}`);
    render(<List items={items}/>);
    expect(screen.getAllByRole('listitem')).toHaveLength(10);
    await userEvent.click(screen.getByRole('button', { name: '下一页' }));
    expect(screen.getAllByRole('listitem')[0].textContent).toBe('row-10');
    await userEvent.selectOptions(screen.getByLabelText('每页条数'), '20');
    expect(screen.getAllByRole('listitem')).toHaveLength(20);
    expect(screen.getAllByRole('listitem')[0].textContent).toBe('row-0');
  });
});
