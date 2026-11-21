<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "Create database" with the four groups of settings from the architecture document:
// General, Sharding, Replication and Ledger. Every group says which of its settings can still be
// changed later and which are fixed at creation. Choices, defaults and ranges come from
// GET /database-create-options, so the server (not the panel) decides what is offered.
import { computed, reactive, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import { RouterLink } from 'vue-router'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import QueryBoundary from './QueryBoundary.vue'
import ShardingModeChoice from './ShardingModeChoice.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useClusterSettings, useDatabaseCreateOptions, useRoles } from '@/api/queries'
import type { DatabaseCreateRequest, Durability, LedgerMode } from '@/api/types'
import { DURABILITY_LABEL, LEDGER_MODE_LABEL } from '@/lib/status'
import {
  canDistributeNew,
  distributeNewDefault,
  hasBuckets,
  offeredModes,
  type ShardingMode,
} from '@/lib/sharding-mode'
import { compact, firstError, identifier, integerBetween } from '@/lib/validators'

const emit = defineEmits<{ close: []; done: [name: string] }>()

const options = useDatabaseCreateOptions()
const settings = useClusterSettings()
const roles = useRoles()
const o = computed(() => options.data.value)
const shardingAvailable = computed(() => settings.data.value?.features.sharding ?? true)
const ledgerAvailable = computed(() => settings.data.value?.features.ledger ?? true)

const form = reactive({
  name: '',
  owner: '',
  encoding: '',
  collation: '',
  connectionLimit: -1,
  sharding: false,
  shardMode: 'row' as ShardingMode,
  distributeNew: false,
  bucketCount: 128,
  durability: 'sync' as Durability,
  ledgerMode: 'off' as LedgerMode,
  digestInterval: 3600,
})
const submitted = ref(false)

// Take the defaults from the server once they are known.
watch(
  o,
  (opts) => {
    if (!opts) return
    form.encoding = opts.encodings.find((e) => e.isDefault)?.name ?? opts.encodings[0]?.name ?? ''
    form.bucketCount = opts.bucketCount.default
    form.durability = opts.defaultDurability
    const modes = offeredModes(opts.shardingModes?.modes)
    const wanted = opts.shardingModes?.default
    form.shardMode = modes.find((m) => m === wanted) ?? modes[0] ?? 'row'
    form.distributeNew = distributeNewDefault(form.shardMode)
    form.digestInterval = opts.ledgerDigestIntervalSeconds.default
  },
  { immediate: true },
)

/** Collations that go with the chosen encoding (`encodings: null` means every encoding). */
const collations = computed(() =>
  (o.value?.collations ?? []).filter(
    (c) => c.encodings === null || c.encodings.includes(form.encoding),
  ),
)
watch(
  [collations, () => form.encoding],
  () => {
    if (!collations.value.some((c) => c.name === form.collation)) {
      form.collation =
        collations.value.find((c) => c.isDefault)?.name ?? collations.value[0]?.name ?? ''
    }
  },
  { immediate: true },
)

/**
 * The modes the server offers (contract 0.15.0). A server that sends none is older: sharding is by rows only and the panel
 * shows no choice.
 */
const modes = computed(() => offeredModes(o.value?.shardingModes?.modes))
const bucketsShown = computed(() => hasBuckets(form.shardMode))

const errors = computed(() =>
  compact({
    name:
      form.name === '' && !submitted.value
        ? undefined
        : firstError(form.name, identifier('The name')),
    'general.connectionLimit': firstError<number | null>(
      form.connectionLimit,
      integerBetween('The connection limit', -1, 1_000_000),
    ),
    'sharding.bucketCount':
      form.sharding && bucketsShown.value
        ? firstError<number | null>(
            form.bucketCount,
            integerBetween(
              'The bucket count',
              o.value?.bucketCount.minimum ?? 1,
              o.value?.bucketCount.maximum ?? 4096,
            ),
          )
        : undefined,
    'ledger.digestIntervalSeconds':
      form.ledgerMode !== 'off'
        ? firstError<number | null>(
            form.digestInterval,
            integerBetween(
              'The digest interval',
              o.value?.ledgerDigestIntervalSeconds.minimum ?? 60,
              o.value?.ledgerDigestIntervalSeconds.maximum ?? 86400,
            ),
          )
        : undefined,
  }),
)

function validate(): boolean {
  submitted.value = true
  return Object.keys(errors.value).length === 0
}

/**
 * What the server is told about sharding. The mode is sent for Schemas and Both only (Rows is what the server does when
 * it hears none, so a request for Rows also works against a server before 0.15.0); the bucket count is not sent for
 * Schemas, which has no hash buckets; the default for new schemas is sent only where the mode has schemas.
 */
function shardingPayload(): NonNullable<DatabaseCreateRequest['sharding']> {
  if (!form.sharding) return { enabled: false, bucketCount: form.bucketCount }
  return {
    enabled: true,
    ...(bucketsShown.value ? { bucketCount: form.bucketCount } : {}),
    ...(form.shardMode !== 'row' ? { mode: form.shardMode } : {}),
    ...(canDistributeNew(form.shardMode) ? { distributeNewSchemas: form.distributeNew } : {}),
  }
}

function payload(): DatabaseCreateRequest {
  return {
    name: form.name,
    general: {
      ...(form.owner ? { owner: form.owner } : {}),
      encoding: form.encoding,
      ...(form.collation ? { lcCollate: form.collation, lcCtype: form.collation } : {}),
      connectionLimit: form.connectionLimit,
    },
    sharding: shardingPayload(),
    replication: { durability: form.durability },
    ledger: { mode: form.ledgerMode, digestIntervalSeconds: form.digestInterval },
  }
}

const start = () => endpoints.createDatabase(payload())
const errorFor = (field: string, server: Record<string, string>): string | undefined =>
  errors.value[field] ?? server[field]
</script>

<template>
  <TaskActionDialog
    title="Create database"
    confirm-label="Create database"
    wide
    :start="start"
    :validate="validate"
    :invalidate="[keys.databases]"
    @close="emit('close')"
    @done="emit('done', form.name)"
  >
    <template #default="{ fieldErrors }">
      <QueryBoundary
        :loading="options.isPending.value"
        :error="options.error.value"
        @retry="options.refetch()"
      >
        <fieldset>
          <legend>General</legend>
          <FormField
            v-slot="slot"
            label="Name"
            :error="errorFor('name', fieldErrors)"
            hint="Lower-case letters, digits and underscore. Cannot be changed later."
            required
          >
            <input
              v-bind="slot"
              v-model="form.name"
              class="input mono"
              autocomplete="off"
              spellcheck="false"
              data-autofocus
            />
          </FormField>
          <FormField v-slot="slot" label="Owner role" hint="Can be changed later. Empty means you.">
            <select v-if="roles.data.value" v-bind="slot" v-model="form.owner">
              <option value="">Me</option>
              <option
                v-for="r in roles.data.value.items.filter((x) => x.canLogin)"
                :key="r.name"
                :value="r.name"
              >
                {{ safeText(r.name) }}
              </option>
            </select>
            <input v-else v-bind="slot" v-model="form.owner" class="input mono" />
          </FormField>
          <div class="form-row">
            <FormField v-slot="slot" label="Encoding" hint="Fixed after creation.">
              <select v-bind="slot" v-model="form.encoding">
                <option v-for="e in o?.encodings" :key="e.name" :value="e.name">
                  {{ safeText(e.name) }}{{ e.isDefault ? ' (default)' : '' }}
                </option>
              </select>
            </FormField>
            <FormField v-slot="slot" label="Collation" hint="Fixed after creation.">
              <select v-bind="slot" v-model="form.collation">
                <option v-for="c in collations" :key="c.name" :value="c.name">
                  {{ safeText(c.name) }}{{ c.isDefault ? ' (default)' : '' }}
                </option>
              </select>
            </FormField>
          </div>
          <FormField
            v-slot="slot"
            label="Connection limit"
            hint="-1 means unlimited. Can be changed later."
            :error="errorFor('general.connectionLimit', fieldErrors)"
          >
            <input
              v-bind="slot"
              v-model.number="form.connectionLimit"
              class="input num"
              type="number"
              min="-1"
            />
          </FormField>
        </fieldset>

        <fieldset>
          <legend>Sharding</legend>
          <label class="check">
            <input v-model="form.sharding" type="checkbox" :disabled="!shardingAvailable" />
            Spread this database over the shard groups
          </label>
          <p v-if="!shardingAvailable" class="hint muted">
            Sharding is not enabled for this cluster. Enable it in
            <RouterLink :to="{ name: 'settings' }">cluster settings</RouterLink> first.
          </p>
          <p v-else class="hint muted">Sharding can be turned on later, but never off again.</p>
          <ShardingModeChoice
            v-if="form.sharding && modes.length > 0"
            v-model:mode="form.shardMode"
            v-model:distribute-new="form.distributeNew"
            :modes="modes"
          />
          <FormField
            v-if="bucketsShown"
            v-slot="slot"
            label="Bucket count"
            :hint="`Default ${o?.bucketCount.default}, from ${o?.bucketCount.minimum} to ${o?.bucketCount.maximum}. Fixed after creation, like the number of vBuckets in Couchbase.`"
            :error="errorFor('sharding.bucketCount', fieldErrors)"
          >
            <input
              v-bind="slot"
              v-model.number="form.bucketCount"
              class="input num"
              type="number"
              :min="o?.bucketCount.minimum"
              :max="o?.bucketCount.maximum"
              :disabled="!form.sharding"
            />
          </FormField>
        </fieldset>

        <fieldset>
          <legend>Replication</legend>
          <FormField
            v-slot="slot"
            label="Durability"
            hint="How a commit waits for standbys. Can be changed later."
          >
            <select v-bind="slot" v-model="form.durability">
              <option v-for="(label, value) in DURABILITY_LABEL" :key="value" :value="value">
                {{ label }}
              </option>
            </select>
          </FormField>
          <p>
            Replica count:
            <strong data-testid="replica-count">{{ o?.replicaCount ?? '-' }}</strong>
            <span class="muted"> (read-only here)</span>
          </p>
          <p class="hint muted">
            The replica count is a cluster setting. Change it in
            <RouterLink :to="{ name: 'settings' }">cluster settings</RouterLink>; a rebalance
            follows.
          </p>
        </fieldset>

        <fieldset>
          <legend>Ledger</legend>
          <FormField
            v-slot="slot"
            label="Ledger mode"
            hint="Can be turned on later. It cannot be turned off for a table that already has it."
          >
            <select v-bind="slot" v-model="form.ledgerMode" :disabled="!ledgerAvailable">
              <option v-for="(label, value) in LEDGER_MODE_LABEL" :key="value" :value="value">
                {{ label }}
              </option>
            </select>
          </FormField>
          <p v-if="!ledgerAvailable" class="hint muted">
            The ledger is not enabled for this cluster. Enable it in
            <RouterLink :to="{ name: 'settings' }">cluster settings</RouterLink> first.
          </p>
          <FormField
            v-slot="slot"
            label="Digest interval (seconds)"
            :hint="`${o?.ledgerDigestIntervalSeconds.minimum} to ${o?.ledgerDigestIntervalSeconds.maximum}. Can be changed later.`"
            :error="errorFor('ledger.digestIntervalSeconds', fieldErrors)"
          >
            <input
              v-bind="slot"
              v-model.number="form.digestInterval"
              class="input num"
              type="number"
              :min="o?.ledgerDigestIntervalSeconds.minimum"
              :max="o?.ledgerDigestIntervalSeconds.maximum"
              :disabled="form.ledgerMode === 'off'"
            />
          </FormField>
        </fieldset>
      </QueryBoundary>
    </template>
  </TaskActionDialog>
</template>
