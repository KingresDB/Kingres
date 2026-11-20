<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Client keys tab of the Ledger page (Console C, contract 0.11.0, design 3.6 to 3.8 and 8.5): for one database, the public keys
// that clients registered to sign the rows they write (Register, Revoke), the ledger tables with their signature policy and the
// control that requires signatures (it waits for the open writers of the table, which the dialog says before the click), and the
// upgrade of the ledger extension to version 0.3, which the keys and the policy need. Only public information is shown: a private
// key is never asked for, shown or stored. An operation that the server does not build yet answers `unsupported_operation` and is
// shown as what it is, in its own place; the rest of the tab goes on.
import { computed, ref } from 'vue'
import GateButton from '@/components/GateButton.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import LedgerRefusal from '@/components/ledger/LedgerRefusal.vue'
import RegisterClientKeyDialog from '@/components/ledger/RegisterClientKeyDialog.vue'
import RevokeClientKeyDialog from '@/components/ledger/RevokeClientKeyDialog.vue'
import SignaturePolicyDialog from '@/components/ledger/SignaturePolicyDialog.vue'
import UpgradeExtensionDialog from '@/components/ledger/UpgradeExtensionDialog.vue'
import { useLedgerClientKeys, useLedgerStatusList, useTables } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import { safeText } from '@/lib/safe-text'
import {
  ATTESTATION_LIMITS_TEXT,
  CLIENT_KEYS_TEXT,
  keyRole,
  keyState,
  POLICY_NOT_REPORTED,
  revocationText,
  shortKey,
  UPGRADE_TEXT,
} from '@/lib/ledger-client'
import type { LedgerClientKey, Table } from '@/api/types'

const props = defineProps<{ databaseName: string }>()
const emit = defineEmits<{ 'update:databaseName': [name: string] }>()
const statusList = useLedgerStatusList()
const dbName = computed(() => props.databaseName)
const databases = computed(() => statusList.data.value?.items.map((s) => s.databaseName) ?? [])
const dbModel = computed({
  get: () => props.databaseName,
  set: (v: string) => emit('update:databaseName', v),
})
const keysQuery = useLedgerClientKeys(dbName)
const tables = useTables(dbName)
const ledgerTables = computed<Table[]>(() =>
  (tables.data.value?.items ?? []).filter((t) => t.ledger !== null),
)

/** The tables that the server said (or that this session switched) require signatures; the contract carries no policy per table. */
const required = ref<Record<string, boolean>>({})
const tableId = (t: Table): string => `${props.databaseName}:${t.schema}.${t.name}`

const dialog = ref<
  | { kind: 'register' }
  | { kind: 'revoke'; key: LedgerClientKey }
  | { kind: 'policy'; table: Table }
  | { kind: 'upgrade' }
  | null
>(null)
const notice = ref<string | null>(null)
</script>

<template>
  <div class="stack-lg">
    <div v-if="statusList.isPending.value" class="skeleton" role="status">
      <span class="sr-only">Loading</span>
    </div>
    <section v-else-if="databases.length === 0" class="card stack" data-testid="client-no-database">
      <h2>Client keys</h2>
      <p class="muted">No database uses the ledger, so there are no client keys.</p>
    </section>
    <template v-else>
      <div class="field" style="max-width: 20em">
        <label for="ck-db">Database</label>
        <select id="ck-db" v-model="dbModel">
          <option v-for="d in databases" :key="d" :value="d">{{ safeText(d) }}</option>
        </select>
      </div>

      <section class="card stack" aria-labelledby="ck-keys-h" data-testid="client-keys">
        <div class="row-between">
          <h2 id="ck-keys-h">Client keys</h2>
          <div class="row">
            <GateButton
              op="upgradeLedgerExtension"
              :disabled="!databaseName"
              @click="dialog = { kind: 'upgrade' }"
              >Upgrade the extension</GateButton
            >
            <GateButton
              op="registerLedgerClientKey"
              variant="primary"
              :disabled="!databaseName"
              @click="dialog = { kind: 'register' }"
              >Register a key</GateButton
            >
          </div>
        </div>
        <p class="muted">{{ CLIENT_KEYS_TEXT }}</p>
        <p class="muted" data-testid="attestation-limits">{{ ATTESTATION_LIMITS_TEXT }}</p>
        <p v-if="notice" class="notice tone-ok" role="status" data-testid="client-notice">
          <span aria-hidden="true">&#10003;</span><span>{{ notice }}</span>
        </p>
        <LedgerRefusal
          v-if="keysQuery.error.value"
          :error="keysQuery.error.value"
          action="list"
          @retry="keysQuery.refetch()"
        />
        <QueryBoundary
          v-else
          :loading="keysQuery.isPending.value"
          :error="null"
          :empty="(keysQuery.data.value?.items.length ?? 0) === 0"
          empty-title="No client key is registered"
          empty-hint="Register the public key of an application to let it sign the rows it writes."
          @retry="keysQuery.refetch()"
        >
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Client keys
              </caption>
              <thead>
                <tr>
                  <th scope="col">Label</th>
                  <th scope="col">Key id</th>
                  <th scope="col">Public key</th>
                  <th scope="col">Role</th>
                  <th scope="col">Registered</th>
                  <th scope="col">State</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="k in keysQuery.data.value?.items"
                  :key="k.keyId"
                  :data-testid="`client-key-${k.keyId}`"
                >
                  <th scope="row"><SafeName :value="k.label" kind="text" :max="100" /></th>
                  <td>
                    <code class="mono">{{ k.keyId }}</code>
                  </td>
                  <td>
                    <code class="mono" :title="k.publicKey">{{ shortKey(k.publicKey) }}</code>
                  </td>
                  <td>{{ keyRole(k) }}</td>
                  <td>
                    {{ formatDateTime(k.registeredAt) }}
                    <div class="muted">
                      by <SafeName :value="k.registeredBy" kind="text" :max="60" />
                    </div>
                  </td>
                  <td data-testid="client-key-state">
                    <StatusBadge
                      v-if="keyState(k) === 'active'"
                      :meta="{ tone: 'ok', icon: '✓', label: 'Active' }"
                    />
                    <template v-else>
                      <StatusBadge :meta="{ tone: 'neutral', icon: '○', label: 'Revoked' }" />
                      <div class="muted">
                        <SafeName :value="revocationText(k)" kind="text" :max="260" />
                      </div>
                    </template>
                  </td>
                  <td>
                    <GateButton
                      v-if="keyState(k) === 'active'"
                      op="revokeLedgerClientKey"
                      small
                      variant="danger"
                      :aria-label="`Revoke key ${k.keyId}`"
                      @click="dialog = { kind: 'revoke', key: k }"
                      >Revoke</GateButton
                    >
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
      </section>

      <section class="card stack" aria-labelledby="ck-tables-h" data-testid="client-tables">
        <h2 id="ck-tables-h">Signature policy of the tables</h2>
        <p class="muted">
          With the policy "required" a table accepts only writes whose entries carry a client
          attestation. The policy of a table starts as "optional" and can only go one way.
        </p>
        <p class="muted" data-testid="policy-not-reported">{{ POLICY_NOT_REPORTED }}</p>
        <QueryBoundary
          :loading="tables.isPending.value"
          :error="tables.error.value"
          :empty="ledgerTables.length === 0"
          empty-title="No ledger table"
          empty-hint="Only a ledger table has a signature policy."
          @retry="tables.refetch()"
        >
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Ledger tables and their signature policy
              </caption>
              <thead>
                <tr>
                  <th scope="col">Table</th>
                  <th scope="col">Ledger</th>
                  <th scope="col">Signature policy</th>
                  <th scope="col"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="t in ledgerTables"
                  :key="`${t.schema}.${t.name}`"
                  :data-testid="`ledger-table-${t.schema}.${t.name}`"
                >
                  <th scope="row">
                    <code class="mono"
                      ><SafeName :value="`${t.schema}.${t.name}`" kind="name" :max="120"
                    /></code>
                  </th>
                  <td>{{ t.ledger?.mode === 'updatable' ? 'Updatable' : 'Append only' }}</td>
                  <td data-testid="table-policy">
                    <StatusBadge
                      v-if="required[tableId(t)]"
                      :meta="{ tone: 'ok', icon: '✓', label: 'Required' }"
                    />
                    <span v-else class="muted">Not reported</span>
                  </td>
                  <td>
                    <GateButton
                      op="setTableSignaturePolicy"
                      small
                      :blocked-reason="
                        required[tableId(t)]
                          ? 'The policy is required already and cannot go back.'
                          : ''
                      "
                      :aria-label="`Require signatures for ${t.schema}.${t.name}`"
                      @click="dialog = { kind: 'policy', table: t }"
                      >Require signatures...</GateButton
                    >
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
      </section>

      <section class="card stack" aria-labelledby="ck-up-h">
        <h2 id="ck-up-h">Ledger extension</h2>
        <p class="muted">{{ UPGRADE_TEXT }}</p>
      </section>
    </template>

    <RegisterClientKeyDialog
      v-if="dialog?.kind === 'register'"
      :database-name="databaseName"
      @close="dialog = null"
      @saved="(id) => (notice = `The key ${id} was registered.`)"
    />
    <RevokeClientKeyDialog
      v-if="dialog?.kind === 'revoke'"
      :database-name="databaseName"
      :client-key="dialog.key"
      @close="dialog = null"
    />
    <SignaturePolicyDialog
      v-if="dialog?.kind === 'policy'"
      :database-name="databaseName"
      :table="dialog.table"
      @close="dialog = null"
      @required="required[tableId((dialog as { table: Table }).table)] = true"
    />
    <UpgradeExtensionDialog
      v-if="dialog?.kind === 'upgrade'"
      :database-name="databaseName"
      @close="dialog = null"
    />
  </div>
</template>
