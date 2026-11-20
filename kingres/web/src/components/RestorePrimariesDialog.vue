<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Restore preferred primaries (design 8.1 and 5.5, contract 0.10.0 and 0.12.0): after a failed server has returned, every group
// whose primary is not on its home server is switched back, one group at a time, each by a planned switchover. Two steps in one
// dialog: choose the groups (all of them that are away, or some), then the plan (a dry run) group by group, with the time writes
// pause and any blocker; nothing starts before the plan is shown. A refusal of the plan or of the start is told in the words of
// its problem code; an answer `unsupported_operation` is the calm "not available on this server version yet" and nothing changed.
// A switchover loses no commit, so there is no decision about lost data here.
import { computed, nextTick, ref, watch } from 'vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FailoverPlanTable from './FailoverPlanTable.vue'
import FailoverRefusal from './FailoverRefusal.vue'
import SafeName from './SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTopology } from '@/api/queries'
import { groupTitle, hardBlockers } from '@/lib/failover'
import type { FailoverPlan, ShardGroup } from '@/api/types'

const emit = defineEmits<{ close: [] }>()
const topology = useTopology()
/** The groups whose primary is not on its preferred server, the map group first. */
const away = computed<ShardGroup[]>(() =>
  (topology.data.value?.groups ?? [])
    .filter((g) => g.primary && !g.primary.isOnPreferredServer)
    .sort((a, b) => Number(b.isMapGroup) - Number(a.isMapGroup) || a.id - b.id),
)
const unticked = ref<Record<number, boolean>>({})
const picked = computed(() => away.value.filter((g) => !unticked.value[g.id]))
const stage = ref<'choose' | 'review'>('choose')
const plan = ref<FailoverPlan | null>(null)
const planError = ref<unknown>(null)
const planning = ref(false)
const heading = ref<HTMLElement | null>(null)
const context = computed(() => ({
  mode: 'restore' as const,
  includesMapGroup: picked.value.some((g) => g.isMapGroup),
}))

function request(): { shardGroupIds?: number[] } {
  // All of them that are away: the request names none, so that a group that comes back meanwhile is not left out by a stale list.
  return picked.value.length === away.value.length
    ? {}
    : { shardGroupIds: picked.value.map((g) => g.id) }
}

async function review(): Promise<void> {
  planning.value = true
  planError.value = null
  try {
    plan.value = await endpoints.previewRestore(request())
    stage.value = 'review'
    await nextTick()
    heading.value?.focus()
  } catch (e) {
    planError.value = e
  } finally {
    planning.value = false
  }
}
function back(): void {
  stage.value = 'choose'
  planError.value = null
}
watch(
  () => JSON.stringify(request()),
  () => {
    if (stage.value === 'review') back()
  },
)

const stuck = computed(() => (plan.value ? hardBlockers(plan.value.blockers) : []))
const noTarget = computed(() => plan.value?.groups.some((g) => g.toServerId === null) ?? false)
const emptyPlan = computed(() => plan.value !== null && plan.value.groups.length === 0)
const blocked = computed(() =>
  stage.value === 'choose'
    ? topology.isPending.value || picked.value.length === 0 || planning.value
    : stuck.value.length > 0 || noTarget.value || emptyPlan.value,
)
function validate(): boolean {
  if (stage.value === 'choose') {
    void review()
    return false
  }
  return true
}
const start = () => endpoints.restorePreferredPrimaries(request())
</script>

<template>
  <TaskActionDialog
    title="Restore preferred primaries"
    :confirm-label="stage === 'choose' ? 'Show the plan' : 'Restore primaries'"
    :start="start"
    :validate="validate"
    :invalidate="[keys.topology, keys.servers, keys.failover]"
    :confirm-disabled="blocked"
    wide
    @close="emit('close')"
  >
    <template #error="{ error }">
      <FailoverRefusal :error="error" :context="context" />
    </template>
    <template #default>
      <template v-if="stage === 'choose'">
        <p>
          A group whose primary runs away from its home server (after a failover) is switched back
          by a planned switchover, one group at a time. No commit is lost; writes to a group pause
          for a moment while it moves. The plan comes next, and nothing changes before you confirm
          it.
        </p>
        <p v-if="topology.isPending.value" role="status">Reading the shard groups ...</p>
        <p
          v-else-if="away.length === 0"
          class="notice tone-info"
          role="status"
          data-testid="nothing-to-restore"
        >
          <span aria-hidden="true">&#9432;</span>
          <span>Every group already runs its primary on its preferred server.</span>
        </p>
        <fieldset v-else class="stack" data-testid="restore-groups">
          <legend>Groups to switch back</legend>
          <label
            v-for="g in away"
            :key="g.id"
            class="check"
            :data-testid="`restore-choice-${g.id}`"
          >
            <input
              type="checkbox"
              :checked="!unticked[g.id]"
              @change="unticked[g.id] = !($event.target as HTMLInputElement).checked"
            />
            {{ groupTitle(g.id, g.isMapGroup) }}: from server
            <SafeName :value="g.primary?.serverId ?? ''" kind="text" :max="60" /> back to server
            <SafeName :value="g.preferredPrimaryServerId" kind="text" :max="60" />
            <span v-if="g.isMapGroup" class="muted"> (moves the manager of the cluster)</span>
          </label>
        </fieldset>
        <FailoverRefusal v-if="planError" :error="planError" :context="context" />
        <p v-if="planning" role="status" data-testid="planning">Reading the plan ...</p>
      </template>
      <template v-else-if="plan">
        <h3 ref="heading" tabindex="-1" data-testid="plan-heading">The plan</h3>
        <p class="muted">
          This is what the server would do now. Nothing has changed yet.
          <button type="button" class="btn btn-small" data-testid="plan-back" @click="back">
            Change the choice
          </button>
        </p>
        <p v-if="emptyPlan" class="notice tone-info" role="status" data-testid="plan-empty">
          <span aria-hidden="true">&#9432;</span>
          <span
            >Nothing to restore: no group of this choice runs away from its home server now.</span
          >
        </p>
        <FailoverPlanTable v-else :plan="plan" />
        <p v-if="stuck.length > 0" class="muted" data-testid="plan-stuck">
          The restore cannot start while the blockers above stand. Wait until they are gone.
        </p>
      </template>
    </template>
  </TaskActionDialog>
</template>
