<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The settings of this server alone (getThisServerSettings / updateThisServerSettings). Only what
// was changed is sent. A saved memory quota or log directory applies when the instance restarts:
// the server keeps saying so (`restartRequired`) until then. The data directory and the client
// ports cannot be changed after the server was created.
import { computed, reactive, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import { vBlockEnterSubmit } from '@/lib/forms'
import { describeProblem } from '@/api/problem'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import type { Instance, LogLevel } from '@/api/types'
import type { components } from '@/api/schema'

type S = components['schemas']

const props = defineProps<{ instances: Instance[] }>()
const emit = defineEmits<{ restart: [instance: Instance] }>()

const client = useQueryClient()
const local = useQuery({
  queryKey: [...keys.thisServer, 'settings'],
  queryFn: endpoints.getThisServerSettings,
})

const form = reactive({ memory: 4096, logLevel: 'info' as LogLevel, logDirectory: '' })
watch(
  () => local.data.value,
  (d) => {
    if (d) {
      form.memory = d.memoryQuota.totalMegabytes
      form.logLevel = d.logLevel
      form.logDirectory = d.dataPaths.logDirectory ?? ''
    }
  },
  { immediate: true },
)
const memoryError = computed(() =>
  form.memory >= 512 && form.memory <= 16_777_216
    ? undefined
    : 'The memory quota must be between 512 and 16777216 MB.',
)
const directoryError = computed(() =>
  form.logDirectory === '' || form.logDirectory.startsWith('/')
    ? undefined
    : 'Use an absolute path that starts with /, or leave the field empty for the system default.',
)
/** Only what was changed; an untouched field is not sent back. */
function changes(): S['LocalSettingsUpdate'] {
  const d = local.data.value
  const body: S['LocalSettingsUpdate'] = {}
  if (!d || form.memory !== d.memoryQuota.totalMegabytes)
    body.memoryQuota = { totalMegabytes: form.memory }
  if (!d || form.logLevel !== d.logLevel) body.logLevel = form.logLevel
  if (form.logDirectory !== (d?.dataPaths.logDirectory ?? ''))
    body.logDirectory = form.logDirectory === '' ? null : form.logDirectory
  return body
}
const save = useMutation({
  mutationFn: () => endpoints.updateThisServerSettings(changes()),
  onSuccess: () => client.invalidateQueries({ queryKey: keys.thisServer }),
})
/** Server messages for single fields (JSON pointers of the request body). */
const serverErrors = computed<Record<string, string>>(() =>
  save.error.value ? describeProblem(save.error.value).fieldErrors : {},
)
const nothingChanged = computed(() => !!local.data.value && Object.keys(changes()).length === 0)
const running = computed(() => props.instances.filter((i) => i.state === 'running'))
</script>

<template>
  <section class="card stack" aria-labelledby="set-h">
    <h2 id="set-h">Local settings</h2>
    <QueryBoundary :loading="local.isPending.value" :error="local.error.value">
      <form
        v-if="local.data.value"
        v-block-enter-submit
        class="stack"
        style="max-width: 36em"
        @submit.prevent="save.mutate()"
      >
        <div v-if="local.data.value.restartRequired" class="notice tone-warn" role="status">
          <span aria-hidden="true">&#9650;</span>
          <div class="stack" style="gap: var(--s-1)">
            <strong>A restart is required</strong>
            <span
              >A saved change (the memory quota or the log directory) takes effect when the instance
              is restarted. Until then the old values are in use. The log level applies at once and
              never needs a restart.</span
            >
            <div class="row">
              <GateButton
                v-for="i in running"
                :key="i.id"
                op="restartThisServerInstance"
                small
                @click="emit('restart', i)"
                >Restart instance #{{ i.id }}</GateButton
              >
            </div>
          </div>
        </div>
        <dl class="kv">
          <dt>Data directory</dt>
          <dd class="mono">
            <SafeName :value="local.data.value.dataPaths.dataDirectory" kind="text" :max="200" />
          </dd>
          <dt>WAL directory</dt>
          <dd data-testid="wal-directory">
            <SafeName
              v-if="local.data.value.dataPaths.walDirectory"
              class="mono"
              :value="local.data.value.dataPaths.walDirectory"
              kind="text"
              :max="200"
            />
            <span v-else>default (inside the data directory)</span>
          </dd>
          <dt>Log directory in use</dt>
          <dd data-testid="log-directory">
            <SafeName
              v-if="local.data.value.dataPaths.logDirectory"
              class="mono"
              :value="local.data.value.dataPaths.logDirectory"
              kind="text"
              :max="200"
            />
            <span v-else>default (the system default)</span>
          </dd>
          <dt>Client ports</dt>
          <dd>
            read-write {{ local.data.value.clientPorts.readWritePort }}, read-only
            <NullableValue :value="local.data.value.clientPorts.readOnlyPort ?? null" />
          </dd>
        </dl>
        <p class="muted">
          The data directory and the client ports cannot be changed after the server was created.
        </p>
        <div class="field">
          <label for="ls-mem">Memory quota (MB)</label
          ><input
            id="ls-mem"
            v-model.number="form.memory"
            class="input num"
            type="number"
            min="512"
            max="16777216"
            :aria-invalid="
              memoryError || serverErrors['/memoryQuota/totalMegabytes'] ? 'true' : undefined
            "
          /><span v-if="memoryError" class="error" role="alert">{{ memoryError }}</span
          ><span v-if="serverErrors['/memoryQuota/totalMegabytes']" class="error" role="alert"
            ><SafeName :value="serverErrors['/memoryQuota/totalMegabytes']" kind="text" :max="300"
          /></span>
          <span class="hint muted">Applies after the instance is restarted.</span>
        </div>
        <div class="field">
          <label for="ls-level">Log level</label
          ><select id="ls-level" v-model="form.logLevel">
            <option value="debug">debug</option>
            <option value="info">info</option>
            <option value="warning">warning</option>
            <option value="error">error</option>
          </select>
          <span class="hint muted">Applies at once.</span>
        </div>
        <div class="field">
          <label for="ls-logdir">Log directory</label
          ><input
            id="ls-logdir"
            v-model="form.logDirectory"
            class="input mono"
            type="text"
            autocomplete="off"
            spellcheck="false"
            placeholder="System default"
            :aria-invalid="directoryError || serverErrors['/logDirectory'] ? 'true' : undefined"
          /><span v-if="directoryError" class="error" role="alert">{{ directoryError }}</span
          ><span v-if="serverErrors['/logDirectory']" class="error" role="alert"
            ><SafeName :value="serverErrors['/logDirectory']" kind="text" :max="300"
          /></span>
          <span class="hint muted"
            >An absolute path of an existing directory that the server can write to. Applies after
            the instance is restarted.</span
          >
        </div>
        <ProblemNotice v-if="save.error.value" :error="save.error.value" />
        <p v-if="save.isSuccess.value && !save.error.value" class="muted" role="status">Saved.</p>
        <div>
          <GateButton
            op="updateThisServerSettings"
            type="submit"
            variant="primary"
            :disabled="!!memoryError || !!directoryError || save.isPending.value || nothingChanged"
            >Save local settings</GateButton
          >
        </div>
      </form>
    </QueryBoundary>
  </section>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dd {
  margin: 0;
}
</style>
