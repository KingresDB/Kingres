<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The progress of a failover (design 8.3), shown on the Servers and Replication pages for as long as a run is going on,
// waits for a person or has failed. It reads `GET /failover` (it also works while the manager of the cluster is moved
// and the run has no task yet) and the task. Per group: where it is moving to, its state, the phase, how long writes
// have been paused, the wait for the old primary's lease. The steps of the task come in one block per group. A run that
// waits for an operator says what it needs and offers "Accept data loss and continue" (a repeat of the request with
// `allowDataLoss`, built from the options the run was started with) and "Cancel". Cancel is disabled with the reason once
// a standby has been promoted. Nothing here is drawn from a server string without SafeName.
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import SafeName from './SafeName.vue'
import StatusBadge from './StatusBadge.vue'
import GateButton from './GateButton.vue'
import TaskProgress from './TaskProgress.vue'
import ProblemNotice from './ProblemNotice.vue'
import DataLossText from './DataLossText.vue'
import FailoverReportDialog from './FailoverReportDialog.vue'
import FailoverRefusal from './FailoverRefusal.vue'
import ConfirmDialog from './ConfirmDialog.vue'
import { endpoints } from '@/api/endpoints'
import { keys, POLL, useFailoverReport, useFailoverStatus } from '@/api/queries'
import { useAnnounce } from '@/composables/useAnnounce'
import {
  blockerLabel,
  groupStateMeta,
  groupTitle,
  KIND_LABEL,
  mapGroupPauseHint,
  phaseLabel,
  runStateMeta,
  TRIGGER_LABEL,
} from '@/lib/failover'
import { formatDateTime, formatDuration } from '@/lib/format'
import { useSessionStore } from '@/stores/session'
import { ApiError } from '@/api/problem'
import type { FailoverPlan, FailoverRequest } from '@/api/types'

const session = useSessionStore()
const client = useQueryClient()
const announce = useAnnounce()
const canRead = computed(() => session.can('getFailoverStatus'))
const status = useFailoverStatus(canRead)
const run = computed(() => status.data.value ?? null)
const shown = computed(() => run.value !== null && run.value.state !== 'idle')
const taskId = computed(() => (shown.value ? (run.value?.taskId ?? null) : null))
const task = useQuery({
  queryKey: computed(() => keys.task(taskId.value ?? -1)),
  queryFn: () => endpoints.getTask(taskId.value as number),
  enabled: computed(() => taskId.value !== null),
  refetchInterval: POLL.fast,
})
const paused = computed(() => run.value?.state === 'paused')
// The options a paused run was started with are in its report; the report of a paused run is what the panel reads to
// repeat the request. A guess at the options would be unsafe (a graceful request ignores `allowDataLoss`).
const report = useFailoverReport(() => (paused.value ? taskId.value : null))
const reportOpen = ref(false)
const deciding = ref(false)

// How long writes of a group have been paused, counted from the time the server gave.
const now = ref(Date.now())
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  timer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
})
onBeforeUnmount(() => {
  if (timer !== undefined) clearInterval(timer)
})
function pausedFor(since: string | null | undefined): number | null {
  if (!since) return null
  const t = Date.parse(since)
  return Number.isNaN(t) ? null : Math.max(0, Math.round((now.value - t) / 1000))
}

const title = computed(() => {
  const r = run.value
  if (!r) return ''
  if (r.kind === 'restorePreferredPrimaries') return 'Restoring the preferred primaries'
  return r.serverId ? `Failover of server ${r.serverId}` : 'Failover'
})
const phase = computed(() => phaseLabel(run.value?.phase))
const mapMoving = computed(() => run.value?.phase === 'bootstrap')
const movingTo = computed(
  () => run.value?.groups.find((g) => g.phase === 'bootstrap' || g.toServerId)?.toServerId ?? null,
)
const needing = computed(() => run.value?.needsDecision ?? [])
const canResume = computed(
  () =>
    paused.value && run.value?.kind === 'failover' && !!run.value.serverId && taskId.value !== null,
)
const cancelBlocked = computed(() =>
  taskId.value === null
    ? 'The run has no task yet, so there is nothing to cancel.'
    : run.value?.cancellable === false && run.value.state === 'running'
      ? 'A standby has been promoted; the failover can only go forward.'
      : run.value?.cancellable === false
        ? 'This run cannot be cancelled now.'
        : '',
)
/** The assessments of the groups that wait for a decision, from the report of the paused run. */
// 0.12.0: the refusal of a repeat carries the plan, whose verdicts are newer than the report's: they win when present.
const resumePlan = ref<FailoverPlan | null>(null)
const assessments = computed(() =>
  (report.data.value?.groups ?? [])
    .filter((g) => needing.value.some((b) => b.shardGroupId === g.shardGroupId))
    .map((g) => {
      const fresh = resumePlan.value?.groups.find((p) => p.shardGroupId === g.shardGroupId)
      return fresh ? { ...g, dataLoss: fresh.dataLoss } : g
    }),
)

const cancel = useMutation({
  mutationFn: () => endpoints.cancelTask(taskId.value as number),
  onSettled: () =>
    Promise.all([
      client.invalidateQueries({ queryKey: keys.failover }),
      client.invalidateQueries({ queryKey: keys.tasks }),
      client.invalidateQueries({ queryKey: keys.topology }),
    ]),
})

// Giving up the bootstrap of the map group (contract 0.18.0). The candidate lets go of its lease, and for a planned switchover the old leader rolls back; a
// bootstrap that promoted the standby cannot be given up (the server answers a conflict), and none in progress answers abandoned: false.
const abandonAsked = ref(false)
// Contract 0.28.0: the operator may declare that the candidate server is lost. Off by default; the dialog's warning says what it means.
const declareLost = ref(false)
const abandonNote = ref('')
const abandonError = ref<unknown>(null)
const abandon = useMutation({
  mutationFn: () =>
    endpoints.abandonMapGroupBootstrap(
      'given up from the console',
      declareLost.value && movingTo.value ? movingTo.value : undefined,
    ),
  onSuccess: (r) => {
    abandonError.value = null
    const resumed = r.resumedServerId
      ? ` The primary of the map group on ${r.resumedServerId} is started again.`
      : ''
    abandonNote.value = r.abandoned
      ? `The bootstrap was given up.${resumed}`
      : `No bootstrap of the map group is in progress.${resumed}`
  },
  onError: (e) => {
    abandonError.value = e
  },
  onSettled: () => {
    abandonAsked.value = false
    declareLost.value = false
    return Promise.all([
      client.invalidateQueries({ queryKey: keys.failover }),
      client.invalidateQueries({ queryKey: keys.topology }),
    ])
  },
})
const abandoning = computed(() => abandon.isPending.value)
function openAbandon(): void {
  declareLost.value = false
  abandonAsked.value = true
}
function closeAbandon(): void {
  declareLost.value = false
  abandonAsked.value = false
}

const resumeError = ref<unknown>(null)
const resuming = ref(false)
async function resume(): Promise<void> {
  const r = run.value
  if (!r?.serverId || taskId.value === null) return
  resuming.value = true
  resumeError.value = null
  try {
    const options = (await endpoints.getFailoverReport(taskId.value)).options
    const { dryRun: _dryRun, ...rest } = options as FailoverRequest
    void _dryRun
    await endpoints.failoverServer(r.serverId, { ...rest, allowDataLoss: true })
    deciding.value = false
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.failover }),
      client.invalidateQueries({ queryKey: keys.tasks }),
    ])
  } catch (e) {
    resumeError.value = e
    if (e instanceof ApiError && e.code === 'failover_needs_decision' && e.problem.plan)
      resumePlan.value = e.problem.plan
  } finally {
    resuming.value = false
  }
}

// What is read aloud: the start, each promotion, the end and a need for a decision.
const seen = new Set<string>()
watch(
  () => run.value,
  (r, before) => {
    if (!r) return
    if (!before) {
      // First answer: remember what is already there, say nothing about it.
      for (const g of r.groups)
        if (g.state === 'promoted' || g.state === 'done') seen.add(`g${g.shardGroupId}`)
      return
    }
    if (r.state === 'running' && before.state !== 'running' && before.state !== 'paused')
      announce(`${title.value} started.`)
    for (const g of r.groups)
      if ((g.state === 'promoted' || g.state === 'done') && !seen.has(`g${g.shardGroupId}`)) {
        seen.add(`g${g.shardGroupId}`)
        announce(`Shard group ${g.shardGroupId} has a new primary.`)
      }
    if (r.state === 'paused' && before.state !== 'paused')
      announce('The failover needs your decision.', 'assertive')
    if (r.state === 'failed' && before.state !== 'failed')
      announce('The failover failed.', 'assertive')
    if (r.state === 'idle' && (before.state === 'running' || before.state === 'paused')) {
      seen.clear()
      announce(`${title.value} has ended.`)
    }
  },
)
</script>

<template>
  <section
    v-if="shown && run"
    id="failover"
    class="card stack failover-progress"
    aria-labelledby="fp-h"
    data-testid="failover-progress"
    :data-state="run.state"
    style="margin-bottom: var(--s-4)"
  >
    <div class="row-between">
      <!-- eslint-disable-next-line vuejs-accessibility/heading-has-content -->
      <h2 id="fp-h"><SafeName :value="title" kind="text" :max="120" /></h2>
      <span class="row">
        <StatusBadge :meta="runStateMeta(run.state)" data-testid="failover-state" />
        <span v-if="run.trigger" class="badge tone-neutral" data-testid="failover-trigger">
          {{ TRIGGER_LABEL[run.trigger] ?? run.trigger }}
        </span>
      </span>
    </div>
    <p v-if="run.kind" class="muted">
      {{ KIND_LABEL[run.kind] ?? run.kind
      }}<template v-if="run.startedAt">, started {{ formatDateTime(run.startedAt) }}</template
      >.
    </p>
    <p v-if="mapMoving" class="notice tone-warn" role="status" data-testid="manager-moving">
      <span aria-hidden="true">&#9650;</span>
      <span>
        <strong
          >The manager of the cluster is moving<template v-if="movingTo">
            to server <SafeName :value="movingTo" kind="text" :max="60" /></template
          >.</strong
        >
        Until it has moved, changes that need the manager wait, and what is shown may be old.
      </span>
    </p>
    <p v-else-if="phase && run.state === 'running'" data-testid="failover-phase">{{ phase }}.</p>
    <div
      v-if="mapMoving && run.state === 'running'"
      class="row"
      data-testid="manager-moving-actions"
    >
      <GateButton
        op="abandonMapGroupBootstrap"
        data-testid="abandon-bootstrap"
        :blocked-reason="abandoning ? 'Giving up ...' : ''"
        @click="openAbandon"
      >
        Give up the move of the manager
      </GateButton>
      <span v-if="abandonNote" class="muted" role="status" data-testid="abandon-note">{{
        abandonNote
      }}</span>
    </div>
    <ProblemNotice v-if="abandonError" :error="abandonError" />
    <p v-if="taskId === null && run.state === 'running'" class="muted" data-testid="no-task-yet">
      The run has no task yet: it is recorded as soon as the system database is writable again.
    </p>

    <ul v-if="run.groups.length > 0" class="groups" data-testid="failover-groups-progress">
      <li v-for="g in run.groups" :key="g.shardGroupId" :data-testid="`fg-${g.shardGroupId}`">
        <div class="row">
          <strong>{{ groupTitle(g.shardGroupId, g.shardGroupId === 0) }}</strong>
          <StatusBadge :meta="groupStateMeta(g.state)" />
          <span class="muted">
            from <SafeName :value="g.fromServerId" kind="text" :max="60" /><template
              v-if="g.toServerId"
            >
              to <SafeName :value="g.toServerId" kind="text" :max="60"
            /></template>
          </span>
        </div>
        <p
          v-if="g.writesPausedSince && pausedFor(g.writesPausedSince) !== null"
          data-testid="writes-paused"
        >
          Writes to group {{ g.shardGroupId }} have been paused for
          {{ formatDuration(pausedFor(g.writesPausedSince)) }}.
        </p>
        <p v-if="g.message" class="muted" data-testid="group-message">
          <SafeName :value="g.message" kind="text" :max="300" />
        </p>
        <p v-else-if="g.phase === 'waitingForLease'" class="muted" data-testid="group-message">
          Waiting for the old primary's lease to run out.
        </p>
        <p v-if="mapGroupPauseHint(g.message)" data-testid="map-pause-hint">
          The old leader was stopped and cannot take the request. Repeat the failover request on
          server
          <SafeName :value="mapGroupPauseHint(g.message)!.server" kind="text" :max="80" />, which
          runs the bootstrap; or give the bootstrap up to start the old leader again.
        </p>
      </li>
    </ul>

    <div
      v-if="paused"
      class="notice tone-warn stack"
      role="group"
      aria-labelledby="fp-wait"
      data-testid="waiting-for-you"
    >
      <h3 id="fp-wait">Waiting for you</h3>
      <p v-if="run.pausedReason" data-testid="paused-reason">
        <SafeName :value="run.pausedReason" kind="text" :max="400" multiline />
      </p>
      <ul v-if="needing.length > 0" class="blockers" data-testid="needs-decision">
        <li v-for="(b, i) in needing" :key="`${i}-${b.shardGroupId}-${b.code}`">
          <strong
            >{{ blockerLabel(b)
            }}<template v-if="b.shardGroupId !== null"> (group {{ b.shardGroupId }})</template
            >.</strong
          >{{ ' ' }}
          <SafeName :value="b.message" kind="text" :max="300" />
        </li>
      </ul>
      <div
        v-for="a in assessments"
        :key="a.shardGroupId"
        class="stack"
        :data-testid="`decision-loss-${a.shardGroupId}`"
      >
        <DataLossText
          :data-loss="a.dataLoss"
          :group-id="a.shardGroupId"
          :at="report.data.value?.startedAt ?? null"
        />
      </div>
      <p v-if="!canResume" class="muted">
        This run cannot be continued from here: start it again from the Servers page, or cancel it.
      </p>
      <FailoverRefusal
        v-if="resumeError && !deciding"
        :error="resumeError"
        :context="{ mode: 'hard', includesMapGroup: false }"
      />
      <div class="row">
        <GateButton
          v-if="canResume"
          op="failoverServer"
          variant="danger"
          data-testid="accept-and-continue"
          :blocked-reason="
            report.isPending.value ? 'Reading what the run was started with ...' : ''
          "
          @click="deciding = true"
        >
          Accept data loss and continue
        </GateButton>
        <GateButton
          op="cancelTask"
          :blocked-reason="cancelBlocked"
          data-testid="cancel-failover"
          @click="cancel.mutate()"
        >
          Cancel
        </GateButton>
      </div>
    </div>
    <div v-else-if="run.state === 'running'" class="row">
      <GateButton
        op="cancelTask"
        :blocked-reason="cancelBlocked"
        data-testid="cancel-failover"
        @click="cancel.mutate()"
      >
        Cancel the failover
      </GateButton>
      <span v-if="cancelBlocked" class="muted" data-testid="cancel-reason">{{
        cancelBlocked
      }}</span>
    </div>
    <ProblemNotice v-if="cancel.error.value" :error="cancel.error.value" />

    <TaskProgress v-if="task.data.value" :task="task.data.value" :poll-error="task.error.value" />
    <p v-else-if="taskId !== null && task.isPending.value" role="status">Reading the task ...</p>

    <p v-if="run.state === 'failed'" class="muted">
      The run failed. The report says what was done for each group and what is left to do.
    </p>
    <div v-if="taskId !== null" class="row">
      <button
        type="button"
        class="btn btn-small"
        data-testid="open-report"
        @click="reportOpen = true"
      >
        Open the report
      </button>
    </div>
  </section>

  <ConfirmDialog
    v-if="deciding"
    title="Continue and accept data loss"
    confirm-label="Accept data loss and continue"
    danger
    :busy="resuming"
    @confirm="resume"
    @cancel="deciding = false"
  >
    <p>
      The failover goes on with the standby it chose. Commits that the plan says can be lost are
      <strong>not recovered</strong>.
    </p>
    <ul v-if="assessments.length > 0">
      <li v-for="a in assessments" :key="a.shardGroupId">
        {{ groupTitle(a.shardGroupId, a.shardGroupId === 0) }}:
        <SafeName :value="a.dataLoss.message" kind="text" :max="300" />
      </li>
    </ul>
    <FailoverRefusal
      v-if="resumeError"
      :error="resumeError"
      :context="{ mode: 'hard', includesMapGroup: false }"
    />
  </ConfirmDialog>
  <ConfirmDialog
    v-if="abandonAsked"
    title="Give up the move of the manager"
    confirm-label="Give up the move"
    danger
    :busy="abandoning"
    @confirm="abandon.mutate()"
    @cancel="closeAbandon"
  >
    <p>
      The standby that was to become the manager lets go of the lease, and the old manager takes
      over again. A move that has promoted the standby already cannot be given up: it goes on to its
      end.
    </p>
    <div v-if="movingTo" class="stack" data-testid="declare-lost">
      <label class="row">
        <input v-model="declareLost" type="checkbox" data-testid="declare-lost-check" />
        <span>Declare this server lost: <SafeName :value="movingTo" kind="text" :max="60" /></span>
      </label>
      <p
        v-if="declareLost"
        class="notice tone-danger"
        role="alert"
        data-testid="declare-lost-warning"
      >
        <span aria-hidden="true">&#10006;</span>
        <span>
          <strong>Only for a server that is really gone.</strong> The cluster then goes on without
          the word of this server. Whatever it promoted and acknowledged before it was lost is
          accepted as lost with this declaration, and it must never come back as a manager. If the
          server is only slow or cut off, do not declare it: wait, or give the move up without it.
          The server must not be heard by the cluster; otherwise the request is refused.
        </span>
      </p>
    </div>
  </ConfirmDialog>
  <FailoverReportDialog
    v-if="reportOpen && taskId !== null"
    :report-id="taskId"
    @close="reportOpen = false"
  />
</template>

<style scoped>
.groups {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
.blockers {
  margin: 0;
  padding-left: var(--s-4);
}
</style>
