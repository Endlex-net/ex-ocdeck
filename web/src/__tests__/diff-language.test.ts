// @vitest-environment jsdom
import { describe, it, expect } from 'vitest';
import { extractExtension, loadLanguage } from '../components/editor/language';

/* ============================ 扩展名提取与语言加载（tasks 4.4，design D7） ============================ */

describe('extractExtension（design D7 伪码四锚点）', () => {
  it('大小写归一：a.GO → ".go"', () => {
    expect(extractExtension('a.GO')).toBe('.go');
  });

  it('dotfile：.gitignore → ""（不当作扩展名）', () => {
    expect(extractExtension('.gitignore')).toBe('');
  });

  it('目录名含点不影响：dir.with.dot/a.ts → ".ts"', () => {
    expect(extractExtension('dir.with.dot/a.ts')).toBe('.ts');
  });

  it('末尾点：name. → ""', () => {
    expect(extractExtension('name.')).toBe('');
  });

  it('补充边界：无后缀与多点文件名', () => {
    expect(extractExtension('makefile')).toBe('');
    expect(extractExtension('archive.tar.gz')).toBe('.gz');
    expect(extractExtension('src/README.md')).toBe('.md');
  });
});

describe('loadLanguage（映射表唯一清单，动态加载）', () => {
  it('映射表内扩展名全部可加载为非空 Extension', async () => {
    const paths = [
      'a.md',
      'a.json',
      'a.yaml',
      'a.yml',
      'a.go',
      'a.js',
      'a.jsx',
      'a.ts',
      'a.tsx',
      'a.py',
      'a.html',
      'a.css',
    ];
    for (const p of paths) {
      expect(await loadLanguage(p), p).not.toBeNull();
    }
  });

  it('未识别扩展名返回 null（纯文本降级，不报错）', async () => {
    expect(await loadLanguage('makefile')).toBeNull();
    expect(await loadLanguage('.gitignore')).toBeNull();
    expect(await loadLanguage('data.txt')).toBeNull();
  });
});
