import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:4096', changeOrigin: true },
      '/ws': { target: 'ws://localhost:4096', ws: true },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    // es2020 默认 target 下 esbuild 0.21.x 会把 @xterm/xterm@6.0.0 requestMode 的 ||=
    // 降级误压缩为未声明变量（终端白屏/卡死，esbuild#4508 / xterm#5800）。
    // es2021 原生支持 ||=，规避该降级路径；浏览器覆盖与 es2020 基线等价。
    target: 'es2021',
  },
});
