import type { Extension } from '@codemirror/state';
import { EditorState } from '@codemirror/state';
import { EditorView, lineNumbers } from '@codemirror/view';
import { syntaxHighlighting } from '@codemirror/language';
import { classHighlighter } from '@lezer/highlight';

/**
 * 只读视图共享 extensions（codemirror-git-diff design D6）。
 * lineSeparator('\n') 让 `\r` 保留为文档字符——默认 state 创建会把 `\r\n`/`\r`
 * 统一为 `\n`，吞掉纯行尾变更（CRLF↔LF 差异必须可见，见 spec「diff 视图渲染」）。
 * classHighlighter 产生稳定的 `.tok-*` 类，配色见 legacy-components.css。
 */
export const readOnlyExtensions: Extension[] = [
  EditorView.editable.of(false),
  EditorState.readOnly.of(true),
  lineNumbers(),
  EditorState.lineSeparator.of('\n'),
  syntaxHighlighting(classHighlighter),
];

/** 编辑器主题：颜色/字体全部跟随 design-system.css 设计变量，明暗主题经变量自动切换。 */
export const editorTheme: Extension = EditorView.theme({
  '&': {
    color: 'var(--fg)',
    backgroundColor: 'var(--bg)',
    fontFamily: 'var(--font-mono)',
    fontSize: '12px',
  },
  '.cm-gutters': {
    backgroundColor: 'var(--surface)',
    color: 'var(--muted)',
    border: 'none',
    borderRight: '1px solid var(--border)',
  },
  '.cm-activeLine': { backgroundColor: 'transparent' },
  '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--fg)' },
});
