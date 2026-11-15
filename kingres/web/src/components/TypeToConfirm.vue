<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "Type the name to confirm". The name is shown in its escaped form (hidden characters visible),
// but what has to be typed is the real name. A name that cannot be typed (hidden characters, or
// very long) can be copied with the button and pasted.
import { computed, ref } from 'vue'
import SafeName from './SafeName.vue'
import { hasHidden, spaceIssues } from '@/lib/safe-text'

const props = defineProps<{ expected: string }>()
const model = defineModel<string>({ required: true })

const id = `tc-${Math.random().toString(36).slice(2, 9)}`
const hard = computed(
  () =>
    hasHidden(props.expected) ||
    spaceIssues(props.expected).length > 0 ||
    [...props.expected].length > 40,
)
const copied = ref(false)

async function copy(): Promise<void> {
  try {
    await navigator.clipboard.writeText(props.expected)
    copied.value = true
  } catch {
    copied.value = false
  }
}
</script>

<template>
  <div class="field">
    <label :for="id">
      Type <code><SafeName :value="expected" kind="name" :max="60" /></code> to confirm
    </label>
    <input :id="id" v-model="model" class="input mono" autocomplete="off" spellcheck="false" />
    <p v-if="hard" class="hint muted">
      This name has hidden characters, spaces that are easy to miss, or many characters. Type it
      exactly as it is (the box compares the real name, not what is drawn above), or copy it and
      paste it.
      <button type="button" class="btn btn-small" @click="copy">Copy the name</button>
      <span v-if="copied" role="status"> Copied.</span>
    </p>
  </div>
</template>
