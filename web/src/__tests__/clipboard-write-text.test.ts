// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { writeTextToClipboard } from '../clipboard';

/* ==================== 共享剪贴板 util 降级链（tasks 2.3 / design D5） ====================
 * ① navigator.clipboard.writeText 可用 → 单次调用，resolve 成功 / reject 失败
 *   （MUST NOT 再尝试 execCommand）；
 * ② API 不可用 → execCommand 降级：返回 true 成功 / false 或抛错失败。 */

let savedExec: PropertyDescriptor | undefined;

function setClipboardApi(writeText?: (t: string) => Promise<void>): void {
  if (writeText) {
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
  } else {
    delete (window.navigator as { clipboard?: unknown }).clipboard;
  }
}

function stubExecCommand(impl: () => boolean): ReturnType<typeof vi.fn> {
  const spy = vi.fn(impl);
  Object.defineProperty(document, 'execCommand', { value: spy, configurable: true });
  return spy;
}

beforeEach(() => {
  savedExec = Object.getOwnPropertyDescriptor(document, 'execCommand');
});

afterEach(() => {
  if (savedExec) Object.defineProperty(document, 'execCommand', savedExec);
  else Reflect.deleteProperty(document, 'execCommand');
  delete (window.navigator as { clipboard?: unknown }).clipboard;
  vi.restoreAllMocks();
});

describe('writeTextToClipboard 降级链（tasks 2.3）', () => {
  it('writeText 可用且 resolve → 成功，单次调用，不触碰 execCommand', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const execSpy = stubExecCommand(() => true);

    await expect(writeTextToClipboard('abc')).resolves.toBeUndefined();
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith('abc');
    expect(execSpy).not.toHaveBeenCalled();
  });

  it('writeText reject → 失败，MUST NOT 再尝试 execCommand 降级', async () => {
    const writeText = vi.fn(() => Promise.reject(new Error('denied')));
    setClipboardApi(writeText);
    const execSpy = stubExecCommand(() => true);

    await expect(writeTextToClipboard('abc')).rejects.toThrow('denied');
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(execSpy).not.toHaveBeenCalled();
  });

  it('API 不可用 + execCommand 返回 true → 成功', async () => {
    setClipboardApi(undefined);
    const execSpy = stubExecCommand(() => true);

    await expect(writeTextToClipboard('legacy')).resolves.toBeUndefined();
    expect(execSpy).toHaveBeenCalledWith('copy');
  });

  it('API 不可用 + execCommand 返回 false → 失败', async () => {
    setClipboardApi(undefined);
    stubExecCommand(() => false);

    await expect(writeTextToClipboard('legacy')).rejects.toThrow();
  });

  it('API 不可用 + execCommand 抛错 → 失败', async () => {
    setClipboardApi(undefined);
    stubExecCommand(() => {
      throw new Error('boom');
    });

    await expect(writeTextToClipboard('legacy')).rejects.toThrow('boom');
  });
});
