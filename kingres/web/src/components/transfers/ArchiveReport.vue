<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The report of the validation of a database archive (contract 0.29.0, a placeholder revision): whether it is valid, the
// versions and the locale of its source, the extensions and what is installed here, the tables by kind, the statements by
// kind, the statements the shape grammar refused, what an import would need, the warnings and the reasons. The text of a
// statement of an archive is never shown, and the console never gets it: a refused statement is a kind, a position and a
// fingerprint. Every string of the report is shown through SafeName, because an archive is a file from anyone.
import { computed } from 'vue'
import SafeName from '@/components/SafeName.vue'
import type { ArchiveReport } from '@/api/types'
import { formatBytes, formatNumber } from '@/lib/format'
import {
  IMPORT_NOT_AVAILABLE_TEXT,
  LEDGER_RESTART_TEXT,
  reportHeadline,
  TABLE_KINDS,
} from '@/lib/archive'

const props = defineProps<{ report: ArchiveReport }>()
const kinds = computed(() =>
  Object.entries(props.report.statements?.byKind ?? {}).sort(([a], [b]) => a.localeCompare(b)),
)
const refused = computed(() => props.report.statements?.refused ?? [])
</script>

<template>
  <div class="stack" data-testid="archive-report" :data-valid="report.valid">
    <p
      class="notice"
      :class="report.valid ? 'tone-ok' : 'tone-danger'"
      :role="report.valid ? 'status' : 'alert'"
      data-testid="report-headline"
    >
      <span aria-hidden="true">{{ report.valid ? '✓' : '✖' }}</span>
      <span>{{ reportHeadline(report) }}</span>
    </p>

    <section v-if="report.reasons.length" aria-labelledby="report-reasons-h">
      <h3 id="report-reasons-h">Why it cannot be used here</h3>
      <ul class="stack" style="gap: var(--s-1)" data-testid="report-reasons">
        <li v-for="(r, i) in report.reasons" :key="i">
          <code><SafeName :value="r.object" :max="80" /></code>:
          <SafeName :value="r.message" kind="text" :max="240" />
          <span class="muted"> (<SafeName :value="r.code" :max="60" />)</span>
        </li>
      </ul>
    </section>

    <section v-if="refused.length" aria-labelledby="report-refused-h">
      <h3 id="report-refused-h">Statements that are refused</h3>
      <p class="muted">
        The text of a statement is not shown. A database with code in a schema (functions, triggers,
        policies, rules) is a job for pg_dump and pg_restore.
      </p>
      <ul class="stack" style="gap: var(--s-1)" data-testid="report-refused">
        <li v-for="(r, i) in refused" :key="i">
          <strong><SafeName :value="r.kind" :max="60" /></strong> at
          <code><SafeName :value="r.position" :max="80" /></code>, fingerprint
          <code><SafeName :value="r.fingerprint" kind="text" :max="80" /></code>:
          <SafeName :value="r.reason" kind="text" :max="240" />
        </li>
      </ul>
    </section>

    <section aria-labelledby="report-versions-h">
      <h3 id="report-versions-h">Source</h3>
      <dl class="kv" data-testid="report-versions">
        <dt>Format</dt>
        <dd class="mono"><SafeName :value="report.format || '-'" :max="60" /></dd>
        <dt>PostgreSQL</dt>
        <dd>{{ report.versions.postgresMajor || '-' }}</dd>
        <dt>KingresDB</dt>
        <dd><SafeName :value="report.versions.kingresVersion || '-'" :max="60" /></dd>
        <dt>Encoding</dt>
        <dd><SafeName :value="report.versions.encoding || '-'" :max="60" /></dd>
        <dt>Locale</dt>
        <dd>
          <SafeName :value="report.versions.lcCollate || '-'" :max="60" />,
          <SafeName :value="report.versions.lcCtype || '-'" :max="60" />
        </dd>
        <dt v-if="report.sha256">SHA-256</dt>
        <dd v-if="report.sha256" class="mono wrap">
          <SafeName :value="report.sha256" kind="text" :max="64" />
        </dd>
      </dl>
      <table v-if="report.versions.extensions.length" class="table" data-testid="report-extensions">
        <caption class="sr-only">
          Extensions of the archive
        </caption>
        <thead>
          <tr>
            <th scope="col">Extension</th>
            <th scope="col">Version</th>
            <th scope="col">Schema</th>
            <th scope="col">Installed here</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="e in report.versions.extensions" :key="`${e.name}-${e.schema}`">
            <th scope="row" class="mono"><SafeName :value="e.name" :max="60" /></th>
            <td><SafeName :value="e.version" kind="text" :max="30" /></td>
            <td class="mono"><SafeName :value="e.schema" :max="60" /></td>
            <td>
              <SafeName v-if="e.installed" :value="e.installed" kind="text" :max="30" />
              <span v-else class="muted">not installed</span>
            </td>
          </tr>
        </tbody>
      </table>
    </section>

    <section aria-labelledby="report-tables-h">
      <h3 id="report-tables-h">Tables</h3>
      <dl class="kv" data-testid="report-tables">
        <template v-for="k in TABLE_KINDS" :key="k.key">
          <dt>{{ k.label }}</dt>
          <dd>{{ formatNumber(report.tables[k.key]) }}</dd>
        </template>
        <dt>Rows</dt>
        <dd>{{ formatNumber(report.tables.rows) }}</dd>
        <dt>Data</dt>
        <dd>{{ formatBytes(report.tables.bytes) }}</dd>
      </dl>
    </section>

    <section v-if="kinds.length" aria-labelledby="report-kinds-h">
      <h3 id="report-kinds-h">Statements by kind</h3>
      <dl class="kv" data-testid="report-kinds">
        <template v-for="[kind, n] in kinds" :key="kind">
          <dt class="mono"><SafeName :value="kind" :max="60" /></dt>
          <dd>{{ formatNumber(n) }}</dd>
        </template>
      </dl>
    </section>

    <p v-if="report.needs?.ledgerRestart" class="notice tone-warn" data-testid="report-ledger">
      <span aria-hidden="true">▲</span>
      <span>{{ LEDGER_RESTART_TEXT }}</span>
    </p>
    <p v-if="report.needs?.largeObjectsLost" class="notice tone-warn" data-testid="report-lobs">
      <span aria-hidden="true">▲</span>
      <span>The source had large objects. The archive does not carry them.</span>
    </p>

    <section v-if="report.warnings?.length" aria-labelledby="report-warnings-h">
      <h3 id="report-warnings-h">Warnings</h3>
      <ul class="stack" style="gap: var(--s-1)" data-testid="report-warnings">
        <li v-for="(w, i) in report.warnings" :key="i">
          <SafeName :value="w" kind="text" :max="240" />
        </li>
      </ul>
    </section>

    <p class="muted" data-testid="report-no-import">{{ IMPORT_NOT_AVAILABLE_TEXT }}</p>
  </div>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
  min-width: 0;
}
</style>
