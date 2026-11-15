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
import GlobalBanners from '@/components/GlobalBanners.vue'
import TaskNotices from '@/components/TaskNotices.vue'
import LiveRegions from '@/components/LiveRegions.vue'
import SafeName from '@/components/SafeName.vue'
import { safeText } from '@/lib/safe-text'
import SystemDatabaseStopped from '@/components/this-server/SystemDatabaseStopped.vue'
import { availability, rememberInstances } from '@/api/availability'
import { useThisServerInstances } from '@/api/queries'
import { createAnnouncer } from '@/composables/useAnnounce'

const router = useRouter()
const route = useRoute()
const session = useSessionStore()
const theme = useThemeStore()
const announcer = createAnnouncer()
const navOpen = ref(false)
// While the server can still answer, learn which instance hosts the system database: when it is stopped
// the list can no longer be read, and "Start instance" needs the id.
const instances = useThisServerInstances(() => session.can('listThisServerInstances'))
watch(
  () => instances.data.value?.items,
  (items) => {
    if (items) rememberInstances(items)
  },
  { immediate: true },
)
const clusterTitle = computed(() => {
  const id = session.session?.clusterId
  return id ? safeText(id, 60) : undefined
})
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

/** Ending the session drops all server data (session.adopt), whatever the way it ended. */
async function signOut(): Promise<void> {
  try {
    await session.signOut()
  } finally {
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
      <span class="cluster-name" :title="clusterTitle">
        <SafeName :value="session.session?.clusterName ?? 'This server'" kind="text" :max="60" />
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
        <strong><SafeName :value="session.username ?? ''" kind="text" :max="40" /></strong>
        <span class="role mono" :title="'Highest cluster role'">{{
          session.role ?? 'no cluster role'
        }}</span>
      </span>
      <button type="button" class="btn btn-small" @click="signOut">Sign out</button>
    </header>

    <LiveRegions :polite="announcer.polite.value" :assertive="announcer.assertive.value" />
    <div
      v-if="session.identityNotice"
      class="banner tone-warn"
      role="status"
      data-testid="banner-identity"
    >
      <span aria-hidden="true">&#9650;</span>
      <span>
        Another browser tab signed in as
        <strong><SafeName :value="session.identityNotice" kind="text" :max="40" /></strong>. This
        tab now shows that account, and the data of the previous account was removed.
      </span>
      <button type="button" class="btn btn-small" @click="session.identityNotice = null">
        Dismiss
      </button>
    </div>
    <GlobalBanners />
    <TaskNotices />

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
        <SystemDatabaseStopped v-if="availability.systemDatabaseStopped" />
        <RouterView v-else />
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
  flex-wrap: wrap;
  gap: var(--s-1) var(--s-3);
  min-height: var(--topbar-h);
  padding: var(--s-1) var(--s-4);
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
  min-width: 0;
  max-width: 20em;
  overflow-wrap: anywhere;
}
.spacer {
  flex: 1;
}
.user {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0 var(--s-2);
  min-width: 0;
  max-width: 100%;
  overflow-wrap: anywhere;
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
  /* The menu opens below the top bar (whatever height the bar has) instead of beside the page. */
  .body {
    flex-direction: column;
  }
  .side {
    display: none;
    width: 100%;
    border-right: 0;
    border-bottom: 1px solid var(--c-border);
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
@media (max-width: 600px) {
  .topbar {
    padding-inline: var(--s-2);
  }
  /* Only the icon of the theme switch at small widths; its accessible name stays. */
  .theme-label {
    display: none;
  }
}
</style>
