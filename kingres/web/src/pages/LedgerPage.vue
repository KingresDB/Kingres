<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Ledger: a page with a tab bar. Overview (status of a database: one hash chain per shard group, what is published and
// anchored, verification, blocks), Digests (digests, aggregate checkpoints and row inclusion proofs), Anchoring (the targets
// and the anchor records), Signing (the keys of the cluster) and Publication (the logs and their settings). Each tab is a component under components/ledger/. The
// database is chosen once for the two tabs that are about a database; Signing and Publication are about the cluster and
// work without a database that uses the ledger.
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import TabBar from '@/components/TabBar.vue'
import SafeName from '@/components/SafeName.vue'
import { safeText } from '@/lib/safe-text'
import { describeProblem } from '@/api/problem'
import { useLedgerStatusList } from '@/api/queries'
import OverviewTab from '@/components/ledger/OverviewTab.vue'
import DigestsTab from '@/components/ledger/DigestsTab.vue'
import AnchoringTab from '@/components/ledger/AnchoringTab.vue'
import ClientKeysTab from '@/components/ledger/ClientKeysTab.vue'
import SigningTab from '@/components/ledger/SigningTab.vue'
import PublicationTab from '@/components/ledger/PublicationTab.vue'

const route = useRoute()
const router = useRouter()
const statusList = useLedgerStatusList()
const dbName = ref(typeof route.query.db === 'string' ? route.query.db : '')

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'digests', label: 'Digests' },
  { id: 'anchoring', label: 'Anchoring' },
  { id: 'clients', label: 'Client keys' },
  { id: 'signing', label: 'Signing' },
  { id: 'publication', label: 'Publication' },
]
const tab = computed({
  get: () => {
    const t = route.query.tab
    return TABS.some((x) => x.id === t) ? (t as string) : 'overview'
  },
  set: (id: string) => {
    void router.replace({ query: queryOf(dbName.value, id) })
  },
})
/** The query of the address: the database and the tab, each only when it is not the default. */
function queryOf(db: string, id: string): Record<string, string> {
  return { ...(db ? { db } : {}), ...(id === 'overview' ? {} : { tab: id }) }
}

watch(
  () => statusList.data.value,
  (d) => {
    // Until the list has arrived there is nothing to choose from: keep what the URL named
    // (it used to be reset to the first database, and the URL with it, on the first load).
    if (!d) return
    const items = d.items
    if (!items.some((s) => s.databaseName === dbName.value))
      dbName.value = items[0]?.databaseName ?? ''
  },
  { immediate: true },
)
watch(dbName, (v) => {
  void router.replace({ query: queryOf(v, tab.value) })
})

const status = computed(() =>
  statusList.data.value?.items.find((s) => s.databaseName === dbName.value),
)
const aboutDatabase = computed(() => tab.value === 'overview' || tab.value === 'digests')
</script>

<template>
  <PageHeader
    title="Ledger"
    subtitle="Tamper-evident history: hash chains, digests, anchoring, signing and publication."
  />
  <TabBar v-model="tab" :tabs="TABS" label="Ledger" />

  <template v-if="aboutDatabase">
    <section
      v-if="statusList.data.value?.unreadable.length"
      class="notice tone-danger"
      role="alert"
      aria-label="The ledger status could not be read"
    >
      <span aria-hidden="true">&#10006;</span>
      <div class="stack" style="gap: var(--s-1)">
        <strong
          >The ledger status could not be read for these databases; the list may be
          incomplete.</strong
        >
        <ul style="margin: 0; padding-left: 1.2em">
          <li v-for="u in statusList.data.value.unreadable" :key="u.databaseName">
            <SafeName :value="u.databaseName" :max="60" />:
            <SafeName :value="describeProblem(u.error).message" kind="text" :max="200" />
          </li>
        </ul>
      </div>
    </section>
    <QueryBoundary
      :loading="statusList.isPending.value"
      :error="statusList.error.value"
      :empty="
        (statusList.data.value?.items.length ?? 0) === 0 &&
        (statusList.data.value?.unreadable.length ?? 0) === 0
      "
      empty-title="No database uses the ledger"
      empty-hint="Turn on the ledger for a database to see its chains here."
      @retry="statusList.refetch()"
    >
      <div class="stack-lg">
        <div class="field" style="max-width: 20em">
          <label for="l-db">Database</label>
          <select id="l-db" v-model="dbName">
            <option
              v-for="s in statusList.data.value?.items"
              :key="s.databaseName"
              :value="s.databaseName"
            >
              {{ safeText(s.databaseName) }}
            </option>
          </select>
        </div>
        <template v-if="status">
          <div
            v-if="tab === 'overview'"
            id="panel-overview"
            role="tabpanel"
            aria-labelledby="tab-overview"
          >
            <OverviewTab :key="dbName" :database-name="dbName" :status="status" />
          </div>
          <div v-else id="panel-digests" role="tabpanel" aria-labelledby="tab-digests">
            <DigestsTab :key="dbName" :database-name="dbName" />
          </div>
        </template>
      </div>
    </QueryBoundary>
  </template>
  <div
    v-else-if="tab === 'anchoring'"
    id="panel-anchoring"
    role="tabpanel"
    aria-labelledby="tab-anchoring"
  >
    <AnchoringTab v-model:database-name="dbName" />
  </div>
  <div
    v-else-if="tab === 'clients'"
    id="panel-clients"
    role="tabpanel"
    aria-labelledby="tab-clients"
  >
    <ClientKeysTab v-model:database-name="dbName" />
  </div>
  <div
    v-else-if="tab === 'signing'"
    id="panel-signing"
    role="tabpanel"
    aria-labelledby="tab-signing"
  >
    <SigningTab />
  </div>
  <div v-else id="panel-publication" role="tabpanel" aria-labelledby="tab-publication">
    <PublicationTab />
  </div>
</template>
