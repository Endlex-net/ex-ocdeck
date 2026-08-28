import type { Extension } from '@codemirror/state';
import { StreamLanguage } from '@codemirror/language';

/**
 * 从 git path 提取小写扩展名（含前导点），作为语言映射表的键
 * （codemirror-git-diff design D7 伪码，浏览器端纯函数，禁止 Node path polyfill）。
 * dotfile（dot=0）、无后缀（dot=-1）、末尾点（dot 为末位）均返回空串 → 纯文本。
 */
export function extractExtension(path: string): string {
  const name = path.slice(path.lastIndexOf('/') + 1);
  const dot = name.lastIndexOf('.');
  return dot > 0 && dot < name.length - 1 ? name.slice(dot).toLowerCase() : '';
}

/** 扩展名 → 语言 loader 静态映射表（design D7 唯一清单）；命中后经动态 import() 按需加载。 */
const languageLoaders: Record<string, () => Promise<Extension>> = {
  '.md': async () => (await import('@codemirror/lang-markdown')).markdown(),
  '.json': async () => (await import('@codemirror/lang-json')).json(),
  '.yaml': async () => (await import('@codemirror/lang-yaml')).yaml(),
  '.yml': async () => (await import('@codemirror/lang-yaml')).yaml(),
  // Go 无官方 lezer 语言包，走 legacy stream parser（design D7）。
  '.go': async () => StreamLanguage.define((await import('@codemirror/legacy-modes/mode/go')).go),
  '.js': async () => (await import('@codemirror/lang-javascript')).javascript(),
  '.jsx': async () => (await import('@codemirror/lang-javascript')).javascript({ jsx: true }),
  '.ts': async () => (await import('@codemirror/lang-javascript')).javascript({ typescript: true }),
  '.tsx': async () =>
    (await import('@codemirror/lang-javascript')).javascript({ typescript: true, jsx: true }),
  '.py': async () => (await import('@codemirror/lang-python')).python(),
  '.html': async () => (await import('@codemirror/lang-html')).html(),
  '.css': async () => (await import('@codemirror/lang-css')).css(),
};

/** 未识别扩展名返回 null，调用方按纯文本渲染（不报错）。
 *  语言 chunk 动态加载失败（如网络中断）同样降级为 null 纯文本，而非让 diff 视图空白。 */
export async function loadLanguage(path: string): Promise<Extension | null> {
  const load = languageLoaders[extractExtension(path)];
  if (!load) return null;
  try {
    return await load();
  } catch {
    return null;
  }
}
