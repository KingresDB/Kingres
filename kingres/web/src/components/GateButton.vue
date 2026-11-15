<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A button for an operation in the contract. When the signed-in role is below the operation's
// `x-required-role`, the button is aria-disabled (still focusable, so the reason can be read) and
// says why. The role requirement comes from the generated operation table.
import { computed } from 'vue'
import { denialReason, type OperationId } from '@/api/roles'
import { useSessionStore } from '@/stores/session'

const props = withDefaults(
  defineProps<{
    op: OperationId
    variant?: 'default' | 'primary' | 'danger'
    small?: boolean
    disabled?: boolean
    /** Why the action cannot work right now (not a role problem); makes the button aria-disabled and says why. */
    blockedReason?: string
    type?: 'button' | 'submit'
  }>(),
  { variant: 'default', small: false, disabled: false, blockedReason: '', type: 'button' },
)
const emit = defineEmits<{ click: [event: MouseEvent] }>()

const session = useSessionStore()
const allowed = computed(() => session.can(props.op))
const reason = computed(() => (allowed.value ? props.blockedReason : denialReason(props.op)))
const reasonId = `gate-${Math.random().toString(36).slice(2, 9)}`
const blocked = computed(() => !allowed.value || props.disabled || props.blockedReason !== '')

function onClick(event: MouseEvent): void {
  if (blocked.value) {
    event.preventDefault()
    return
  }
  emit('click', event)
}
</script>

<template>
  <button
    :type="type"
    class="btn"
    :class="[variant === 'default' ? '' : `btn-${variant}`, { 'btn-small': small }]"
    :aria-disabled="blocked ? 'true' : undefined"
    :aria-describedby="reason ? reasonId : undefined"
    :title="reason || undefined"
    @click="onClick"
  >
    <slot />
    <span v-if="reason" :id="reasonId" class="sr-only">{{ reason }}</span>
  </button>
</template>
