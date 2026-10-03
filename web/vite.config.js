import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// `npm run dev` 的 API 代理目标，可用 ND_API_TARGET=http://localhost:9999 npm run dev 覆盖。
// 默认不用 :8080，它常被连旧后端的 SSH 隧道占用。
const apiTarget = process.env.ND_API_TARGET || 'http://localhost:8091';

// 构建产物经 go:embed 嵌入 Go 二进制（web/embed.go → all:dist）并同源提供，资源路径必须用相对 base。
export default defineConfig({
  plugins: [react()],
  base: './',
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    proxy: {
      '/api': apiTarget,
      '/boot': apiTarget,
      '/healthz': apiTarget,
    },
  },
  // 测试跑在 jsdom 上，因为要验证的是浏览器行为：点击生效、轮询结束、计数渲染。
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.jsx'],
  },
});
