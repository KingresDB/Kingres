<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The failover dialog (design 8.2). Three steps in one dialog:
//   1. Choose: the mode, the groups the server leads, the standby to promote per group, a comment.
//   2. The plan: a dry run (`previewFailover`) shown group by group with the verdict about lost data in words, the
//      estimated time, the blockers and whether a running rebalance will be stopped. The dry run is also where a
//      refusal shows (the server answers the same refusals for it as for the request), so a request that cannot work
//      is refused before the person has typed anything. A refusal of the start itself is shown in the same words.
//   3. Confirm: for a hard failover the address of the server is typed; when the verdict is not "no data loss" the
//      person ticks that data may be lost, which is what sets `allowDataLoss`. Starting closes into the progress.
// The plan of a failover that needs a decision is read again after the server refuses with 409 `failover_needs_decision`:
// the `Problem` of the contract has no member for the plan that the text of the 409 promises, so the dialog asks for it.
import { computed, nextTick, ref, watch } from 'vue'
import type { QueryKey } from '@tanstack/vue-query'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import SafeName from './SafeName.vue'
import FailoverPlanTable from './FailoverPlanTable.vue'
import FailoverRefusal from './FailoverRefusal.vue'
import StatusBadge from './StatusBadge.vue'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { keys, useTopology } from '@/api/queries'
import { hardBlockers, groupTitle, needsDecision } from '@/lib/failover'
import { formatLag } from '@/lib/format'
import { safeText } from '@/lib/safe-text'
import { SYNC_STATE } from '@/lib/status'
import type { FailoverPlan, FailoverRequest, Server, ShardGroup } from '@/api/types'

const props = defineProps<{ server: Server; invalidate?: QueryKey[] }>()
const emit = defineEmits<{ close: [] }>()

const topology = useTopology()
/** The groups whose primary runs on this server, the map group first. */
const owned = computed<ShardGroup[]>(() =>
  (topology.data.value?.groups ?? [])
    .filter((g) => g.primary?.serverId === props.server.id)
    .sort((a, b) => Number(b.isMapGroup) - Number(a.isMapGroup) || a.id - b.id),
)

// A server that answers is handed over from; one that does not is failed over.
const mode = ref<'hard' | 'graceful'>(props.server.status === 'live' ? 'graceful' : 'hard')
const unticked = ref<Record<number, boolean>>({})
const target = ref<Record<number, string>>({})
const comment = ref('')
const stage = ref<'choose' | 'review'>('choose')
const plan = ref<FailoverPlan | null>(null)
const planError = ref<unknown>(null)
const planning = ref(false)
const acceptLoss = ref(false)
const heading = ref<HTMLElement | null>(null)

const picked = computed(() => owned.value.filter((g) => !unticked.value[g.id]))
const includesMapGroup = computed(() => picked.value.some((g) => g.isMapGroup))
const refusalContext = computed(() => ({
  mode: mode.value,
  includesMapGroup: includesMapGroup.value,
}))

function request(): Omit<FailoverRequest, 'dryRun'> {
  const targets = picked.value
    .filter((g) => (target.value[g.id] ?? '') !== '')
    .map((g) => ({ shardGroupId: g.id, serverId: target.value[g.id] as string }))
  const text = comment.value.trim()
  return {
    mode: mode.value,
    shardGroupIds: picked.value.map((g) => g.id),
    ...(targets.length > 0 ? { targets } : {}),
    ...(text !== '' ? { comment: text } : {}),
  }
}

const canReview = computed(
  () => !topology.isPending.value && picked.value.length > 0 && !planning.value,
)

async function review(): Promise<void> {
  planning.value = true
  planError.value = null
  try {
    plan.value = await endpoints.previewFailover(props.server.id, request())
    acceptLoss.value = false
    stage.value = 'review'
    await nextTick()
    heading.value?.focus()
  } catch (e) {
    planError.value = e
  } finally {
    planning.value = false
  }
}

/** Reads the plan again, after the server says the plan changed under the person's hands. */
async function refreshPlan(): Promise<void> {
  try {
    plan.value = await endpoints.previewFailover(props.server.id, request())
    acceptLoss.value = false
  } catch (e) {
    planError.value = e
    stage.value = 'choose'
  }
}

function back(): void {
  stage.value = 'choose'
  planError.value = null
}

// A change of the choice makes the plan stale: it is read again before anything can start.
// The key is the request itself: the page reads the topology every few seconds, which makes new arrays of the same groups.
watch(
  () => JSON.stringify(request()),
  () => {
    if (stage.value === 'review') back()
  },
)

const loss = computed(() => plan.value?.groups.some((g) => needsDecision(g.dataLoss)) ?? false)
const stuck = computed(() => (plan.value ? hardBlockers(plan.value.blockers) : []))
const noTarget = computed(() => plan.value?.groups.some((g) => g.toServerId === null) ?? false)
const emptyPlan = computed(() => plan.value !== null && plan.value.groups.length === 0)
const startBlocked = computed(() => {
  if (stage.value === 'choose') return !canReview.value
  return (
    stuck.value.length > 0 || noTarget.value || emptyPlan.value || (loss.value && !acceptLoss.value)
  )
})

const config = computed(() => ({
  title: `Fail over server ${props.server.address}`,
  confirmLabel:
    stage.value === 'choose'
      ? 'Show the plan'
      : mode.value === 'hard'
        ? 'Hard failover'
        : 'Graceful failover',
  danger: stage.value === 'review' && mode.value === 'hard',
  // Marking a server dead is not undone by a click: the address has to be typed.
  typeToConfirm:
    stage.value === 'review' && mode.value === 'hard' ? props.server.address : undefined,
}))

function validate(): boolean {
  if (stage.value === 'choose') {
    void review()
    return false
  }
  return true
}

async function start() {
  try {
    return await endpoints.failoverServer(props.server.id, {
      ...request(),
      ...(acceptLoss.value ? { allowDataLoss: true } : {}),
    })
  } catch (e) {
    if (e instanceof ApiError && e.code === 'failover_needs_decision') await refreshPlan()
    throw e
  }
}

function standbyText(g: ShardGroup, serverId: string): string {
  const m = g.standbys.find((x) => x.serverId === serverId)
  if (!m) return safeText(serverId, 60)
  const sync = m.syncState ? SYNC_STATE[m.syncState].label.toLowerCase() : 'not streaming'
  const lag =
    m.lagSeconds === null && m.lagBytes === null
      ? 'lag not known'
      : `${formatLag(m.lagSeconds, m.lagBytes)} behind`
  return `${safeText(serverId, 60)}: ${lag}, ${sync}`
}
</script>

<template>
  <TaskActionDialog
    :title="config.title"
    :confirm-label="config.confirmLabel"
    :danger="config.danger"
    :type-to-confirm="config.typeToConfirm"
    :start="start"
    :validate="validate"
    :invalidate="[...(invalidate ?? []), keys.failover, keys.topology]"
    :confirm-disabled="startBlocked"
    wide
    @close="emit('close')"
  >
    <template #error="{ error }">
      <FailoverRefusal :error="error" :context="refusalContext" />
    </template>
    <template #paused>
      <p class="notice tone-warn" role="status" data-testid="failover-paused-note">
        <span aria-hidden="true">&#9650;</span>
        <span>
          <strong>The failover waits for you.</strong> Close this window: the progress card on the
          Servers and Replication pages shows what it needs and lets you decide.
        </span>
      </p>
    </template>
    <template #default>
      <template v-if="stage === 'choose'">
        <p>
          Choose how server
          <strong><SafeName :value="server.address" kind="text" :max="80" /></strong>
          is failed over. The plan comes next, and nothing changes before you confirm it.
        </p>
        <fieldset>
          <legend>Failover mode</legend>
          <label class="check"
            ><input v-model="mode" type="radio" value="graceful" /> Graceful: hand over without
            loss. The server must be live.</label
          >
          <p class="muted hint" data-testid="graceful-hint">
            The plan says whether this server version can do a planned switchover yet.
          </p>
          <label class="check"
            ><input v-model="mode" type="radio" value="hard" /> Hard: the server is down or not
            answering. Commits that reached no standby can be lost.</label
          >
        </fieldset>
        <p v-if="mode === 'hard'" class="notice tone-danger" role="alert">
          <span aria-hidden="true">&#10006;</span>
          <span>
            Hard failover marks
            <strong><SafeName :value="server.address" kind="text" :max="80" /></strong> as dead. Its
            primary instances are replaced by their standbys, and their former neighbours carry
            double write load until you restore the preferred primaries.
            <template v-if="server.status === 'live'"
              >This server still answers, so it is told to stop its primaries before a standby is
              promoted.</template
            >
          </span>
        </p>
        <p v-else>
          The primaries on
          <strong><SafeName :value="server.address" kind="text" :max="80" /></strong> hand over to
          their standbys. The server stays in the cluster as a standby.
        </p>

        <p v-if="topology.isPending.value" role="status">Reading the shard groups ...</p>
        <p
          v-else-if="owned.length === 0"
          class="notice tone-info"
          role="status"
          data-testid="no-groups"
        >
          <span aria-hidden="true">&#9432;</span>
          <span
            >This server is the primary of no shard group, so there is nothing to fail over.</span
          >
        </p>
        <fieldset v-else class="stack" data-testid="failover-groups">
          <legend>Groups to fail over</legend>
          <div
            v-for="g in owned"
            :key="g.id"
            class="stack group-choice"
            :data-testid="`choice-${g.id}`"
          >
            <label class="check">
              <input
                type="checkbox"
                :checked="!unticked[g.id]"
                @change="unticked[g.id] = !($event.target as HTMLInputElement).checked"
              />
              {{ groupTitle(g.id, g.isMapGroup) }}
              <StatusBadge
                v-if="g.isMapGroup"
                :meta="{ tone: 'warn', icon: '▲', label: 'Moves the manager of the cluster' }"
              />
            </label>
            <FormField
              v-slot="slot"
              :label="`Standby to promote in ${groupTitle(g.id, g.isMapGroup)}`"
              :hint="g.standbys.length === 0 ? 'This group has no standby.' : undefined"
            >
              <select
                v-bind="slot"
                v-model="target[g.id]"
                :disabled="unticked[g.id] || g.standbys.length === 0"
              >
                <option value="">The one that holds the most WAL (recommended)</option>
                <option v-for="m in g.standbys" :key="m.serverId" :value="m.serverId">
                  {{ standbyText(g, m.serverId) }}
                </option>
              </select>
            </FormField>
          </div>
        </fieldset>

        <FormField
          v-slot="slot"
          label="Comment (optional)"
          hint="Recorded in the audit log, up to 500 characters."
        >
          <input v-bind="slot" v-model="comment" class="input" maxlength="500" autocomplete="off" />
        </FormField>
        <FailoverRefusal v-if="planError" :error="planError" :context="refusalContext" />
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
        <FailoverPlanTable :plan="plan" />
        <label v-if="loss" class="check notice tone-warn" data-testid="accept-loss">
          <input v-model="acceptLoss" type="checkbox" />
          <span>
            <strong>I accept that data may be lost.</strong> Commits that the plan says can be lost
            are not recovered after the failover.
          </span>
        </label>
        <p v-if="stuck.length > 0" class="muted" data-testid="plan-stuck">
          The failover cannot start while the blockers above stand. Change the choice, or wait until
          they are gone.
        </p>
      </template>
    </template>
  </TaskActionDialog>
</template>

<style scoped>
.group-choice {
  gap: var(--s-1);
}
fieldset {
  margin: 0;
}
</style>
