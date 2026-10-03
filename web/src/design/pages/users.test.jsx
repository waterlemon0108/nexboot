import React from 'react';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../lib/api.js';
import { StoreProvider } from '../store.jsx';
import { ConfirmProvider } from '../overlay.jsx';
import { PageSettings } from './settings.jsx';

describe('用户管理', () => {
  afterEach(() => vi.restoreAllMocks());

  it('用户多于 10 个时分页', async () => {
    const users = Array.from({ length: 11 }, (_, i) => ({ id: `u${i}`, username: `user${String(i).padStart(2, '0')}` }));
    vi.spyOn(api, 'listUsers').mockImplementation(async () => ({ items: users, total: users.length }));
    render(<StoreProvider><ConfirmProvider><PageSettings/></ConfirmProvider></StoreProvider>);
    expect(await screen.findByText('共 11 条')).toBeTruthy();
    expect(screen.getByText('user09')).toBeTruthy();
    expect(screen.queryByText('user10')).toBeNull();
  });
});
