<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The extensions `kingres_columnar` and `pg_stat_statements`: whether each one is preloaded on every server. That is all the
// contract offers about them. The preloaded libraries are the value of `shared_preload_libraries` (a managed setting, read from
// `getConfigParameter`); the contract has no operation that lists the extensions installed in a database or installs one, and
// the install state of a database is not shown here because nothing reports it. Columnar tables are shown on the pages of the
// databases and tables (storage format, conversion), which this panel links to.
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { RouterLink } from 'vue-router'
import QueryBoundary from '../QueryBoundary.vue'
import SafeName from '../SafeName.vue'
import TableWrap from '../TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'

const EXTENSIONS = [
  {
    library: 'kingres_columnar',
    title: 'Columnar storage',
    text: 'Stores a table by column, compressed, with stripe skipping. A table is converted on its page (Convert storage), and the storage format is shown on the pages of the databases and the tables.',
  },
  {
    library: 'pg_stat_statements',
    title: 'Statement statistics',
    text: 'Counts the statements that run (calls, time, rows). It needs shared memory that only the postmaster can set up, so it works only when it is preloaded.',
  },
] as const

const parameter = useQuery({
  queryKey: [...keys.config, 'parameter', 'shared_preload_libraries'],
  queryFn: () => endpoints.getConfigParameter('shared_preload_libraries'),
})
const servers = computed(() => parameter.data.value?.servers ?? [])

/** The libraries of a value such as `kingres_shard,kingres_columnar`; the entries may carry quotes and spaces. */
function libraries(value: string | null): string[] | null {
  if (value === null) return null
  return value
    .split(',')
    .map((x) => x.trim().replace(/^"(.*)"$/, '$1'))
    .filter((x) => x !== '')
}
function loaded(value: string | null, library: string): boolean | null {
  const list = libraries(value)
  return list === null ? null : list.includes(library)
}
/** True: every server preloads it; false: a server does not; null: the API does not say (PostgreSQL hides the setting from most roles). */
function preloadedEverywhere(library: string): boolean | null {
  if (servers.value.length === 0) return null
  const each = servers.value.map((s) => loaded(s.value, library))
  if (each.includes(false)) return false
  return each.every((x) => x === true) ? true : null
}
const shown = (library: string) => {
  const v = preloadedEverywhere(library)
  return v === true
    ? { tone: 'ok', icon: '✓', text: 'Preloaded on every server' }
    : v === false
      ? { tone: 'warn', icon: '▲', text: 'Not preloaded on every server' }
      : { tone: 'neutral', icon: '?', text: 'Not reported by the API' }
}
</script>

<template>
  <section class="stack" aria-labelledby="ext-h" data-testid="extensions-panel">
    <h2 id="ext-h">Extensions</h2>
    <p class="muted">
      Whether the libraries of the two extensions are loaded on each server (the setting
      <code>shared_preload_libraries</code>, which the cluster manages). A library that is not
      loaded is loaded by restarting the instance of that server.
    </p>
    <QueryBoundary
      :loading="parameter.isLoading.value"
      :error="parameter.error.value"
      @retry="parameter.refetch()"
    >
      <div
        v-for="e in EXTENSIONS"
        :key="e.library"
        class="card stack"
        :data-testid="`ext-${e.library}`"
      >
        <div class="row-between">
          <h3>
            {{ e.title }} (<code>{{ e.library }}</code
            >)
          </h3>
          <span
            class="badge"
            :class="`tone-${shown(e.library).tone}`"
            :data-testid="`ext-${e.library}-state`"
          >
            <span aria-hidden="true">{{ shown(e.library).icon }}</span>
            <span>{{ shown(e.library).text }}</span>
          </span>
        </div>
        <p>{{ e.text }}</p>
        <TableWrap>
          <table>
            <caption>
              Preloaded per server:
              {{
                e.library
              }}
            </caption>
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col">Preloaded</th>
                <th scope="col">State of the setting</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="s in servers"
                :key="s.serverId"
                :data-testid="`ext-${e.library}-${s.serverId}`"
              >
                <th scope="row"><SafeName :value="s.serverId" kind="text" :max="60" /></th>
                <td>
                  {{
                    loaded(s.value, e.library) === null
                      ? 'Unknown'
                      : loaded(s.value, e.library)
                        ? 'Yes'
                        : 'No'
                  }}
                </td>
                <td>
                  {{ s.state
                  }}<template v-if="s.pendingValue !== null"> (restart pending)</template>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </div>
      <p
        v-if="parameter.data.value?.hidden"
        class="notice tone-info"
        role="note"
        data-testid="ext-hidden-note"
      >
        <span aria-hidden="true">&#9432;</span>
        <span
          >PostgreSQL hides the value of <code>shared_preload_libraries</code> from this role, so
          the API cannot say which libraries are preloaded. A conversion to columnar storage says
          <code>extension_missing</code> when the library is not loaded.</span
        >
      </p>
      <p class="notice tone-info" role="note" data-testid="ext-install-note">
        <span aria-hidden="true">&#9432;</span>
        <span>
          The API does not report which databases have these extensions installed and has no action
          that installs them. The cluster installs <code>pg_stat_statements</code> in the databases
          it makes. Conversion of a table to columnar storage says
          <code>extension_missing</code> when the extension is not installed. See the
          <RouterLink :to="{ name: 'databases' }">databases</RouterLink> for the storage of their
          tables.
        </span>
      </p>
    </QueryBoundary>
  </section>
</template>
