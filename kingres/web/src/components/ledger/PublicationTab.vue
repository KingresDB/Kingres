<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Publication tab of the Ledger page (contract 0.11.0, design 4.2, 4.6, 4.9, 8.5): whether the logs are published, the
// listener (and a warning for plain HTTP), the origin prefix (locked once a key exists), the public URL, the interval, the
// logs with their size and last checkpoint, the addresses to give to auditors, and what the publication reveals, said
// before the operator turns it on. The state of the logs comes from an operation that the server may not build yet: then
// the tab says so and the settings still work.
import { computed, reactive, ref, watch } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '@/components/QueryBoundary.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import GateButton from '@/components/GateButton.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import FormField from '@/components/FormField.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useLedgerPublication, useLedgerSettings } from '@/api/queries'
import { formatAgo, formatNumber } from '@/lib/format'
import {
  PUBLICATION_EXPOSES,
  PUBLICATION_GRANULARITY,
  PUBLICATION_HIDES,
  publicationMeta,
  VERIFIER_TEXT,
} from '@/lib/ledger-signing'
import type { LedgerSettingsUpdate } from '@/api/types'

const client = useQueryClient()
const settings = useLedgerSettings()
const publication = useLedgerPublication()
const s = computed(() => settings.data.value)
const pub = computed(() => publication.data.value)

const form = reactive({ prefix: '', url: '', interval: '' })
const errors = ref<Record<string, string>>({})
// The form follows the settings until the operator has typed something (a refetch does not overwrite the typing).
const dirty = ref(false)
watch(
  s,
  (v) => {
    if (v && !dirty.value) {
      form.prefix = v.publication.originPrefix
      form.url = v.publication.publicUrl ?? ''
      form.interval = String(v.publication.intervalSeconds)
    }
  },
  { immediate: true },
)

const save = useMutation({
  mutationFn: (body: LedgerSettingsUpdate) => endpoints.updateLedgerSettings(body),
  onSuccess: async () => {
    dirty.value = false
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.ledgerSettings }),
      client.invalidateQueries({ queryKey: keys.ledgerPublication }),
      client.invalidateQueries({ queryKey: keys.ledgerStatus }),
    ])
  },
})
const serverErrors = computed(() =>
  save.error.value ? describeProblem(save.error.value).fieldErrors : {},
)
const errorFor = (field: string): string | undefined =>
  errors.value[field] ?? serverErrors.value[field]

const locked = computed(() => s.value?.publication.originPrefixLocked === true)

function submit(): void {
  const e: Record<string, string> = {}
  const n = Number(form.interval)
  if (!Number.isInteger(n) || n < 10 || n > 3600)
    e['publication.intervalSeconds'] = 'Enter a whole number of seconds from 10 to 3600.'
  const p = form.prefix
  if (!locked.value && (p.length < 1 || p.length > 160 || /:\/\/|\/$|\s/.test(p)))
    e['publication.originPrefix'] =
      'Use a name without a scheme, without a trailing slash and without spaces, at most 160 characters.'
  if (form.url.trim() !== '' && !/^https?:\/\/[^\s/]+/.test(form.url.trim()))
    e['publication.publicUrl'] = 'Give an address that starts with http:// or https://.'
  errors.value = e
  if (Object.keys(e).length > 0 || !s.value) return
  const cur = s.value.publication
  const change: NonNullable<LedgerSettingsUpdate['publication']> = {}
  if (!locked.value && p !== cur.originPrefix) change.originPrefix = p
  const url = form.url.trim() === '' ? null : form.url.trim()
  if (url !== cur.publicUrl) change.publicUrl = url
  if (n !== cur.intervalSeconds) change.intervalSeconds = n
  if (Object.keys(change).length === 0) {
    dirty.value = false
    return
  }
  save.mutate({ publication: change })
}

const confirming = ref<'on' | 'off' | null>(null)
const toggle = useMutation({
  mutationFn: (enabled: boolean) => endpoints.updateLedgerSettings({ publication: { enabled } }),
  onSuccess: async () => {
    confirming.value = null
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.ledgerSettings }),
      client.invalidateQueries({ queryKey: keys.ledgerPublication }),
      client.invalidateQueries({ queryKey: keys.ledgerStatus }),
    ])
  },
})

/** The address of a log for an auditor: the public URL followed by what comes after the origin prefix. */
function logUrl(origin: string): string | null {
  const base = s.value?.publication.publicUrl
  const prefix = s.value?.publication.originPrefix
  if (!base || !prefix || !origin.startsWith(`${prefix}/`)) return null
  return `${base.replace(/\/$/, '')}${origin.slice(prefix.length)}`
}
const clusterUrl = computed(() => s.value?.publication.publicUrl?.replace(/\/$/, '') ?? null)

const copied = ref<string | null>(null)
async function copy(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = text
  } catch {
    copied.value = null
  }
}
const KIND: Record<string, string> = { chain: 'Chain log', aggregate: 'Aggregate log' }
</script>

<template>
  <div class="stack-lg">
    <section class="card stack" aria-labelledby="pubset-h">
      <div class="row-between">
        <h2 id="pubset-h">Publication</h2>
        <GateButton
          v-if="s && !s.publication.enabled"
          op="updateLedgerSettings"
          variant="primary"
          :disabled="toggle.isPending.value"
          @click="confirming = 'on'"
          >Turn publication on</GateButton
        >
        <GateButton
          v-else-if="s"
          op="updateLedgerSettings"
          :disabled="toggle.isPending.value"
          @click="confirming = 'off'"
          >Turn publication off</GateButton
        >
      </div>
      <p class="muted">
        When publication is on, the logs of the ledger are served over HTTP as signed checkpoints
        and tiles, so that anyone can check them with <code>kingres-verify</code>.
      </p>
      <QueryBoundary
        :loading="settings.isPending.value"
        :error="settings.error.value"
        @retry="settings.refetch()"
      >
        <template v-if="s">
          <p class="row" data-testid="publication-enabled">
            <StatusBadge
              :meta="
                s.publication.enabled
                  ? { tone: 'ok', icon: '✓', label: 'Publication is on' }
                  : { tone: 'neutral', icon: '○', label: 'Publication is off' }
              "
            />
          </p>
          <form class="stack" novalidate @submit.prevent="submit">
            <FormField
              v-slot="slot"
              label="Origin prefix"
              :hint="
                locked
                  ? 'Locked: a signing key exists, and every signature is made under this name. It cannot be changed; another prefix needs a new cluster generation.'
                  : 'The name under which every signature is made (no scheme, no trailing slash). It is locked when the first signing key is created. The value shown is a proposal until then.'
              "
              :error="errorFor('publication.originPrefix')"
            >
              <input
                v-bind="slot"
                v-model="form.prefix"
                class="input mono"
                autocomplete="off"
                spellcheck="false"
                :readonly="locked"
                @input="dirty = true"
              />
            </FormField>
            <p v-if="locked" class="muted" data-testid="prefix-locked">
              The origin prefix is locked.
            </p>
            <FormField
              v-slot="slot"
              label="Public URL"
              hint="Where the publication is reachable from outside; the addresses below are built from it. Leave it empty if it is not reachable yet."
              :error="errorFor('publication.publicUrl')"
            >
              <input
                v-bind="slot"
                v-model="form.url"
                class="input mono"
                autocomplete="off"
                spellcheck="false"
                @input="dirty = true"
              />
            </FormField>
            <FormField
              v-slot="slot"
              label="Checkpoint interval in seconds"
              hint="From 10 to 3600. It is also the granularity of the timing information that the publication gives."
              :error="errorFor('publication.intervalSeconds')"
            >
              <input
                v-bind="slot"
                v-model="form.interval"
                class="input"
                inputmode="numeric"
                autocomplete="off"
                @input="dirty = true"
              />
            </FormField>
            <ProblemNotice v-if="save.error.value" :error="save.error.value" />
            <div>
              <GateButton
                op="updateLedgerSettings"
                type="submit"
                variant="primary"
                :disabled="save.isPending.value"
                >Save the settings</GateButton
              >
            </div>
          </form>
        </template>
      </QueryBoundary>
      <ProblemNotice v-if="toggle.error.value && !confirming" :error="toggle.error.value" />
    </section>

    <section class="card stack" aria-labelledby="publisten-h">
      <h2 id="publisten-h">Listener and logs</h2>
      <QueryBoundary
        :loading="publication.isPending.value"
        :error="publication.error.value"
        @retry="publication.refetch()"
      >
        <template v-if="pub">
          <dl class="facts" data-testid="listener">
            <div>
              <dt>Listener</dt>
              <dd class="mono wrap">
                <SafeName
                  v-if="pub.listener.address"
                  :value="pub.listener.address"
                  kind="text"
                  :max="120"
                />
                <span v-else class="muted">none configured</span>
              </dd>
            </div>
            <div>
              <dt>State</dt>
              <dd>
                <StatusBadge
                  :meta="
                    pub.listener.running
                      ? { tone: 'ok', icon: '●', label: 'Running' }
                      : { tone: 'neutral', icon: '○', label: 'Not running' }
                  "
                />
              </dd>
            </div>
            <div>
              <dt>Last checkpoint</dt>
              <dd>
                <template v-if="pub.lastCheckpointAt">{{
                  formatAgo(pub.lastCheckpointAt)
                }}</template>
                <span v-else class="muted">none yet</span>
              </dd>
            </div>
          </dl>
          <p
            v-if="pub.listener.address && !pub.listener.tls"
            class="notice tone-warn"
            role="status"
            data-testid="plain-http"
          >
            <span aria-hidden="true">&#9650;</span>
            <span
              >The listener serves plain HTTP. The checkpoints are signed, so they cannot be forged,
              but anyone on the path can see who reads the publication and can hide a log from a
              reader. Put TLS in front of it before you give the addresses to auditors.</span
            >
          </p>
          <p
            v-if="pub.enabled && !pub.listener.address"
            class="notice tone-warn"
            role="status"
            data-testid="no-listener"
          >
            <span aria-hidden="true">&#9650;</span>
            <span
              >Publication is on but no listener is configured, so nothing is served. The signed
              checkpoints are still kept.</span
            >
          </p>
          <QueryBoundary
            :loading="false"
            :error="null"
            :empty="pub.logs.length === 0"
            :empty-title="pub.enabled ? 'No logs yet' : 'Nothing is published'"
            :empty-hint="
              pub.enabled
                ? 'A log appears when a database with ledger tables has a chain.'
                : 'Turn publication on to publish the logs.'
            "
          >
            <TableWrap>
              <table class="table">
                <caption class="sr-only">
                  Published logs
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Log</th>
                    <th scope="col">Kind</th>
                    <th scope="col" class="num">Size</th>
                    <th scope="col">Last checkpoint</th>
                    <th scope="col">State</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="l in pub.logs" :key="l.origin" data-testid="log-row">
                    <th scope="row" class="mono wrap">
                      <SafeName :value="l.origin" kind="text" :max="120" />
                    </th>
                    <td><SafeName :value="KIND[l.kind] ?? l.kind" kind="text" :max="40" /></td>
                    <td class="num">{{ formatNumber(l.size) }}</td>
                    <td>
                      <template v-if="l.lastCheckpointAt">{{
                        formatAgo(l.lastCheckpointAt)
                      }}</template>
                      <span v-else class="muted">none</span>
                    </td>
                    <td><StatusBadge :meta="publicationMeta(l.state)" /></td>
                  </tr>
                </tbody>
              </table>
            </TableWrap>
          </QueryBoundary>
        </template>
      </QueryBoundary>
    </section>

    <section class="card stack" aria-labelledby="pubaddr-h">
      <h2 id="pubaddr-h">Addresses for auditors</h2>
      <p v-if="!clusterUrl" class="muted" data-testid="no-public-url">
        Set the public URL above to see the addresses of the logs.
      </p>
      <template v-else>
        <p class="muted">
          The keyring and the signed events are under the address of the cluster; each log serves
          its
          <code>checkpoint</code> and its <code>tile</code> files under its own address.
        </p>
        <ul class="plain" data-testid="addresses">
          <li>
            Cluster:
            <code class="mono wrap"><SafeName :value="clusterUrl" kind="text" :max="200" /></code>
            <button type="button" class="btn btn-small" @click="copy(clusterUrl)">Copy</button>
            <span v-if="copied === clusterUrl" role="status">Copied.</span>
          </li>
          <template v-if="pub">
            <li v-for="l in pub.logs.filter((x) => x.state !== 'closed')" :key="l.origin">
              <template v-if="logUrl(l.origin)">
                {{ KIND[l.kind] ?? 'Log' }}:
                <code class="mono wrap"
                  ><SafeName :value="logUrl(l.origin) ?? ''" kind="text" :max="300"
                /></code>
                <button type="button" class="btn btn-small" @click="copy(logUrl(l.origin) ?? '')">
                  Copy
                </button>
                <span v-if="copied === logUrl(l.origin)" role="status">Copied.</span>
              </template>
            </li>
          </template>
        </ul>
      </template>
      <p class="muted">{{ VERIFIER_TEXT }}</p>
    </section>

    <section class="card stack" aria-labelledby="pubexp-h">
      <h2 id="pubexp-h">What the publication reveals</h2>
      <p>The publication contains hashes only. A third party who reads it learns:</p>
      <ul data-testid="exposes">
        <li v-for="t in PUBLICATION_EXPOSES" :key="t">{{ t }}</li>
      </ul>
      <p>{{ PUBLICATION_HIDES }}</p>
      <p class="muted">{{ PUBLICATION_GRANULARITY }}</p>
    </section>

    <ConfirmDialog
      v-if="confirming === 'on'"
      title="Turn publication on"
      confirm-label="Turn publication on"
      :busy="toggle.isPending.value"
      @cancel="confirming = null"
      @confirm="toggle.mutate(true)"
    >
      <p>From now on the logs of the ledger are published. Read what this reveals first:</p>
      <ul>
        <li v-for="t in PUBLICATION_EXPOSES" :key="t">{{ t }}</li>
      </ul>
      <p>{{ PUBLICATION_HIDES }}</p>
      <p class="muted">{{ PUBLICATION_GRANULARITY }}</p>
      <ProblemNotice v-if="toggle.error.value" :error="toggle.error.value" />
    </ConfirmDialog>
    <ConfirmDialog
      v-if="confirming === 'off'"
      title="Turn publication off"
      confirm-label="Turn publication off"
      :busy="toggle.isPending.value"
      @cancel="confirming = null"
      @confirm="toggle.mutate(false)"
    >
      <p>
        No new checkpoint is signed or published while publication is off. What was published stays
        valid, and nothing is deleted. Without new checkpoints the ledger cannot be anchored.
      </p>
      <ProblemNotice v-if="toggle.error.value" :error="toggle.error.value" />
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.facts {
  display: grid;
  gap: var(--s-3);
  grid-template-columns: repeat(auto-fit, minmax(14em, 1fr));
  margin: 0;
}
.facts dt {
  font-weight: 600;
}
.facts dd {
  margin: 0;
}
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
</style>
