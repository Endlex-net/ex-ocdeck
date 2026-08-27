import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

describe('TaskWorkbenchPage SSE 接线契约（源码断言，task-detail-stream D6/D7）', () => {
  const pageSrc = readFileSync(
    fileURLToPath(new URL('../pages/TaskWorkbenchPage.tsx', import.meta.url)),
    'utf8',
  );
  const appSrc = readFileSync(fileURLToPath(new URL('../App.tsx', import.meta.url)), 'utf8');

  it('订阅替代轮询：subscribeTask，不再 usePoll / getTask / load()', () => {
    expect(pageSrc).toMatch(/subscribeTask\(taskID,\s*\{/);
    expect(pageSrc).not.toMatch(/usePoll/);
    expect(pageSrc).not.toMatch(/api\.getTask/);
    expect(pageSrc).not.toMatch(/\bvoid load\(\)/);
    expect(pageSrc).not.toMatch(/const load = /);
    expect(pageSrc).not.toMatch(/initActive/);
  });

  it('首帧前展示「连接中…」（task === null && !notFound）', () => {
    expect(pageSrc).toMatch(/if \(task === null\)/);
    expect(pageSrc).toContain('连接中…');
  });

  it('onGone → setNotFound；onError 走 setError；onData 清 error', () => {
    expect(pageSrc).toMatch(/onGone:\s*\(\)\s*=>\s*setNotFound\(true\)/);
    expect(pageSrc).toMatch(/onError:\s*setError/);
    expect(pageSrc).toMatch(/setError\(''\)/);
  });

  it('卸载 cleanup 关闭订阅', () => {
    expect(pageSrc).toMatch(/return \(\) => sub\.close\(\)/);
  });

  it('操作成功后不再手动 load，仍 refreshShared', () => {
    expect(pageSrc).toMatch(/const onTaskActionDone = \(\) => \{/);
    expect(pageSrc).toMatch(/void refreshShared\(\)/);
    const doneBlock = pageSrc.match(
      /const onTaskActionDone = \(\) => \{[\s\S]*?\n  \};/,
    )?.[0];
    expect(doneBlock).toBeDefined();
    expect(doneBlock).not.toMatch(/load\(/);
  });

  it('taskID 切换经 App 路由 key={taskID} 重挂载重建订阅', () => {
    expect(appSrc).toMatch(/<TaskWorkbenchPage key=\{res\.taskID\} taskID=\{res\.taskID\}/);
  });
});
