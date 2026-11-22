<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Step 1 of an import: choose a file (a drop zone and a file input). The first 64 KiB are read here, in the browser
// (`File.slice`), for the preview and for a guess of the delimiter, the header and the encoding; nothing is sent. The size of
// the file is shown against the limit of one import before anything is uploaded, and a file above it is refused here.
import { computed, ref } from 'vue'
import SafeName from '@/components/SafeName.vue'
import { guessCsv, PREVIEW_BYTES, type CsvGuess } from '@/lib/csv-guess'
import { formatBytes } from '@/lib/format'
import { MAX_ATOMIC_BYTES } from '@/lib/transfer'

const props = defineProps<{ maxFileBytes: number | null }>()
const emit = defineEmits<{
  chosen: [payload: { file: File; bytes: Uint8Array; guess: CsvGuess; refused: string | null }]
  cleared: []
}>()

const input = ref<HTMLInputElement | null>(null)
const over = ref(false)
const reading = ref(false)
const problem = ref<string | null>(null)
const file = ref<File | null>(null)
const guess = ref<CsvGuess | null>(null)

/** Why the file cannot be imported, in words, or null. */
function refusal(f: File, g: CsvGuess): string | null {
  if (f.size === 0) return 'The file is empty.'
  if (props.maxFileBytes !== null && f.size > props.maxFileBytes)
    return `The file is larger than the ${formatBytes(props.maxFileBytes)} this cluster stages. Split the file into parts of at most ${formatBytes(MAX_ATOMIC_BYTES)} and import them one after the other.`
  // A compressed file is counted by the server after it is expanded; its size here proves nothing about that.
  if (!g.compressed && f.size > MAX_ATOMIC_BYTES)
    return `An import is all or nothing and takes at most ${formatBytes(MAX_ATOMIC_BYTES)} of data. Split the file into parts of at most ${formatBytes(MAX_ATOMIC_BYTES)} and import them one after the other.`
  return null
}

const refused = computed(() =>
  file.value && guess.value ? refusal(file.value, guess.value) : null,
)

async function take(f: File | undefined): Promise<void> {
  problem.value = null
  if (!f) return
  reading.value = true
  try {
    // Only the beginning of the file is read, and only here.
    const bytes = new Uint8Array(await f.slice(0, PREVIEW_BYTES).arrayBuffer())
    file.value = f
    guess.value = guessCsv(bytes, f.size)
    emit('chosen', { file: f, bytes, guess: guess.value, refused: refusal(f, guess.value) })
  } catch {
    file.value = null
    guess.value = null
    problem.value = 'The browser could not read this file.'
    emit('cleared')
  } finally {
    reading.value = false
  }
}

function onPick(event: Event): void {
  void take((event.target as HTMLInputElement).files?.[0])
}
function onDrop(event: DragEvent): void {
  over.value = false
  void take(event.dataTransfer?.files?.[0])
}
function clear(): void {
  file.value = null
  guess.value = null
  if (input.value) input.value.value = ''
  emit('cleared')
}
</script>

<template>
  <div class="stack" data-testid="import-file-step">
    <!-- A drop zone is a convenience for the mouse; the file input inside it is the control, and it works by keyboard. -->
    <!-- eslint-disable-next-line vuejs-accessibility/no-static-element-interactions -->
    <div
      class="drop"
      :class="{ over }"
      data-testid="drop-zone"
      @dragover.prevent="over = true"
      @dragleave="over = false"
      @drop.prevent="onDrop"
    >
      <label for="import-file"><strong>Choose a CSV file</strong> or drop it here</label>
      <input
        id="import-file"
        ref="input"
        type="file"
        accept=".csv,.tsv,.txt,.gz,text/csv,text/plain,application/gzip"
        data-autofocus
        aria-describedby="import-limits"
        @change="onPick"
      />
    </div>
    <p id="import-limits" class="muted" data-testid="import-limits">
      An import is all or nothing and takes at most {{ formatBytes(MAX_ATOMIC_BYTES) }} of data (a
      compressed file is counted after it is expanded). Split a larger file. Nothing is sent until
      you confirm the options.<template v-if="maxFileBytes !== null">
        The cluster stages files of at most {{ formatBytes(maxFileBytes) }}.</template
      >
    </p>
    <p v-if="reading" class="muted" role="status">Reading the beginning of the file ...</p>
    <p v-if="problem" class="notice tone-danger" role="alert">
      <span aria-hidden="true">&#10006;</span> <span>{{ problem }}</span>
    </p>
    <div v-if="file && guess" class="stack" data-testid="chosen-file">
      <div class="row-between">
        <span
          ><SafeName :value="file.name" :max="100" />
          <span class="muted num">({{ formatBytes(file.size) }})</span></span
        >
        <button type="button" class="btn btn-small" @click="clear">Choose another file</button>
      </div>
      <div
        class="progress"
        role="img"
        :aria-label="`${formatBytes(file.size)} of the ${formatBytes(MAX_ATOMIC_BYTES)} an import takes`"
      >
        <span
          :class="{ over: refused !== null }"
          :style="{ width: `${Math.min(100, (file.size / MAX_ATOMIC_BYTES) * 100)}%` }"
        />
      </div>
      <p v-if="refused" class="notice tone-danger" role="alert" data-testid="file-refused">
        <span aria-hidden="true">&#10006;</span> <span>{{ refused }}</span>
      </p>
      <p v-else-if="guess.compressed" class="notice tone-info" data-testid="file-compressed">
        <span aria-hidden="true">&#9432;</span>
        <span
          >This file is compressed (gzip). It cannot be previewed here; the server detects the
          compression and counts the data after expanding it.</span
        >
      </p>
    </div>
  </div>
</template>

<style scoped>
.drop {
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
  padding: var(--s-5) var(--s-4);
  border: 2px dashed var(--c-border-strong);
  border-radius: var(--radius);
  background: var(--c-surface);
}
.drop.over {
  border-color: var(--c-accent);
  background: var(--c-surface-2);
}
.progress span.over {
  background: var(--c-danger-solid);
}
</style>
