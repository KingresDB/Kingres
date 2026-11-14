<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { defineAsyncComponent, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ProblemNotice from '@/components/ProblemNotice.vue'
import FormField from '@/components/FormField.vue'
import BrandLogo from '@/components/BrandLogo.vue'
import { SessionCookieError, useSessionStore } from '@/stores/session'
import { safeRedirect } from '@/lib/redirect'

const session = useSessionStore()
const route = useRoute()
const router = useRouter()

const username = ref('')
const password = ref('')
const error = ref<unknown>(null)
// bootstrap.ts adds ?expired=1 when a request reports that the session is gone.
const expired = route.query.expired === '1'
const busy = ref(false)
// Only in development with the mock API; the import is removed from production builds.
const DemoHint =
  import.meta.env.DEV && import.meta.env.VITE_MOCKS !== 'off'
    ? defineAsyncComponent(() => import('@/mocks/DemoHint.vue'))
    : null

async function submit(): Promise<void> {
  busy.value = true
  error.value = null
  try {
    await session.signIn(username.value, password.value)
    // The page a previous user was on is not offered to someone else; a redirect must stay inside
    // the panel.
    const previous = session.expiredUser
    session.expiredUser = null
    const target =
      previous !== null && previous !== session.username ? '/' : safeRedirect(route.query.redirect)
    await router.push(target)
  } catch (e) {
    error.value = e
    password.value = ''
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="login">
    <div class="login-box">
      <!-- Decorative: the heading below says "KingresDB" in text. -->
      <BrandLogo variant="stacked" :size="140" decorative />
      <form class="card stack" aria-labelledby="login-title" @submit.prevent="submit">
        <p v-if="expired" class="notice tone-warn" role="status">
          <span aria-hidden="true">&#9650;</span> Your session has expired. Sign in again to
          continue.
        </p>
        <h1 id="login-title">Sign in to KingresDB</h1>
        <p class="muted">Use your PostgreSQL role. Your cluster role decides what you can do.</p>
        <p v-if="error instanceof SessionCookieError" class="notice tone-warn" role="alert">
          <span aria-hidden="true">&#9650;</span>
          <span>
            <strong>Your browser did not keep the session.</strong> The session cookie is only kept
            over HTTPS or when the panel is opened at <code>localhost</code>. Use an HTTPS address
            or open the panel on this server through <code>http://localhost</code>.
          </span>
        </p>
        <ProblemNotice v-else-if="error" :error="error" />
        <FormField v-slot="slot" label="User name">
          <input
            v-bind="slot"
            v-model="username"
            class="input"
            autocomplete="username"
            required
            data-autofocus
          />
        </FormField>
        <FormField v-slot="slot" label="Password">
          <input
            v-bind="slot"
            v-model="password"
            class="input"
            type="password"
            autocomplete="current-password"
            required
          />
        </FormField>
        <button type="submit" class="btn btn-primary" :disabled="busy || !username || !password">
          {{ busy ? 'Signing in...' : 'Sign in' }}
        </button>
        <component :is="DemoHint" v-if="DemoHint" />
      </form>
    </div>
  </main>
</template>

<style scoped>
.login {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: var(--s-4);
}
.login-box {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--s-4);
  width: min(400px, 100%);
}
/* Gold brand strip on the sign-in card (decoration only). */
.login .card {
  position: relative;
  overflow: hidden;
  width: 100%;
}
.login .card::before {
  content: '';
  position: absolute;
  inset: 0 0 auto 0;
  height: 3px;
  background: linear-gradient(90deg, var(--brand-gold-light), var(--brand-gold-deep));
}
</style>
