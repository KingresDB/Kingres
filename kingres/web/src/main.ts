// SPDX-License-Identifier: PostgreSQL
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
  createKingres().install(app)
  app.mount('#app')
}

void bootstrap()
