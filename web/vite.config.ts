import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // 构建产物输出到后端 embed 目录，由 Go 二进制托管（单容器部署）。
    outDir: '../server/web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    // 开发期把 /api 代理到本地后端，避免跨域。
    proxy: {
      '/api': {
        target: 'http://localhost:3000',
        changeOrigin: true,
      },
    },
  },
})
