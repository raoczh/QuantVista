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
    // capacitor chunk 仅 Android 壳内动态加载；从 modulepreload 剔除，
    // 浏览器端连预下载都不发生（modulepreload 虽不执行模块，但会白下载）。
    modulePreload: {
      resolveDependencies(_filename, deps) {
        return deps.filter((d) => !d.includes('capacitor'))
      },
    },
    rollupOptions: {
      output: {
        // @capacitor/* 钉进独立 chunk：源码里全部经动态 import 引用（isNativeApp
        // 守卫），但多个动态入口共享 @capacitor/core 时 rollup 会把 core 提升进
        // 入口 chunk（其顶层副作用会给浏览器 window 装 Capacitor polyfill）。
        // 强制独立后仅 Android 壳内加载，浏览器端零加载零增重。
        manualChunks(id) {
          if (id.includes('node_modules/@capacitor/')) return 'capacitor'
        },
      },
    },
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
