<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The audit of the publication (contract 0.11.0, design 8.5): a verification of one database with the check `publication`, which
// reads every chain again from block 0 and compares it with the published leaves. Also the bundle for offline verification, which
// is about the same published logs. The database is chosen here because the Publication tab is about the cluster.
import { computed, ref, watch } from 'vue'
import GateButton from '@/components/GateButton.vue'
import SafeName from '@/components/SafeName.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import BundleDialog from '@/components/ledger/BundleDialog.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useLedgerStatusList } from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import { safeText } from '@/lib/safe-text'
import { AUDIT_TEXT, verifyBody } from '@/lib/ledger-ops'

const statusList = useLedgerStatusList()
const databases = computed(() => statusList.data.value?.items.map((s) => s.databaseName) ?? [])
const db = ref('')
watch(
  databases,
  (d) => {
    if (!d.includes(db.value)) db.value = d[0] ?? ''
  },
  { immediate: true },
)
const bundling = ref(false)
const audit = useTaskAction(
  () => endpoints.startLedgerVerification(db.value, verifyBody('full', ['publication'])),
  { invalidate: () => [keys.ledger(db.value), keys.ledgerStatus, keys.ledgerPublication] },
)
</script>

<template>
  <section class="card stack" aria-labelledby="pubaudit-h" data-testid="publication-audit">
    <h2 id="pubaudit-h">Audit and bundle</h2>
    <p v-if="databases.length === 0" class="muted" data-testid="audit-no-database">
      No database uses the ledger, so there is nothing to audit or to bundle.
    </p>
    <template v-else>
      <p class="muted" data-testid="audit-text">{{ AUDIT_TEXT }}</p>
      <div class="form-row">
        <div class="field">
          <label for="pa-db">Database</label>
          <select id="pa-db" v-model="db">
            <option v-for="d in databases" :key="d" :value="d">{{ safeText(d) }}</option>
          </select>
        </div>
        <GateButton
          op="startLedgerVerification"
          :disabled="audit.isBusy.value || !db"
          @click="audit.run()"
          >Audit the publication now</GateButton
        >
        <GateButton op="createLedgerBundle" :disabled="!db" @click="bundling = true"
          >Create a bundle</GateButton
        >
      </div>
      <LedgerRefusal v-if="audit.error.value" :error="audit.error.value" ops="verify" />
      <TaskProgress :task="audit.task.value" :poll-error="audit.pollError.value" />
      <p class="muted">
        The result is a verification record of <SafeName :value="db" :max="60" /> on the Overview
        tab.
      </p>
    </template>
    <BundleDialog v-if="bundling" :database-name="db" @close="bundling = false" />
  </section>
</template>
