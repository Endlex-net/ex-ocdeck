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
/** 可编辑扩展（diff-review-workbench D5）：编辑模式以此替换 readOnlyExtensions。
 *  MUST NOT 设 lineSeparator('\n')：该 facet 会让 CM 只按 '\n' 切行，粘贴的 \r\n/\r
 *  原样进入文档并被 toString() 送入写回（后端 invalid_input 拒绝）。缺省 DefaultSplit
 *  （/\r\n?|\n/）会把插入文本归一为 \n 行，doc.toString() 恒以 \n 连接——正好满足写回协议。 */
export const editableExtensions: Extension[] = [
  lineNumbers(),
  syntaxHighlighting(classHighlighter),
];

export const readOnlyExtensions: Extension[] = [
  EditorView.editable.of(false),
  EditorState.readOnly.of(true),
  lineNumbers(),
  EditorState.lineSeparator.of('\n'),
  syntaxHighlighting(classHighlighter),
];

/** 编辑器主题：颜色/字体全部跟随 design-system.css 设计变量，明暗主题经变量自动切换。
 *  光标：CM 基座主题为 `.cm-cursor` 硬编码黑色 border-left（深色覆盖仅在其自带 dark 主题下
 *  生效，本主题未声明 dark）——暗色模式下光标不可见。此处经 --editor-caret 变量显式接管。 */
export const editorTheme: Extension = EditorView.theme({
  '&': {
    color: 'var(--fg)',
    backgroundColor: 'var(--bg)',
    fontFamily: 'var(--font-mono)',
    fontSize: '12px',
    caretColor: 'var(--editor-caret)',
  },
  '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--editor-caret)' },
  '.cm-gutters': {
    backgroundColor: 'var(--surface)',
    color: 'var(--muted)',
    border: 'none',
    borderRight: '1px solid var(--border)',
  },
  '.cm-activeLine': { backgroundColor: 'transparent' },
  '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--fg)' },
});
