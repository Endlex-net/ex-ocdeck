import { describe, it, expect } from 'vitest';

/**
 * 最小 smoke 测试：证明 vitest runner 可用。
 * 放在 web/src/__tests__/ 下；不引入运行时依赖，仅断言平凡纯函数。
 */
function add(a: number, b: number): number {
  return a + b;
}

describe('smoke', () => {
  it('vitest runner works', () => {
    expect(add(1, 2)).toBe(3);
  });
});