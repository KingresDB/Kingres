<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Application shell: top bar, side navigation (Cluster / This server), global banners, content.
// Navigation items come from the router's route table; an item is shown only when the signed-in
// role satisfies the `x-required-role` of the operation the page is built on.
import { computed, ref, watch } from 'vue'
import BrandLogo from '@/components/BrandLogo.vue'
import { useMediaQuery } from '@/composables/useMediaQuery'
import { RouterLink, RouterView, useRoute, useRouter } from 'vue-router'
import { useSessionStore } from '@/stores/session'
import { useThemeStore } from '@/stores/theme'
import { useQueryClient } from '@tanstack/vue-query'
import GlobalBanners from '@/components/GlobalBanners.vue'

const router = useRouter()
const route = useRoute()
const session = useSessionStore()
const theme = useThemeStore()
const queryClient = useQueryClient()
const navOpen = ref(false)
const narrow = useMediaQuery('(max-width: 860px)')

function itemsFor(scope: 'cluster' | 'server') {
  return router
    .getRoutes()
    .filter((r) => r.meta.nav === scope && r.meta.operation && session.can(r.meta.operation))
    .map((r) => ({ name: String(r.name), path: r.path, title: r.meta.title ?? String(r.name) }))
}
const clusterItems = computed(() => itemsFor('cluster'))
const serverItems = computed(() => itemsFor('server'))

const themeLabel = computed(
  () => ({ system: 'System theme', light: 'Light theme', dark: 'Dark theme' })[theme.mode],
)

async function signOut(): Promise<void> {
  try {
    await session.signOut()
  } finally {
    queryClient.clear()
    await router.push({ name: 'login' })
  }
}

watch(
  () => route.fullPath,
  () => {
    navOpen.value = false
  },
)
</script>

<template>
  <div class="shell">
    <a class="skip" href="#main">Skip to content</a>
    <header class="topbar">
      <button
        type="button"
        class="btn btn-small menu-btn"
        :aria-expanded="navOpen"
        aria-controls="side-nav"
        @click="navOpen = !navOpen"
      >
        Menu
      </button>
      <!-- The logo names the product (accessible name "KingresDB"); on narrow screens the mark alone. -->
      <RouterLink :to="{ name: 'dashboard' }" class="brand-link">
        <BrandLogo :variant="narrow ? 'mark' : 'horizontal'" :size="30" />
      </RouterLink>
      <span class="cluster-name" :title="session.session?.clusterId ?? undefined">
        {{ session.session?.clusterName ?? 'This server' }}
      </span>
      <span class="spacer" />
      <button
        type="button"
        class="btn btn-small"
        :aria-label="`${themeLabel}. Switch theme`"
        @click="theme.cycle()"
      >
        <span aria-hidden="true">{{
          theme.mode === 'dark' ? '☾' : theme.mode === 'light' ? '☀' : '◑'
        }}</span>
        {{ themeLabel }}
      </button>
      <span class="user">
        <span class="sr-only">Signed in as</span>
        <strong>{{ session.username }}</strong>
        <span class="role mono" :title="'Highest cluster role'">{{
          session.role ?? 'no cluster role'
        }}</span>
      </span>
      <button type="button" class="btn btn-small" @click="signOut">Sign out</button>
    </header>

    <GlobalBanners />

    <div class="body">
      <nav id="side-nav" class="side" :class="{ open: navOpen }" aria-label="Main">
        <template v-if="clusterItems.length">
          <h2 class="group">Cluster</h2>
          <ul>
            <li v-for="item in clusterItems" :key="item.name">
              <RouterLink :to="{ name: item.name }">{{ item.title }}</RouterLink>
            </li>
          </ul>
        </template>
        <template v-if="serverItems.length">
          <h2 class="group">This server</h2>
          <ul>
            <li v-for="item in serverItems" :key="item.name">
              <RouterLink :to="{ name: item.name }">{{ item.title }}</RouterLink>
            </li>
          </ul>
        </template>
      </nav>
      <main id="main" tabindex="-1" class="content">
        <RouterView />
      </main>
    </div>
  </div>
</template>

<style scoped>
.shell {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
}
.skip {
  position: absolute;
  left: -999px;
  top: 0;
  background: var(--c-surface);
  padding: var(--s-2) var(--s-3);
  z-index: 200;
}
.skip:focus {
  left: var(--s-2);
  top: var(--s-2);
}
.topbar {
  position: sticky;
  top: 0;
  z-index: 50;
  display: flex;
  align-items: center;
  gap: var(--s-3);
  height: var(--topbar-h);
  padding: 0 var(--s-4);
  background: var(--c-surface);
  border-bottom: 1px solid var(--c-border);
}
.brand-link {
  display: inline-flex;
  align-items: center;
  border-radius: var(--radius-sm);
}
/* A thin gold strip: a brand accent that carries no meaning, so it does not depend on contrast. */
.topbar::before {
  content: '';
  position: absolute;
  inset: 0 0 auto 0;
  height: 3px;
  background: linear-gradient(90deg, var(--brand-gold-light), var(--brand-gold-deep));
}
.cluster-name {
  color: var(--c-text-muted);
  padding-left: var(--s-3);
  border-left: 1px solid var(--c-border);
}
.spacer {
  flex: 1;
}
.user {
  display: inline-flex;
  align-items: baseline;
  gap: var(--s-2);
}
.role {
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}
.menu-btn {
  display: none;
}
.body {
  display: flex;
  flex: 1;
  min-height: 0;
}
.side {
  width: var(--nav-w);
  flex: none;
  padding: var(--s-3) var(--s-2);
  background: var(--c-surface);
  border-right: 1px solid var(--c-border);
}
.side ul {
  list-style: none;
  margin: 0 0 var(--s-3);
  padding: 0;
}
.group {
  font-size: var(--fs-xs);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--c-text-muted);
  padding: var(--s-2) var(--s-3);
}
.side a {
  display: block;
  padding: var(--s-1) var(--s-3);
  border-radius: var(--radius-sm);
  color: var(--c-text);
  text-decoration: none;
}
.side a:hover {
  background: var(--c-surface-2);
}
.side a.router-link-active {
  box-shadow: inset 3px 0 0 var(--c-brand);
  background: var(--c-info-bg);
  color: var(--c-info-fg);
  font-weight: 600;
}
.content {
  flex: 1;
  min-width: 0;
  padding: var(--s-5);
}
.content:focus {
  outline: none;
}
@media (max-width: 860px) {
  .menu-btn {
    display: inline-flex;
  }
  .side {
    display: none;
    position: fixed;
    top: var(--topbar-h);
    bottom: 0;
    z-index: 60;
    overflow-y: auto;
  }
  .side.open {
    display: block;
  }
  .content {
    padding: var(--s-3);
  }
  .cluster-name {
    display: none;
  }
}
</style>
