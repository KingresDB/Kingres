<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Changes one PostgreSQL parameter (design of phase 13, "Edit dialog"): the level and its scope, a value checked as it
// is typed by the same rules as the server, a preview of what the change does on every server (asked of the server,
// debounced), then the task with a state per server and a Retry. The same dialog reverts a change (the earlier value is
// put back, previewed like any change) and sets the time zone. Nothing is restarted by a change: a setting that needs a
// restart is shown as pending, and the restart is a decision of its own.
import { computed, reactive, ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import ConfigValue from './ConfigValue.vue'
import { endpoints } from '@/api/endpoints'
import {
  keys,
  useConfigServers,
  useConfigSummary,
  useDatabases,
  useServerGroups,
  useServers,
  useTimeZones,
} from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import { useSessionStore } from '@/stores/session'
import { checkConfigValue, describeSpec, specOf, typableUnits } from '@/lib/config-value'
import {
  CONFIG_SERVER_STATE,
  DOWNTIME_NOTE,
  LEVEL_LABEL,
  changeSentence,
  levelWords,
  takesEffectWords,
} from '@/lib/config'
import { safeText } from '@/lib/safe-text'
import type {
  ConfigServerState,
  ConfigChange,
  ConfigChangePreview,
  ConfigChangeRequest,
  ConfigLevel,
  ConfigParameter,
} from '@/api/types'

const props = defineProps<{
  /** The parameter. Looked up again here so that the dialog has its metadata and current overrides. */
  name: string
  /** Set to the same value at the cluster level (log_timezone goes with TimeZone). */
  alsoNames?: string[]
  /** Start at this level, scope and value; `lockLevel` keeps the person from moving to another level. */
  preset?: {
    level?: ConfigLevel
    scopeId?: string | null
    value?: string | null
    lockLevel?: boolean
  }
  /** Put the value of this change back (a new change, previewed like any other). */
  revert?: ConfigChange | null
  title?: string
}>()
const emit = defineEmits<{ close: [] }>()

const session = useSessionStore()
const LEVELS: ConfigLevel[] = ['cluster', 'serverGroup', 'server', 'database']

const parameter = useQuery({
  queryKey: computed(() => [...keys.config, 'parameter', props.name]),
  queryFn: () => endpoints.getConfigParameter(props.name),
})
const p = computed<ConfigParameter | null>(() => parameter.data.value ?? null)
const groups = useServerGroups()
const servers = useServers()
const databases = useDatabases()
const zones = useTimeZones()
const isZone = computed(() => p.value?.name === 'TimeZone' || p.value?.name === 'log_timezone')

// --- what the person chooses -----------------------------------------------------------------------------

const form = reactive({
  level: (props.revert?.level ?? props.preset?.level ?? 'cluster') as ConfigLevel,
  scopeId: props.revert?.scopeId ?? props.preset?.scopeId ?? '',
  value: props.preset?.value ?? '',
  remove: props.revert ? props.revert.oldValue === null : props.preset?.value === null,
})
const locked = computed(() => props.revert !== undefined && props.revert !== null)
const levelLocked = computed(() => locked.value || props.preset?.lockLevel === true)
let primed = false
watch(
  p,
  (param) => {
    if (!param || primed) return
    primed = true
    if (props.revert) {
      form.value = props.revert.oldValue ?? ''
      return
    }
    if (props.preset?.value === undefined || props.preset?.value === '') {
      // Read from the parameter itself: this runs at once when the parameter is already cached, before the
      // computed values below exist.
      const here = param.overrides.find(
        (o) =>
          o.level === form.level &&
          (o.scopeId ?? '') === (form.level === 'cluster' ? '' : form.scopeId),
      )?.value
      form.value = here ?? param.servers.find((s) => s.value !== null)?.value ?? ''
    }
    if (!param.allowedLevels.includes(form.level)) form.level = param.allowedLevels[0] ?? 'cluster'
  },
  { immediate: true },
)

const scopeOptions = computed<{ id: string; label: string }[]>(() => {
  const label = (s: string): string => safeText(s, 80)
  if (form.level === 'serverGroup')
    return (groups.data.value?.items ?? []).map((g) => ({ id: g.name, label: label(g.name) }))
  if (form.level === 'server')
    return (servers.data.value?.items ?? []).map((x) => ({ id: x.id, label: label(x.id) }))
  if (form.level === 'database')
    return (databases.data.value?.items ?? []).map((d) => ({ id: d.name, label: label(d.name) }))
  return []
})
// A level that needs a scope starts at the first one that exists.
watch(
  [() => form.level, scopeOptions],
  () => {
    if (form.level === 'cluster') {
      form.scopeId = ''
      return
    }
    // The scopes arrive after the dialog opened: a scope that was asked for stays until the list is there.
    if (scopeOptions.value.length > 0 && !scopeOptions.value.some((o) => o.id === form.scopeId))
      form.scopeId = scopeOptions.value[0]?.id ?? ''
  },
  { immediate: true },
)

const SCOPE_LABEL: Record<ConfigLevel, string> = {
  cluster: '',
  serverGroup: 'Server group',
  server: 'Server',
  database: 'Database',
}

function levelReason(level: ConfigLevel): string {
  const param = p.value
  if (!param || param.allowedLevels.includes(level)) return ''
  if (param.clusterOnly && (level === 'serverGroup' || level === 'server'))
    return 'This setting must be equal on every server, so it is set for the cluster.'
  if (level === 'database') return 'PostgreSQL has no database level for this setting.'
  return 'This setting cannot be set at this level.'
}

/** The value set at the chosen level and scope, if any. */
const currentOverride = computed(() =>
  p.value?.overrides.find(
    (o) =>
      o.level === form.level &&
      (o.scopeId ?? '') === (form.level === 'cluster' ? '' : form.scopeId),
  ),
)

// --- checking the value as it is typed --------------------------------------------------------------------

const unitChoices = computed(() => typableUnits(p.value?.unit ?? null))
const typedUnit = computed(() => {
  const m = /^[+-]?\d+(?:\.\d+)? ?([A-Za-z]+)$/.exec(form.value)
  return m && unitChoices.value.includes(m[1] as string) ? (m[1] as string) : ''
})
function pickUnit(unit: string): void {
  const m = /^([+-]?\d+(?:\.\d+)?) ?[A-Za-z]*$/.exec(form.value)
  if (m) form.value = `${m[1]}${unit}`
}

const verdict = computed<{ ok: boolean; message: string; normalized: string }>(() => {
  const param = p.value
  if (!param) return { ok: false, message: '', normalized: '' }
  if (form.remove) return { ok: true, message: '', normalized: '' }
  if (isZone.value) {
    if (form.value.trim() === '')
      return { ok: false, message: 'Choose a time zone.', normalized: '' }
    const list = zones.data.value?.items
    const known = list?.find((z) => z.name.toLowerCase() === form.value.toLowerCase())
    if (list && !known)
      return {
        ok: false,
        message: 'PostgreSQL does not know this time zone: pick one from the list.',
        normalized: '',
      }
    return { ok: true, message: '', normalized: known?.name ?? form.value }
  }
  const r = checkConfigValue(form.value, specOf(param))
  return r.ok
    ? { ok: true, message: '', normalized: r.normalized }
    : { ok: false, message: r.message, normalized: '' }
})

// --- applying and following ---------------------------------------------------------------------------------

let sent: ConfigChangeRequest | null = null
const action = useTaskAction<'first' | 'retry'>(
  (how) => {
    if (how === 'first' && props.revert) return endpoints.revertConfigChange(props.revert.changeId)
    return endpoints.applyConfigChange((sent ?? request.value) as ConfigChangeRequest)
  },
  {
    invalidate: () => [
      keys.config,
      keys.configServers,
      keys.configSummary,
      keys.configTime,
      keys.servers,
    ],
    label: () => props.title ?? `Change ${props.name}`,
  },
)
const started = computed(() => action.task.value !== null)
const configServers = useConfigServers(started)
async function apply(how: 'first' | 'retry'): Promise<void> {
  if (how === 'first') sent = request.value
  await action.run(how)
}

// --- the preview ---------------------------------------------------------------------------------------------

// The configuration revision the person saw when the dialog opened (the first answer fetched after it opened, never an
// older cached one): the apply carries it, and the server refuses (409 config_change_superseded) when someone changed
// a value of the request after it. A Retry resends the same request, so it carries the same revision and is accepted
// when the first attempt was recorded after all, never when somebody else changed the value meanwhile (P13R-11).
const openedAt = Date.now()
const summary = useConfigSummary()
const seenRevision = ref<number | null>(null)
watch(
  () => [summary.data.value?.revision, summary.dataUpdatedAt.value] as const,
  ([rev, at]) => {
    if (seenRevision.value === null && rev !== undefined && at >= openedAt) seenRevision.value = rev
  },
  { immediate: true },
)

const request = computed<ConfigChangeRequest | null>(() => {
  const param = p.value
  if (!param || !verdict.value.ok) return null
  if (form.level !== 'cluster' && form.scopeId === '') return null
  const value = form.remove ? null : verdict.value.normalized
  const names = [param.name, ...(form.level === 'cluster' ? (props.alsoNames ?? []) : [])]
  const expectedRevision = seenRevision.value
  return {
    changes: names.map((name) => ({
      name,
      level: form.level,
      scopeId: form.level === 'cluster' ? null : form.scopeId,
      value,
    })),
    ...(expectedRevision === null ? {} : { expectedRevision }),
  }
})

const preview = ref<ConfigChangePreview | null>(null)
const previewError = ref<unknown>(null)
const previewing = ref(false)
let previewTimer: ReturnType<typeof setTimeout> | undefined
let previewSeq = 0
const canPreview = computed(() => session.can('previewConfigChange'))

watch(
  request,
  (req) => {
    clearTimeout(previewTimer)
    preview.value = null
    previewError.value = null
    if (!req || !canPreview.value || started.value) {
      previewing.value = false
      return
    }
    previewing.value = true
    const seq = ++previewSeq
    previewTimer = setTimeout(async () => {
      try {
        const result = await endpoints.previewConfigChange(req)
        if (seq === previewSeq) preview.value = result
      } catch (e) {
        if (seq === previewSeq) previewError.value = e
      } finally {
        if (seq === previewSeq) previewing.value = false
      }
    }, 350)
  },
  { deep: true, immediate: true },
)

const changing = computed(
  () =>
    new Set(
      (preview.value?.effects ?? [])
        .filter((e) => e.takesEffect !== 'unchanged')
        .map((e) => e.serverId),
    ).size,
)
const total = computed(
  () =>
    servers.data.value?.items.length ??
    new Set((preview.value?.effects ?? []).map((e) => e.serverId)).size,
)
const sentence = computed(() =>
  changeSentence(changing.value, total.value, preview.value?.restartServerIds.length ?? 0),
)
// Apply waits for the revision the person saw (P13S-07): a request without it would overwrite a change nobody told them of.
// When the summary cannot be read at all, the request goes without it and the last request wins, as before.
const revisionKnown = computed(() => seenRevision.value !== null || summary.isError.value)
const canApply = computed(
  () =>
    preview.value !== null &&
    preview.value.valid &&
    !previewing.value &&
    changing.value > 0 &&
    (props.revert !== undefined || revisionKnown.value),
)

const task = computed(() => action.task.value)
/** What the task says about a server's step: done, or still working (and whether it waits for the server). */
function stepOf(serverId: string): { done: boolean; waiting: boolean } {
  const t = task.value
  const step = t?.progress.steps.find((s) => s.name === `Apply on server ${serverId}`)
  if (!t || !step) return { done: t?.state === 'done' || t?.state === 'failed', waiting: false }
  return {
    done: step.state === 'done' || step.state === 'failed' || t.state === 'done',
    waiting: step.state === 'running' && /waiting/i.test(step.detail ?? ''),
  }
}
/** The parameters this dialog changes: what waits for a restart for another reason is not part of this change. */
const touched = computed(
  () => new Set([props.name, ...(props.alsoNames ?? [])].map((n) => n.toLowerCase())),
)
const rows = computed(() =>
  (configServers.data.value?.items ?? []).map((s) => {
    const pending = s.pendingRestart.filter((n) => touched.value.has(n.toLowerCase()))
    const state: ConfigServerState =
      s.state === 'failed' || s.state === 'waiting'
        ? s.state
        : pending.length > 0
          ? 'pendingRestart'
          : 'applied'
    return { s, pending, state, ...stepOf(s.serverId) }
  }),
)
const settled = computed(
  () => task.value !== null && task.value.state !== 'running' && task.value.state !== 'pending',
)
const needsRetry = computed(
  () => settled.value && rows.value.some((r) => r.s.state === 'failed' || r.s.state === 'waiting'),
)
const pendingNames = computed(() => [...new Set(rows.value.flatMap((r) => r.pending))])

async function cancelTask(): Promise<void> {
  if (task.value) await endpoints.cancelTask(task.value.id)
}

const nameOf = (id: string): string => safeText(id, 80)
const valueId = 'cfg-value'
</script>

<template>
  <AppDialog
    :title="title ?? (revert ? `Revert the change of ${name}` : `Edit ${name}`)"
    wide
    :persistent="action.isStarting.value"
    @close="emit('close')"
  >
    <p v-if="parameter.isPending.value" role="status" class="muted">Loading the parameter.</p>
    <ProblemNotice
      v-else-if="parameter.error.value"
      :error="parameter.error.value"
      @retry="parameter.refetch()"
    />
    <template v-else-if="p && !started">
      <p class="muted">
        <SafeName :value="p.description" kind="text" :max="300" />
        <template v-if="p.restartRequired"> A change takes effect after a restart.</template>
      </p>

      <!-- The level and its scope -->
      <template v-if="revert">
        <p data-testid="revert-summary">
          Put the value of <code><SafeName :value="p.name" kind="text" :max="80" /></code> at the
          {{ levelWords(revert.level) }} level<template v-if="revert.scopeId !== null">
            (<SafeName :value="revert.scopeId" kind="text" :max="80" />)</template
          >
          back to
          <template v-if="revert.oldValue !== null"
            ><ConfigValue :value="revert.oldValue"
          /></template>
          <template v-else>no override</template>
          (it is
          <template v-if="revert.newValue !== null"
            ><ConfigValue :value="revert.newValue" /></template
          ><template v-else>not set</template> now). This is a new change: it is checked and applied
          like any other, and the history keeps both.
        </p>
      </template>
      <template v-else>
        <fieldset class="levels">
          <legend>Level</legend>
          <div v-for="l in LEVELS" :key="l" class="level">
            <label class="check"
              ><input
                v-model="form.level"
                type="radio"
                name="cfg-level"
                :value="l"
                :disabled="!p.allowedLevels.includes(l) || (levelLocked && form.level !== l)"
                :aria-describedby="levelReason(l) ? `cfg-level-reason-${l}` : undefined"
              />
              {{ LEVEL_LABEL[l] }}</label
            >
            <span v-if="levelReason(l)" :id="`cfg-level-reason-${l}`" class="hint muted">{{
              levelReason(l)
            }}</span>
          </div>
        </fieldset>
        <div v-if="form.level !== 'cluster'" class="field">
          <label for="cfg-scope">{{ SCOPE_LABEL[form.level] }}</label>
          <select
            id="cfg-scope"
            v-model="form.scopeId"
            :disabled="levelLocked && preset?.scopeId !== undefined"
          >
            <option v-for="o in scopeOptions" :key="o.id" :value="o.id">{{ o.label }}</option>
          </select>
          <span v-if="scopeOptions.length === 0" class="hint">There is none to choose from.</span>
        </div>
        <p v-if="currentOverride" class="row" data-testid="current-override">
          <span
            >Set here now: <ConfigValue :value="currentOverride.value" /> (revision
            {{ currentOverride.revision }}).</span
          >
          <button
            v-if="!form.remove"
            type="button"
            class="btn btn-small"
            @click="form.remove = true"
          >
            Remove this override
          </button>
          <button v-else type="button" class="btn btn-small" @click="form.remove = false">
            Keep the override
          </button>
        </p>
        <p v-else-if="form.remove" class="muted">
          Nothing is set at this level, so there is nothing to remove.
        </p>

        <!-- The value -->
        <div v-if="!form.remove" class="field">
          <label :for="valueId">{{ isZone ? 'Time zone' : 'Value' }}</label>
          <template v-if="p.type === 'bool'">
            <select
              :id="valueId"
              v-model="form.value"
              :aria-invalid="verdict.ok ? undefined : 'true'"
              aria-describedby="cfg-value-help cfg-value-msg"
            >
              <option value="on">on</option>
              <option value="off">off</option>
            </select>
          </template>
          <template v-else-if="p.type === 'enum'">
            <select
              :id="valueId"
              v-model="form.value"
              :aria-invalid="verdict.ok ? undefined : 'true'"
              aria-describedby="cfg-value-help cfg-value-msg"
            >
              <option v-for="e in p.enumValues ?? []" :key="e" :value="e">
                {{ safeText(e, 80) }}
              </option>
            </select>
          </template>
          <template v-else>
            <div class="row">
              <input
                :id="valueId"
                v-model="form.value"
                class="input mono"
                type="text"
                spellcheck="false"
                autocomplete="off"
                :list="isZone ? 'cfg-zones' : undefined"
                :aria-invalid="verdict.ok ? undefined : 'true'"
                aria-describedby="cfg-value-help cfg-value-msg"
              />
              <select
                v-if="unitChoices.length > 0"
                aria-label="Unit"
                :value="typedUnit"
                @change="pickUnit(($event.target as HTMLSelectElement).value)"
              >
                <option value="" disabled>unit</option>
                <option v-for="u in unitChoices" :key="u" :value="u">{{ u }}</option>
              </select>
            </div>
            <datalist v-if="isZone" id="cfg-zones">
              <option v-for="z in zones.data.value?.items ?? []" :key="z.name" :value="z.name">
                {{ z.utcOffset }}
              </option>
            </datalist>
          </template>
          <span id="cfg-value-help" class="hint"
            >{{ describeSpec(specOf(p)) }}
            <template v-if="p.defaultValue !== null"
              >Default: <ConfigValue :value="p.defaultValue" />.</template
            ></span
          >
          <span
            id="cfg-value-msg"
            class="error"
            role="status"
            aria-live="polite"
            data-testid="value-message"
            >{{ verdict.message }}</span
          >
        </div>
      </template>

      <!-- The preview -->
      <section
        v-if="canPreview"
        aria-label="What this change does"
        class="stack"
        data-testid="preview"
      >
        <p v-if="previewing" class="muted" role="status">Checking with the servers.</p>
        <ProblemNotice v-if="previewError" :error="previewError" />
        <template v-if="preview">
          <div
            v-if="!preview.valid"
            class="notice tone-danger"
            role="alert"
            data-testid="preview-problems"
          >
            <span aria-hidden="true">&#10006;</span>
            <ul class="plain">
              <li v-for="pr in preview.problems" :key="`${pr.index}-${pr.pointer}-${pr.code}`">
                <SafeName :value="pr.message" kind="text" :max="400" />
              </li>
            </ul>
          </div>
          <template v-else>
            <p data-testid="preview-sentence">{{ sentence }}</p>
            <TableWrap>
              <table class="table">
                <caption class="sr-only">
                  Effect of the change on every server
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Server</th>
                    <th v-if="new Set(preview.effects.map((e) => e.name)).size > 1" scope="col">
                      Parameter
                    </th>
                    <th scope="col">Before</th>
                    <th scope="col">After</th>
                    <th scope="col">How</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="(e, i) in preview.effects"
                    :key="i"
                    :data-testid="`effect-${e.serverId}`"
                  >
                    <th scope="row"><SafeName :value="e.serverId" kind="text" :max="60" /></th>
                    <td v-if="new Set(preview.effects.map((x) => x.name)).size > 1">
                      <code><SafeName :value="e.name" kind="text" :max="60" /></code>
                    </td>
                    <td><ConfigValue :value="e.before" /></td>
                    <td><ConfigValue :value="e.after" /></td>
                    <td>
                      <span
                        class="badge"
                        :class="
                          e.takesEffect === 'restart'
                            ? 'tone-warn'
                            : e.takesEffect === 'waits'
                              ? 'tone-info'
                              : 'tone-neutral'
                        "
                        ><span aria-hidden="true">{{
                          e.takesEffect === 'restart'
                            ? '▲'
                            : e.takesEffect === 'waits'
                              ? '◐'
                              : e.takesEffect === 'reload'
                                ? '↻'
                                : '○'
                        }}</span>
                        {{ takesEffectWords(e.takesEffect) }}</span
                      >
                    </td>
                  </tr>
                </tbody>
              </table>
            </TableWrap>
            <div
              v-if="(preview.warnings ?? []).length > 0"
              class="notice tone-warn"
              role="status"
              data-testid="preview-warnings"
            >
              <span aria-hidden="true">&#9650;</span>
              <ul class="plain">
                <li
                  v-for="w in preview.warnings"
                  :key="`${w.index}-${w.pointer}-${w.code}`"
                  :data-code="w.code"
                >
                  <SafeName :value="w.message" kind="text" :max="400" />
                </li>
              </ul>
            </div>
            <div
              v-if="preview.restartServerIds.length > 0"
              class="notice tone-warn"
              data-testid="restart-note"
            >
              <span aria-hidden="true">&#9650;</span>
              <div class="stack">
                <span
                  >Nothing is restarted by this change. A restart is needed on
                  <template v-for="(id, i) in preview.restartServerIds" :key="id"
                    ><SafeName :value="id" kind="text" :max="60" /><template
                      v-if="i < preview.restartServerIds.length - 1"
                      >,
                    </template></template
                  >.</span
                >
                <span>{{ DOWNTIME_NOTE }}</span>
                <span v-if="preview.restartOrderConflict" data-testid="restart-conflict"
                  ><strong>No safe restart order.</strong>
                  <SafeName :value="preview.restartOrderConflict" kind="text" :max="400"
                /></span>
                <ol
                  v-else-if="preview.restartOrder.length > 0"
                  class="order"
                  data-testid="restart-order"
                >
                  <li
                    v-for="step in preview.restartOrder"
                    :key="`${step.serverId}-${step.instanceId}`"
                  >
                    Instance {{ step.instanceId }} ({{ step.role }}) on
                    <SafeName :value="step.serverId" kind="text" :max="60" />
                  </li>
                </ol>
              </div>
            </div>
            <p v-if="changing === 0" class="muted">No server would change.</p>
          </template>
        </template>
      </section>
      <p v-else class="muted">
        The preview is for administrators; you can read the current values here.
      </p>
      <ProblemNotice v-if="action.error.value" :error="action.error.value" />
    </template>

    <!-- After Apply: the task and one line per server -->
    <template v-else-if="p && started">
      <TaskProgress :task="task" :poll-error="action.pollError.value" />
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            State of every server
          </caption>
          <thead>
            <tr>
              <th scope="col">Server</th>
              <th scope="col">State</th>
              <th scope="col">Detail</th>
              <th scope="col">Action</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in rows" :key="r.s.serverId" :data-testid="`server-state-${r.s.serverId}`">
              <th scope="row"><SafeName :value="r.s.serverId" kind="text" :max="60" /></th>
              <td>
                <span v-if="!r.done" class="badge" :class="r.waiting ? 'tone-info' : 'tone-neutral'"
                  ><span aria-hidden="true">{{ r.waiting ? '◐' : '○' }}</span>
                  {{ r.waiting ? 'Waiting for the server' : 'Not yet applied' }}</span
                >
                <StatusBadge v-else :meta="CONFIG_SERVER_STATE[r.state]" />
              </td>
              <td>
                <template v-if="r.done">
                  <ul v-if="r.s.failed.length" class="plain">
                    <li v-for="f in r.s.failed" :key="f.name">
                      <code><SafeName :value="f.name" kind="text" :max="60" /></code>:
                      <SafeName :value="f.error" kind="text" :max="300" multiline />
                    </li>
                  </ul>
                  <span v-else-if="r.state === 'pendingRestart'">
                    Waits for a restart:
                    <template v-for="(n, i) in r.pending" :key="n"
                      ><code><SafeName :value="n" kind="text" :max="60" /></code
                      ><template v-if="i < r.pending.length - 1">, </template></template
                    >.
                  </span>
                  <span v-else-if="r.s.state === 'waiting' && r.s.detail">
                    <SafeName :value="r.s.detail" kind="text" :max="300" />
                  </span>
                  <span v-else-if="r.state === 'applied'">Running the new value.</span>
                </template>
                <span v-else-if="r.waiting"
                  >This server cannot be reached; it gets the change when it returns (the change is
                  not lost).</span
                >
              </td>
              <td>
                <button
                  v-if="needsRetry && (r.s.state === 'failed' || r.s.state === 'waiting')"
                  type="button"
                  class="btn btn-small"
                  :disabled="action.isBusy.value"
                  :aria-label="`Retry on ${nameOf(r.s.serverId)}`"
                  @click="apply('retry')"
                >
                  Retry
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <p
        v-if="task?.state === 'done' && pendingNames.length === 0"
        class="notice tone-ok"
        role="status"
      >
        <span aria-hidden="true">&#10003;</span> Applied on every server.
      </p>
      <p v-else-if="task?.state === 'done'" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span> Applied. Some servers wait for a restart; nothing
        was restarted.
      </p>
      <p v-else-if="task?.state === 'failed' && task.error" class="notice tone-danger" role="alert">
        <span aria-hidden="true">&#10006;</span>
        <span><SafeName :value="task.error.message" kind="text" :max="600" multiline /></span>
      </p>
      <div v-if="task?.cancellable" class="row">
        <GateButton op="cancelTask" small @click="cancelTask">Cancel the task</GateButton>
        <span class="muted"
          >The change stays recorded; the servers not reached wait and can be retried.</span
        >
      </div>
      <ProblemNotice v-if="action.error.value" :error="action.error.value" />
    </template>

    <template #footer>
      <template v-if="!started">
        <button
          type="button"
          class="btn"
          :disabled="action.isStarting.value"
          @click="emit('close')"
        >
          Cancel
        </button>
        <GateButton
          :op="revert ? 'revertConfigChange' : 'applyConfigChange'"
          variant="primary"
          :disabled="!canApply || action.isStarting.value"
          :blocked-reason="
            canApply
              ? ''
              : 'Apply is available when the servers have checked the change and it would change something.'
          "
          @click="apply('first')"
        >
          {{ revert ? 'Revert' : 'Apply' }}
        </GateButton>
      </template>
      <button v-else type="button" class="btn btn-primary" @click="emit('close')">
        {{ action.isActive.value ? 'Hide (keeps running)' : 'Close' }}
      </button>
    </template>
  </AppDialog>
</template>

<style scoped>
.levels {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: var(--s-2) var(--s-4);
}
.level {
  display: flex;
  flex-direction: column;
}
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
}
.order {
  margin: 0;
  padding-left: 1.4em;
}
</style>
