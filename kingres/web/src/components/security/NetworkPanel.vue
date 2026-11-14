<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Network access rules (the pg_hba.conf rules, applied on every server). The list is replaced as
// a whole with optimistic concurrency (`revision`); applying it on all servers is a task.
import { computed, ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import ProblemNotice from '../ProblemNotice.vue'
import TaskProgress from '../TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import type { NetworkAccessRule } from '@/api/types'

const access = useQuery({ queryKey: keys.networkAccess, queryFn: endpoints.getNetworkAccess })

interface EditableRule extends Omit<NetworkAccessRule, 'databases' | 'users' | 'comment'> {
  databases: string
  users: string
  comment: string
}
const rules = ref<EditableRule[]>([])
const tlsRequired = ref(false)
const dirty = ref(false)

function load(): void {
  const d = access.data.value
  if (!d) return
  tlsRequired.value = d.tlsRequired
  rules.value = d.rules.map((r) => ({
    ...r,
    databases: r.databases.join(', '),
    users: r.users.join(', '),
    comment: r.comment ?? '',
  }))
  dirty.value = false
}
// Take over server data until the user starts editing; later refreshes must not overwrite edits.
watch(
  () => access.data.value,
  () => {
    if (!dirty.value) load()
  },
  { immediate: true },
)

const list = (s: string) =>
  s
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
const save = useTaskAction(
  () => {
    const d = access.data.value
    if (!d) throw new Error('Network access rules are not loaded.')
    return endpoints.replaceNetworkAccess({
      revision: d.revision,
      tlsRequired: tlsRequired.value,
      rules: rules.value.map((r) => ({
        ...r,
        databases: list(r.databases),
        users: list(r.users),
        comment: r.comment || null,
      })),
    })
  },
  {
    invalidate: () => [keys.networkAccess, keys.audit],
    onDone: () => {
      dirty.value = false
    },
  },
)

function addRule(): void {
  dirty.value = true
  rules.value.push({
    type: tlsRequired.value ? 'hostssl' : 'host',
    databases: 'all',
    users: 'all',
    address: '',
    method: 'scram-sha-256',
    comment: '',
  })
}
function removeRule(i: number): void {
  dirty.value = true
  rules.value.splice(i, 1)
}
const conflict = computed(
  () => (save.error.value as { code?: string } | null)?.code === 'optimistic_lock_failed',
)
function reload(): void {
  save.reset()
  dirty.value = false
  void access.refetch().then(load)
}
</script>

<template>
  <QueryBoundary
    :loading="access.isPending.value"
    :error="access.error.value"
    @retry="access.refetch()"
  >
    <form v-if="access.data.value" class="stack" @submit.prevent="save.run()" @input="dirty = true">
      <p class="muted">
        Revision {{ access.data.value.revision }}. Rules are checked from top to bottom; the first
        match wins.
      </p>
      <label class="check"
        ><input v-model="tlsRequired" type="checkbox" /> Require TLS (plain <code>host</code> rules
        are rejected)</label
      >

      <div class="table-wrap">
        <table class="table">
          <caption class="sr-only">
            Network access rules
          </caption>
          <thead>
            <tr>
              <th scope="col">Type</th>
              <th scope="col">Databases</th>
              <th scope="col">Users</th>
              <th scope="col">Address</th>
              <th scope="col">Method</th>
              <th scope="col">Comment</th>
              <th scope="col"><span class="sr-only">Remove</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(r, i) in rules" :key="i">
              <td>
                <select v-model="r.type" :aria-label="`Rule ${i + 1} type`">
                  <option value="host">host</option>
                  <option value="hostssl">hostssl</option>
                  <option value="hostnossl">hostnossl</option>
                </select>
              </td>
              <td>
                <input
                  v-model="r.databases"
                  class="input mono"
                  :aria-label="`Rule ${i + 1} databases`"
                />
              </td>
              <td>
                <input v-model="r.users" class="input mono" :aria-label="`Rule ${i + 1} users`" />
              </td>
              <td>
                <input
                  v-model="r.address"
                  class="input mono"
                  :aria-label="`Rule ${i + 1} address`"
                  placeholder="10.0.0.0/24"
                />
              </td>
              <td>
                <select v-model="r.method" :aria-label="`Rule ${i + 1} method`">
                  <option value="scram-sha-256">scram-sha-256</option>
                  <option value="cert">cert (client certificate)</option>
                  <option value="reject">reject</option>
                </select>
              </td>
              <td>
                <input v-model="r.comment" class="input" :aria-label="`Rule ${i + 1} comment`" />
              </td>
              <td>
                <button
                  type="button"
                  class="btn btn-small"
                  :aria-label="`Remove rule ${i + 1}`"
                  @click="removeRule(i)"
                >
                  Remove
                </button>
              </td>
            </tr>
            <tr v-if="rules.length === 0">
              <td colspan="7" class="muted">No rules. Nobody can connect over the network.</td>
            </tr>
          </tbody>
        </table>
      </div>

      <ProblemNotice v-if="save.error.value" :error="save.error.value" />
      <p v-if="conflict">
        <button type="button" class="btn" @click="reload">Discard my edits and reload</button>
      </p>
      <TaskProgress :task="save.task.value" :poll-error="save.pollError.value" />

      <div class="row">
        <button type="button" class="btn" @click="addRule">Add rule</button>
        <GateButton
          op="replaceNetworkAccess"
          type="submit"
          variant="primary"
          :disabled="save.isBusy.value"
          >Apply on all servers</GateButton
        >
        <button v-if="dirty" type="button" class="btn" @click="load">Discard changes</button>
      </div>

      <section aria-labelledby="apply-h">
        <h3 id="apply-h">Applied revision per server</h3>
        <div class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col" class="num">Revision</th>
                <th scope="col">Result</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in access.data.value.applyStatus" :key="s.serverId">
                <th scope="row">Server {{ s.serverId }}</th>
                <td class="num">{{ s.appliedRevision }}</td>
                <td>
                  <span v-if="s.error" class="badge tone-danger"
                    ><span aria-hidden="true">&#10006;</span> {{ s.error }}</span
                  >
                  <span
                    v-else-if="s.appliedRevision === access.data.value.revision"
                    class="badge tone-ok"
                    ><span aria-hidden="true">&#10003;</span> Applied</span
                  >
                  <span v-else class="badge tone-info"
                    ><span aria-hidden="true">&#9680;</span> Pending</span
                  >
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </form>
  </QueryBoundary>
</template>
