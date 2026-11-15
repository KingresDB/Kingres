<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The line at the bottom of every page of the console (also the sign-in page and the first-run
// wizard): the copyright, the link to the product's site, the licence and the link to the source of
// the version that runs (AGPL-3.0-only, section 13: a user of a network service can get its source).
// The server reports its version and its source commit in GET /setup/state, which needs no session.
// What it reports is text from the server: the commit becomes part of an address only when it is a
// hexadecimal id, anything else leaves the link at the repository. Every link opens in a new tab
// without giving the target a handle on this page or its address.
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { safeText } from '@/lib/safe-text'

const REPOSITORY = 'https://github.com/KingresDB/Kingres'
const LICENCE = 'https://www.gnu.org/licenses/agpl-3.0.html'
const COMMIT = /^[0-9a-f]{7,64}$/

const node = useQuery({
  queryKey: keys.nodeState,
  queryFn: endpoints.getNodeState,
  staleTime: 30_000,
  retry: false,
})

const sourceUrl = computed(() => {
  const commit = node.data.value?.commit
  return typeof commit === 'string' && COMMIT.test(commit)
    ? `${REPOSITORY}/tree/${commit}`
    : REPOSITORY
})
const versionText = computed(() => {
  const d = node.data.value
  if (!d?.version) return ''
  const pg = d.postgresVersion ? ` on PostgreSQL ${safeText(d.postgresVersion, 30)}` : ''
  return `KingresDB ${safeText(d.version, 30)}${pg}`
})
</script>

<template>
  <footer class="app-footer">
    <span>Copyright &copy; 2026 RooyeKhat Media Co.</span>
    <a href="https://kingresdb.com" target="_blank" rel="noopener noreferrer"
      >kingresdb.com<span class="sr-only"> (opens in a new tab)</span></a
    >
    <span>
      Licensed under
      <a :href="LICENCE" target="_blank" rel="noopener noreferrer" data-testid="licence-link"
        >AGPL-3.0<span class="sr-only"> (licence text, opens in a new tab)</span></a
      >
    </span>
    <span class="source">
      <a :href="sourceUrl" target="_blank" rel="noopener noreferrer" data-testid="source-link"
        >Source code<span class="sr-only"> (opens in a new tab)</span></a
      >
      <span v-if="versionText" data-testid="running-version">{{ versionText }}</span>
    </span>
  </footer>
</template>

<style scoped>
.app-footer {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: center;
  gap: var(--s-1) var(--s-3);
  padding: var(--s-3) var(--s-4);
  font-size: var(--fs-sm);
  color: var(--c-text-muted);
  border-top: 1px solid var(--c-border);
}
.source {
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--s-1) var(--s-2);
  justify-content: center;
}
</style>
