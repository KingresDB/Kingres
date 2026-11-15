// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { createApp } from 'vue'
import App from './App.vue'
import { createKingres } from './bootstrap'
import './styles/tokens.css'
import './styles/base.css'

/** Starts the mock API when running `npm run dev` without a backend. Never included in builds. */
async function startMocks(): Promise<void> {
  if (import.meta.env.DEV && import.meta.env.VITE_MOCKS !== 'off') {
    const { startBrowserMocks } = await import('./mocks/browser')
    await startBrowserMocks()
  }
}

async function bootstrap(): Promise<void> {
  await startMocks()
  const app = createApp(App)
  const kingres = createKingres()
  kingres.install(app)
  // The browser tests drive the application through its router (development build only).
  if (import.meta.env.DEV) (window as unknown as { __router?: unknown }).__router = kingres.router
  app.mount('#app')
}

void bootstrap()
