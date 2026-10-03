import { describe, expect, it } from 'vitest';
import { redName, fmtDateTime, fmtDateTimeSec } from './format.js';

// 还原点显示操作者输入的名称；没有显示名的旧记录显示 ASCII 快照名。
describe('还原点显示名', () => {
  it('优先显示操作者输入的名字', () => {
    expect(redName({ Name: '@office', DisplayName: '装完office' })).toBe('装完office');
  });

  it('迁移前的旧行退回快照名', () => {
    expect(redName({ Name: '@r1', DisplayName: '' })).toBe('@r1');
    expect(redName({ Name: '@r1' })).toBe('@r1');
  });

  it('没有行时是空串，不是 undefined', () => {
    expect(redName(null)).toBe('');
    expect(redName(undefined)).toBe('');
  });
});

// 执行中的任务没有结束时间：后端给 null 或 Go 零值，不能显示成 1970 或 0001 年。
describe("空时间显示为 —", () => {
  for (const v of [null, undefined, "", "0001-01-01T00:00:00Z"]) {
    it(String(v), () => {
      expect(fmtDateTime(v)).toBe("—");
      expect(fmtDateTimeSec(v)).toBe("—");
    });
  }
});
