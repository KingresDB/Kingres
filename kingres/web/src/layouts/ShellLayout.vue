<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Application shell: top bar, side navigation (Cluster / This server), global banners, content.
// Navigation items come from the router's route table; an item is shown only when the signed-in
// role satisfies the `x-required-role` of the operation the page is built on.
import { computed, ref, watch } from 'vue'
import AppFooter from '@/components/AppFooter.vue'
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
/** The system database is stopped: the server accepts only start, session and instance list. */
const stopped = computed(() => availability.systemDatabaseStopped)
const signOutReason =
  'Signing out is not possible while the system database is stopped: the server cannot end the session now. Start the instance first.'
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
        class="top-btn menu-btn"
        aria-label="Menu"
        :aria-expanded="navOpen"
        aria-controls="side-nav"
        @click="navOpen = !navOpen"
      >
        <svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true" focusable="false">
          <path d="M2 4.5h14M2 9h14M2 13.5h14" stroke="currentColor" stroke-width="1.8" />
        </svg>
      </button>
      <!-- The logo names the product (accessible name "KingresDB"); on narrow screens the mark alone. -->
      <RouterLink :to="{ name: 'dashboard' }" class="brand-link">
        <BrandLogo :variant="narrow ? 'mark' : 'horizontal'" :size="narrow ? 30 : 38" />
      </RouterLink>
      <span class="cluster-name" :title="clusterTitle">
        <SafeName :value="session.session?.clusterName ?? 'This server'" kind="text" :max="60" />
      </span>
      <span class="spacer" />
      <button
        type="button"
        class="top-btn"
        :aria-label="`${themeLabel}. Switch theme`"
        @click="theme.cycle()"
      >
        <span aria-hidden="true">{{
          theme.mode === 'dark' ? '☾' : theme.mode === 'light' ? '☀' : '◑'
        }}</span>
        <span class="theme-label">{{ themeLabel }}</span>
      </button>
      <span class="user">
        <span class="sr-only">Signed in as</span>
        <strong class="user-name"
          ><SafeName :value="session.username ?? ''" kind="text" :max="40"
        /></strong>
        <span class="role mono" :title="'Highest cluster role'">{{
          session.role ?? 'no cluster role'
        }}</span>
      </span>
      <button
        type="button"
        class="top-btn"
        :aria-disabled="stopped ? 'true' : undefined"
        :aria-describedby="stopped ? 'signout-off-reason' : undefined"
        :title="stopped ? signOutReason : undefined"
        @click="stopped ? undefined : signOut()"
      >
        Sign out
      </button>
      <span v-if="stopped" id="signout-off-reason" class="sr-only">{{ signOutReason }}</span>
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
              <span
                v-if="stopped"
                class="nav-off"
                aria-disabled="true"
                data-testid="nav-disabled"
                >{{ item.title }}</span
              >
              <RouterLink v-else :to="{ name: item.name }">{{ item.title }}</RouterLink>
            </li>
          </ul>
        </template>
        <template v-if="serverItems.length">
          <h2 class="group">This server</h2>
          <ul>
            <li v-for="item in serverItems" :key="item.name">
              <span
                v-if="stopped"
                class="nav-off"
                aria-disabled="true"
                data-testid="nav-disabled"
                >{{ item.title }}</span
              >
              <RouterLink v-else :to="{ name: item.name }">{{ item.title }}</RouterLink>
            </li>
          </ul>
        </template>
        <p v-if="stopped" id="nav-off-reason" class="nav-note" data-testid="nav-off-reason">
          Switched off: the system database is stopped, and the server accepts only "start instance"
          until it runs again.
        </p>
      </nav>
      <main id="main" tabindex="-1" class="content">
        <SystemDatabaseStopped v-if="availability.systemDatabaseStopped" />
        <RouterView v-else />
      </main>
    </div>
    <AppFooter />
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
/* The top bar is one row at every width and black in both themes. What does not fit is shortened
   (cluster name, user name) or reduced to its icon (theme switch); nothing wraps to a second row. */
.topbar {
  position: sticky;
  top: 0;
  z-index: 50;
  display: flex;
  align-items: center;
  gap: var(--s-3);
  min-height: var(--topbar-h);
  padding: 3px var(--s-4) 0;
  background: var(--topbar-bg);
  color: var(--topbar-fg);
  border-bottom: 1px solid var(--topbar-border);
  /* The logo on black takes the colouring made for dark backgrounds, whatever the theme. */
  --logo-crown: var(--brand-cream);
  --logo-db: var(--brand-gold-light);
}
.topbar :focus-visible {
  outline-color: var(--topbar-focus);
}
.brand-link {
  display: inline-flex;
  align-items: center;
  flex: none;
  border-radius: var(--radius-sm);
}
.top-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: var(--s-2);
  flex: none;
  min-height: 32px;
  min-width: 32px;
  padding: 0 var(--s-3);
  font: inherit;
  font-size: var(--fs-sm);
  font-weight: 500;
  white-space: nowrap;
  color: var(--topbar-fg);
  background: transparent;
  border: 1px solid var(--topbar-border);
  border-radius: var(--radius-sm);
  cursor: pointer;
}
.top-btn:hover:not([aria-disabled='true']) {
  background: var(--topbar-hover);
}
.top-btn[aria-disabled='true'] {
  opacity: 0.55;
  cursor: not-allowed;
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
  color: var(--topbar-muted);
  padding-left: var(--s-3);
  border-left: 1px solid var(--topbar-border);
  min-width: 0;
  max-width: 20em;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.spacer {
  flex: 1;
  min-width: 0;
}
.user {
  display: inline-flex;
  flex-direction: column;
  justify-content: center;
  min-width: 0;
  line-height: 1.2;
  text-align: right;
}
.user-name {
  max-width: 16em;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.role {
  font-size: var(--fs-xs);
  color: var(--topbar-muted);
  white-space: nowrap;
}
.menu-btn {
  display: none;
  padding: 0;
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
.nav-off {
  display: block;
  padding: var(--s-1) var(--s-3);
  color: var(--c-text-muted);
  text-decoration: line-through;
}
.nav-note {
  margin: var(--s-2) var(--s-3);
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
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
    gap: var(--s-2);
  }
  /* Only the icon of the theme switch at small widths; its accessible name stays. */
  .theme-label {
    display: none;
  }
  .top-btn {
    padding: 0 var(--s-2);
  }
  .user-name {
    max-width: 7em;
  }
  /* The role stays in the page for assistive technology; there is no room to show it. */
  .role {
    position: absolute;
    width: 1px;
    height: 1px;
    margin: -1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
  }
}
</style>
