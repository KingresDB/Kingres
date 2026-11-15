<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// An inclusion proof as the server sent it: what it says, the file to save for `kingres-verify`
// (the exact text of the answer), and how to check it. Every string comes from the server and is
// drawn through SafeName.
import { computed } from 'vue'
import SafeName from '@/components/SafeName.vue'
import { saveTextFile } from '@/lib/download'
import { formatNumber } from '@/lib/format'
import type { RawProof } from '@/lib/ledger-proof'

const props = defineProps<{ proof: RawProof }>()

const v = computed(() => props.proof.view)
const tableId = computed(() =>
  /^\d{1,20}$/.test(props.proof.tableId) ? props.proof.tableId : '<table id>',
)
const command = computed(
  () =>
    `kingres-verify proof --proof proof.json --digest digest.json --table-id ${tableId.value} \\n  --row-hash <hash recomputed from your own copy of the row>`,
)
function download(): void {
  saveTextFile('proof.json', props.proof.raw)
}
</script>

<template>
  <div class="stack" data-testid="proof">
    <p class="muted">
      This proof says that a ledger entry with the row hash below, for the table with the id below,
      is part of a block that is reached from a digest of the same chain. It cannot know the row
      itself: you recompute the row hash from your own copy of the row (with
      <code>kingres-verify row-hash</code>) and the verifier compares it.
    </p>
    <dl class="kv">
      <dt>Chain</dt>
      <dd>
        <SafeName :value="v.chain_id" kind="text" :max="80" /> (shard group {{ v.shard_group_id }})
      </dd>
      <dt>Table</dt>
      <dd>
        <SafeName v-if="v.relation" :value="v.relation" :max="120" />
        <span class="muted"> (table id {{ tableId }})</span>
      </dd>
      <dt>Entry</dt>
      <dd>
        <strong>{{ v.op === 'I' ? 'Insert' : 'Delete' }}</strong>, sequence {{ v.seq }}, transaction
        id
        {{ v.ledger_txid }}
      </dd>
      <dt>Row hash</dt>
      <dd class="mono"><SafeName :value="v.row_hash" kind="text" :max="80" /></dd>
      <dt>Position in the transaction</dt>
      <dd>entry {{ v.entry_index }} of {{ v.entry_count }}</dd>
      <dt>Position in the block</dt>
      <dd>
        transaction {{ v.tx_index }} of {{ v.block.tx_count }}, written by
        <SafeName :value="v.user_name" :max="60" />
      </dd>
      <dt>Block</dt>
      <dd>
        height {{ formatNumber(v.block.height) }}
        <span v-if="v.block.block_hash" class="mono"
          ><SafeName :value="v.block.block_hash" kind="text" :max="80"
        /></span>
      </dd>
      <dt>Path to a digest</dt>
      <dd>{{ v.chain.length }} later block header(s)</dd>
    </dl>
    <div class="row" style="gap: var(--s-6)">
      <div>
        <h4>Entry path ({{ v.entry_path.length }} hashes)</h4>
        <ol class="mono path">
          <li v-for="(h, i) in v.entry_path" :key="i">
            <SafeName :value="h" kind="text" :max="80" />
          </li>
        </ol>
      </div>
      <div>
        <h4>Transaction path ({{ v.tx_path.length }} hashes)</h4>
        <ol class="mono path">
          <li v-for="(h, i) in v.tx_path" :key="i">
            <SafeName :value="h" kind="text" :max="80" />
          </li>
        </ol>
      </div>
    </div>
    <div>
      <button type="button" class="btn btn-primary" @click="download">Download proof.json</button>
    </div>
    <section class="stack" aria-label="How to check this proof">
      <h3>How to check it</h3>
      <p>
        The check is done by the separate program <code>kingres-verify</code>, not by this panel and
        not by the server. It needs the proof file above and a digest of the same chain that you
        saved <strong>outside the database</strong> when it was generated (download it from the
        Digests list on this page). Then run:
      </p>
      <pre class="mono" data-testid="verify-command">{{ command }}</pre>
      <p class="muted">
        <code>VERIFIED</code> and exit code 0 mean the entry is in the chain that the digest covers.
        The files are saved exactly as the server sent them; do not edit or reformat them.
      </p>
    </section>
  </div>
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
  overflow-wrap: anywhere;
}
.path {
  margin: 0;
  padding-left: 1.6em;
  overflow-wrap: anywhere;
}
pre {
  margin: 0;
  padding: var(--s-3);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
