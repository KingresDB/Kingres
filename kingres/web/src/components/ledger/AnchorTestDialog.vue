<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The test of an anchor target (contract 0.11.0, design 6.8): the server runs the named checks now and answers at once; a
// failure of the remote side is a failed check in the answer, not an error. The dialog shows the result of the last test
// when there is one, says what the test leaves behind before the click (for a WORM store: one empty probe object), runs the
// test on request, and shows each check with its status in words as well as in a symbol. Nothing a server sends is drawn
// without SafeName, and a detail never holds a credential (the server filters it; the console never adds one).
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import { checkMeta, kindLabel, testSummary, WORM_PROBE_TEXT } from '@/lib/ledger-anchor'
import type { LedgerAnchorTarget, LedgerAnchorTest } from '@/api/types'

const props = defineProps<{ target: LedgerAnchorTarget }>()
const emit = defineEmits<{ close: [] }>()
const client = useQueryClient()

const fresh = ref<LedgerAnchorTest | null>(null)
const result = computed(() => fresh.value ?? props.target.lastTest)
const isFresh = computed(() => fresh.value !== null)
const run = useMutation({
  mutationFn: () => endpoints.testLedgerAnchorTarget(props.target.id),
  onSuccess: async (t) => {
    fresh.value = t
    await client.invalidateQueries({ queryKey: keys.ledgerAnchorTargets })
  },
})
</script>

<template>
  <AppDialog
    title="Test the anchor target"
    wide
    :persistent="run.isPending.value"
    @close="emit('close')"
  >
    <div class="stack" data-testid="anchor-test">
      <p>
        <strong><SafeName :value="target.name" kind="text" :max="63" /></strong>
        <span class="muted"> ({{ kindLabel(target.kind) }})</span>
      </p>
      <p class="muted">
        The test runs a short list of checks against the target now and shows what each found. It
        changes nothing in the ledger.
      </p>
      <p
        v-if="target.kind === 'worm'"
        class="notice tone-info"
        role="status"
        data-testid="probe-note"
      >
        <span aria-hidden="true">&#9432;</span><span>{{ WORM_PROBE_TEXT }}</span>
      </p>
      <p
        v-if="run.isPending.value"
        class="notice tone-info"
        role="status"
        data-testid="test-running"
      >
        <span aria-hidden="true">&#9680;</span>
        <span>Testing. This can take up to 30 seconds.</span>
      </p>
      <ProblemNotice v-if="run.error.value" :error="run.error.value" />
      <section
        v-if="result"
        class="stack"
        aria-labelledby="test-result-h"
        data-testid="test-result"
      >
        <h3 id="test-result-h">{{ isFresh ? 'Result' : 'Result of the last test' }}</h3>
        <p class="row">
          <StatusBadge :meta="testSummary(result)" />
          <span v-if="result.testedAt" class="muted">{{ formatDateTime(result.testedAt) }}</span>
        </p>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Checks of the test
            </caption>
            <thead>
              <tr>
                <th scope="col">Check</th>
                <th scope="col">Result</th>
                <th scope="col">What was found</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(c, i) in result.checks" :key="i" :data-testid="`check-${c.status}`">
                <th scope="row"><SafeName :value="c.name" kind="text" :max="80" /></th>
                <td><StatusBadge :meta="checkMeta(c.status)" /></td>
                <td><SafeName :value="c.detail" kind="text" :max="400" multiline /></td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>
      <p v-else-if="!run.isPending.value" class="muted" data-testid="never-tested">
        This target has not been tested yet.
      </p>
    </div>
    <template #footer>
      <button type="button" class="btn" :disabled="run.isPending.value" @click="emit('close')">
        Close
      </button>
      <GateButton
        op="testLedgerAnchorTarget"
        variant="primary"
        :disabled="run.isPending.value"
        @click="run.mutate()"
        >{{ result ? 'Run the test again' : 'Run the test' }}</GateButton
      >
    </template>
  </AppDialog>
</template>
