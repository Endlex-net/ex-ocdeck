// @vitest-environment jsdom
import { describe, it, expect, beforeEach } from 'vitest';
import {
  PALETTE_FOCUS_EVENT,
  __resetPaletteFocusForTest,
  consumePendingPaletteFocus,
  emitPaletteFocus,
  normalizePaletteFocusPayload,
  readPaletteFocusDetail,
} from '../palette-focus';

const legalPayloads = [
  { name: '{id}', payload: undefined, want: {} },
  { name: '{id, projectName}', payload: { projectName: 'p' }, want: { projectName: 'p' } },
  {
    name: '{id, projectName, projectID}',
    payload: { projectName: 'p', projectID: '1' },
    want: { projectName: 'p', projectID: '1' },
  },
] as const;

describe('normalizePaletteFocusPayload', () => {
  it('keeps legal payloads and collapses illegal projectID-only to {}', () => {
    expect(normalizePaletteFocusPayload(undefined)).toEqual({});
    expect(normalizePaletteFocusPayload({})).toEqual({});
    expect(normalizePaletteFocusPayload({ projectName: 'p' })).toEqual({ projectName: 'p' });
    expect(normalizePaletteFocusPayload({ projectName: 'p', projectID: '1' })).toEqual({
      projectName: 'p',
      projectID: '1',
    });
    expect(normalizePaletteFocusPayload({ projectID: '1' })).toEqual({});
    expect(normalizePaletteFocusPayload({ id: 'new-task-name', projectID: '1' })).toEqual({});
    expect(normalizePaletteFocusPayload({ id: 'new-task-name', payload: { projectName: 'p' } })).toEqual({});
  });
});

describe('pending 跨路由', () => {
  beforeEach(() => {
    __resetPaletteFocusForTest();
  });

  it.each(legalPayloads)('consume 三种合法 detail：$name', ({ payload, want }) => {
    emitPaletteFocus('new-task-name', payload);
    expect(consumePendingPaletteFocus('new-task-name')).toEqual(want);
    expect(consumePendingPaletteFocus('new-task-name')).toBeNull();
  });

  it('无匹配返回 null；匹配无 payload 返回 {}', () => {
    expect(consumePendingPaletteFocus('new-task-name')).toBeNull();
    emitPaletteFocus('new-task-name');
    expect(consumePendingPaletteFocus('register-project-name')).toBeNull();
    expect(consumePendingPaletteFocus('new-task-name')).toEqual({});
  });

  it('非法 {projectID} pending 归一为 {}', () => {
    emitPaletteFocus('new-task-name', { projectID: '1' } as never);
    expect(consumePendingPaletteFocus('new-task-name')).toEqual({});
  });
});

describe('实时事件', () => {
  beforeEach(() => {
    __resetPaletteFocusForTest();
  });

  it.each(legalPayloads)('dispatch 三种合法 detail：$name', ({ payload, want }) => {
    let seen: ReturnType<typeof readPaletteFocusDetail> = null;
    const onEvent = (e: Event) => {
      seen = readPaletteFocusDetail((e as CustomEvent).detail);
    };
    document.addEventListener(PALETTE_FOCUS_EVENT, onEvent);
    emitPaletteFocus('new-task-name', payload);
    document.removeEventListener(PALETTE_FOCUS_EVENT, onEvent);
    expect(seen).toEqual({ id: 'new-task-name', payload: want });
  });

  it('非法 {id, projectID} 实时事件归一为 {}', () => {
    let seen: ReturnType<typeof readPaletteFocusDetail> = null;
    const onEvent = (e: Event) => {
      seen = readPaletteFocusDetail((e as CustomEvent).detail);
    };
    document.addEventListener(PALETTE_FOCUS_EVENT, onEvent);
    document.dispatchEvent(
      new CustomEvent(PALETTE_FOCUS_EVENT, { detail: { id: 'new-task-name', projectID: '1' } }),
    );
    document.removeEventListener(PALETTE_FOCUS_EVENT, onEvent);
    expect(seen).toEqual({ id: 'new-task-name', payload: {} });
  });
});

describe('register-project-name 回归', () => {
  beforeEach(() => {
    __resetPaletteFocusForTest();
  });

  it('pending 与实时事件只按 id 匹配，无 payload 仍返回 {}', () => {
    emitPaletteFocus('register-project-name');
    expect(consumePendingPaletteFocus('register-project-name')).toEqual({});
    emitPaletteFocus('register-project-name');
    expect(consumePendingPaletteFocus('new-task-name')).toBeNull();
    expect(consumePendingPaletteFocus('register-project-name')).toEqual({});
  });
});
