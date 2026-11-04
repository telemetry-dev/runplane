import tailwindcss from '@tailwindcss/vite'
import { tanstackStart } from '@tanstack/react-start/plugin/vite'
import viteReact from '@vitejs/plugin-react'
import { defineConfig, type ProxyOptions } from 'vite'

// The Go server (runplane serve) listens on :8484 by default.
const apiTarget = process.env.RUNPLANE_API ?? 'http://localhost:8484'

export default defineConfig({
  // Parallel dev servers in one checkout each need their own dependency cache.
  cacheDir: process.env.VITE_CACHE_DIR ?? 'node_modules/.vite',
  resolve: { tsconfigPaths: true },
  server: {
    port: 3000,
    proxy: {
      // ws: true also forwards the debug shell WebSocket.
      '/api': { target: apiTarget, changeOrigin: true, ws: true, configure: flushEventStreams },
      '/install.sh': { target: apiTarget, changeOrigin: true },
    },
  },
  plugins: [
    tanstackStart({
      // The Go binary serves the static build, so render only the shell (dist/client/index.html).
      spa: { enabled: true, prerender: { outputPath: '/index' } },
    }),
    viteReact(),
    tailwindcss(),
  ],
})

// The proxy sends response headers with the first body chunk. An SSE stream may
// stay quiet for 15 seconds, and EventSource reports "open" only on headers, so
// send them at once. The microtask runs after the proxy has written the headers.
function flushEventStreams(proxy: Parameters<NonNullable<ProxyOptions['configure']>>[0]) {
  proxy.on('proxyRes', (proxyRes, _req, res) => {
    if (proxyRes.headers['content-type']?.startsWith('text/event-stream')) queueMicrotask(() => res.flushHeaders())
  })
}
