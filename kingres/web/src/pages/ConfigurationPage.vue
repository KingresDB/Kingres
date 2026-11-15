<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Configuration (contract 0.8.0): the PostgreSQL settings of the servers at the cluster, server group, server and
// database level, the history of changes with revert, and the clocks and time zones. Changing is for administrators;
// everyone who may read the cluster sees the values. A change never restarts anything: what needs a restart is listed
// and restarted by a decision of its own.
import { computed, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import TabBar from '@/components/TabBar.vue'
import GateButton from '@/components/GateButton.vue'
import SafeName from '@/components/SafeName.vue'
import ParametersPanel from '@/components/config/ParametersPanel.vue'
import ChangesPanel from '@/components/config/ChangesPanel.vue'
import TimePanel from '@/components/config/TimePanel.vue'
import ConfigEditDialog from '@/components/config/ConfigEditDialog.vue'
import RestartPendingDialog from '@/components/config/RestartPendingDialog.vue'
import { useConfigServers } from '@/api/queries'
import { isUnsupported } from '@/api/problem'
import type { ConfigChange } from '@/api/types'

const route = useRoute()
const router = useRouter()

const TABS = [
  { id: 'parameters', label: 'Parameters' },
  { id: 'changes', label: 'Changes' },
  { id: 'time', label: 'Time' },
]
const tab = computed({
  get: () => {
    const t = route.query.tab
    return TABS.some((x) => x.id === t) ? (t as string) : 'parameters'
  },
  set: (id: string) => {
    void router.replace({ query: id === 'parameters' ? {} : { tab: id } })
  },
})

const servers = useConfigServers()
const status = computed(() => servers.data.value?.items ?? [])
const pending = computed(() => status.value.filter((s) => s.pendingRestart.length > 0))
const failed = computed(() => status.value.filter((s) => s.state === 'failed'))
const waiting = computed(() => status.value.filter((s) => s.state === 'waiting'))
const unsupported = computed(() => isUnsupported(servers.error.value))

const editing = ref<string | null>(null)
const reverting = ref<ConfigChange | null>(null)
const restarting = ref(false)
</script>

<template>
  <PageHeader
    title="Configuration"
    subtitle="PostgreSQL settings of the servers, the history of changes, the clocks and the time zones."
  >
    <template #actions>
      <GateButton
        op="restartPendingInstances"
        :blocked-reason="pending.length === 0 ? 'No instance waits for a restart.' : ''"
        @click="restarting = true"
        >Restart pending instances</GateButton
      >
    </template>
  </PageHeader>

  <div
    v-if="!unsupported && (pending.length > 0 || failed.length > 0 || waiting.length > 0)"
    class="stack status"
  >
    <p v-if="pending.length > 0" class="notice tone-warn" data-testid="pending-note">
      <span aria-hidden="true">&#9650;</span>
      <span
        >{{ pending.length }} {{ pending.length === 1 ? 'server waits' : 'servers wait' }} for a
        restart:
        <template v-for="(s, i) in pending" :key="s.serverId"
          ><RouterLink :to="{ name: 'configuration', query: { pendingRestart: '1' } }"
            ><SafeName :value="s.serverId" kind="text" :max="60" /></RouterLink
          ><template v-if="i < pending.length - 1">, </template></template
        >. Nothing is restarted by a change.</span
      >
    </p>
    <p v-if="failed.length > 0" class="notice tone-danger" role="status" data-testid="failed-note">
      <span aria-hidden="true">&#10006;</span>
      <span
        >A change failed on
        <template v-for="(s, i) in failed" :key="s.serverId"
          ><SafeName :value="s.serverId" kind="text" :max="60" /><template
            v-if="i < failed.length - 1"
            >,
          </template></template
        >. Change the value again to retry it, or revert the change.</span
      >
    </p>
    <p v-if="waiting.length > 0" class="notice tone-info" role="status" data-testid="waiting-note">
      <span aria-hidden="true">&#9719;</span>
      <span
        >Waiting for
        <template v-for="(s, i) in waiting" :key="s.serverId"
          ><SafeName :value="s.serverId" kind="text" :max="60" /><template
            v-if="i < waiting.length - 1"
            >,
          </template></template
        >: the change is applied when the server is back.</span
      >
    </p>
  </div>

  <TabBar v-model="tab" :tabs="TABS" label="Configuration" />
  <div
    v-if="tab === 'parameters'"
    id="panel-parameters"
    role="tabpanel"
    aria-labelledby="tab-parameters"
  >
    <ParametersPanel @edit="editing = $event" />
  </div>
  <div
    v-else-if="tab === 'changes'"
    id="panel-changes"
    role="tabpanel"
    aria-labelledby="tab-changes"
  >
    <ChangesPanel @revert="reverting = $event" />
  </div>
  <div v-else id="panel-time" role="tabpanel" aria-labelledby="tab-time">
    <TimePanel />
  </div>

  <ConfigEditDialog v-if="editing !== null" :name="editing" @close="editing = null" />
  <ConfigEditDialog
    v-if="reverting"
    :name="reverting.name"
    :revert="reverting"
    :title="`Revert change ${reverting.changeId}`"
    @close="reverting = null"
  />
  <RestartPendingDialog v-if="restarting" @close="restarting = false" />
</template>

<style scoped>
.status {
  margin-bottom: var(--s-3);
}
</style>
