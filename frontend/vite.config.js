import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react({
    jsxRuntime: 'automatic'
  })],
  server: {
    port: process.env.VITE_PORT ? parseInt(process.env.VITE_PORT) : 12713,
    host: true
  },
  build: {
    // 确保静态资源被正确复制
    assetsDir: 'assets',
    // 产物直接写到 Go 后端的 embed 目录（dockerCopilot/front/），
    // 这样 `npm run build` 之后直接 `go build` 打出来的就是最新前端，
    // 不需要再手工拷贝一次产物。
    outDir: '../front',
    // outDir 落在工程根目录之外，vite 默认会拒绝清空它；
    // 必须显式允许，否则上一轮的旧 chunk 会残留、越堆越多。
    emptyOutDir: true
  },
  esbuild: {
    loader: 'jsx',
    include: /src\/.*\.[jt]sx?$/,
    exclude: []
  },
  optimizeDeps: {
    esbuildOptions: {
      loader: {
        '.js': 'jsx'
      }
    }
  }
})