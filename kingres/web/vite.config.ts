// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import { readFileSync } from 'node:fs'
import { fileURLToPath, URL } from 'node:url'
import { type Plugin } from 'vite'
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

/**
 * Serves the Mock Service Worker script from node_modules during development only.
 * It is deliberately not copied into `public/`, so it can never end up in `dist/`.
 */
function mswWorker(): Plugin {
  return {
    name: 'kingres-msw-worker',
    apply: 'serve',
    configureServer(server) {
      server.middlewares.use('/mockServiceWorker.js', (_req, res) => {
        const file = fileURLToPath(
          new URL('./node_modules/msw/lib/mockServiceWorker.js', import.meta.url),
        )
        res.setHeader('Content-Type', 'text/javascript')
        res.end(readFileSync(file))
      })
    },
  }
}

export default defineConfig({
  plugins: [vue(), mswWorker()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  server: {
    port: 5173,
    // Used when mocks are off (`npm run dev:backend`): forward the REST API to a local kingresd.
    proxy: { '/api': { target: 'http://127.0.0.1:8091', changeOrigin: false } },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    rollupOptions: {
      output: {
        // Framework code changes rarely; keep it in its own long-lived chunk.
        manualChunks(id) {
          if (id.includes('node_modules/@tanstack')) return 'query'
          if (/node_modules\/(vue|@vue|vue-router|pinia)\//.test(id)) return 'framework'
          return undefined
        },
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./tests/setup.ts'],
    include: ['tests/**/*.test.ts', 'src/**/*.test.ts'],
    css: false,
    testTimeout: 15000,
  },
})
