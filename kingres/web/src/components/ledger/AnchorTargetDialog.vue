<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Adds or edits an anchor target (contract 0.11.0, design 6.2 to 6.5, 6.8 and 8.5): a form for each of the three kinds.
// Credentials are write-only: a field is empty when the dialog opens (also on an edit: what is stored is never read back),
// its value is held only while the dialog is open and is cleared after the request and when the dialog closes, and an
// empty field on an edit keeps what is stored. A lock mode that cannot be undone (compliance) is saved only after the word
// is typed, with the date on which the lock would end.
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import FormField from '@/components/FormField.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import TypeToConfirm from '@/components/TypeToConfirm.vue'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { keys } from '@/api/queries'
import type { LedgerAnchorTarget, LedgerAnchorTargetUpdate } from '@/api/types'
import {
  ANCHOR_KINDS,
  buildConfig,
  buildCredentials,
  COMPLIANCE_TEXT,
  CREDENTIAL_FIELDS,
  CREDENTIALS_TEXT,
  extraMembers,
  FIELDS,
  fieldOfServerError,
  GOVERNANCE_TEXT,
  initialValues,
  KIND_HELP,
  KIND_LABEL,
  needsComplianceConfirmation,
  OTS_WAIT_TEXT,
  PUBLIC_CALENDARS,
  retentionEnds,
  retentionOf,
  valuesFromConfig,
  validateForm,
  type AnchorKind,
  type FormValues,
} from '@/lib/ledger-anchor'

const props = defineProps<{ target: LedgerAnchorTarget | null }>()
const emit = defineEmits<{ close: []; saved: [target: LedgerAnchorTarget] }>()
const client = useQueryClient()

const editing = computed(() => props.target !== null)
const kind = ref<AnchorKind>((props.target?.kind as AnchorKind | undefined) ?? 'worm')
const name = ref(props.target?.name ?? '')
const enabled = ref(props.target?.enabled ?? false)
const values = reactive<FormValues>(
  props.target ? valuesFromConfig(kind.value, props.target.config) : initialValues(kind.value),
)
// What the form held when it opened: the configuration is sent only when a field was changed, so that the defaults of this
// form never rewrite a configuration that a person did not touch.
const opened = JSON.stringify(values)
const credentials = reactive<FormValues>({})
const typed = ref('')
const errors = ref<Record<string, string>>({})
const extra = computed(() => (props.target ? extraMembers(kind.value, props.target.config) : {}))

function chooseKind(k: AnchorKind): void {
  if (editing.value || k === kind.value) return
  kind.value = k
  for (const key of Object.keys(values)) delete values[key]
  Object.assign(values, initialValues(k))
  for (const key of Object.keys(credentials)) delete credentials[key]
  errors.value = {}
}
function clearCredentials(): void {
  for (const key of Object.keys(credentials)) credentials[key] = ''
}
// The secrets are held by this dialog only; when it goes away so do they.
onBeforeUnmount(clearCredentials)

const main = computed(() => FIELDS[kind.value].filter((f) => !f.advanced))
const advanced = computed(() => FIELDS[kind.value].filter((f) => f.advanced))
const confirmCompliance = computed(() =>
  needsComplianceConfirmation(kind.value, values, props.target),
)
const mode = computed(() => (typeof values.mode === 'string' ? values.mode : 'compliance'))

function usePublic(): void {
  values.calendars = PUBLIC_CALENDARS.join('\n')
}

const save = useMutation({
  mutationFn: async (): Promise<LedgerAnchorTarget | null> => {
    const config = buildConfig(kind.value, values, extra.value)
    const creds = buildCredentials(kind.value, credentials)
    if (!props.target)
      return endpoints.createLedgerAnchorTarget({
        kind: kind.value,
        name: name.value.trim(),
        enabled: enabled.value,
        config,
        ...(creds ? { credentials: creds } : {}),
      })
    const t = props.target
    const body: LedgerAnchorTargetUpdate = {}
    if (name.value.trim() !== t.name) body.name = name.value.trim()
    if (enabled.value !== t.enabled) body.enabled = enabled.value
    if (JSON.stringify(values) !== opened) body.config = config
    if (creds) body.credentials = creds
    if (Object.keys(body).length === 0) return null
    return endpoints.updateLedgerAnchorTarget(t.id, body)
  },
  onSuccess: async (t) => {
    // Whatever the request did, the secrets are gone from the form.
    clearCredentials()
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.ledgerAnchorTargets }),
      client.invalidateQueries({ queryKey: keys.ledgerStatus }),
    ])
    if (t) emit('saved', t)
    else unchanged.value = true
    if (t) emit('close')
  },
  onError: () => clearCredentials(),
})
const unchanged = ref(false)

const serverErrors = computed(() => {
  const out: Record<string, string> = {}
  const e = save.error.value
  if (!e) return out
  for (const [field, msg] of Object.entries(describeProblem(e).fieldErrors))
    out[fieldOfServerError(field)] = msg
  if (e instanceof ApiError && e.code === 'already_exists')
    out.name = 'A target with this name exists. Choose another name.'
  return out
})
const errorFor = (key: string): string | undefined => errors.value[key] ?? serverErrors.value[key]
// A problem that is about one field sits next to its field and is repeated here in words; the name already in use sits
// next to the name only.
const notice = computed(() => {
  const e = save.error.value
  if (!e || (e instanceof ApiError && e.code === 'already_exists')) return null
  return e
})

function submit(): void {
  unchanged.value = false
  save.reset()
  const e = validateForm(kind.value, name.value, values, credentials, !editing.value)
  if (confirmCompliance.value && typed.value !== 'compliance')
    e.confirm = 'Type compliance to confirm the lock mode.'
  errors.value = e
  if (Object.keys(e).length > 0) return
  save.mutate()
}

const titleText = computed(() =>
  editing.value ? 'Edit the anchor target' : 'Add an anchor target',
)
const retention = computed(() => retentionOf(values))
const retentionOk = computed(
  () => Number.isInteger(retention.value) && retention.value >= 1 && retention.value <= 36500,
)
</script>

<template>
  <AppDialog :title="titleText" wide :persistent="save.isPending.value" @close="emit('close')">
    <form
      id="anchor-target-form"
      class="stack"
      novalidate
      data-testid="anchor-target-form"
      @submit.prevent="submit"
    >
      <fieldset v-if="!editing" class="stack">
        <legend>Kind of target</legend>
        <label v-for="k in ANCHOR_KINDS" :key="k" class="check kind-choice">
          <input
            type="radio"
            name="anchor-kind"
            :value="k"
            :checked="kind === k"
            :data-testid="`kind-${k}`"
            @change="chooseKind(k)"
          />
          <span
            ><strong>{{ KIND_LABEL[k] }}</strong
            ><br /><span class="muted">{{ KIND_HELP[k] }}</span></span
          >
        </label>
      </fieldset>
      <p v-else class="stack" style="gap: var(--s-1)">
        <span
          >Kind: <strong data-testid="fixed-kind">{{ KIND_LABEL[kind] }}</strong></span
        >
        <span class="muted">The kind of a target cannot change.</span>
      </p>

      <FormField
        v-slot="slot"
        label="Name"
        hint="1 to 63 characters; it names the target in the lists and in the alerts."
        :error="errorFor('name')"
        required
      >
        <input v-bind="slot" v-model="name" class="input" autocomplete="off" maxlength="63" />
      </FormField>

      <template v-for="f in main" :key="`${kind}-${f.key}`">
        <FormField
          v-slot="slot"
          :label="f.label"
          :hint="f.hint"
          :error="errorFor(f.key)"
          :required="f.required"
        >
          <textarea
            v-if="f.type === 'lines'"
            v-bind="slot"
            v-model="values[f.key] as string"
            class="input"
            :class="{ mono: f.mono }"
            rows="4"
            spellcheck="false"
            autocomplete="off"
          />
          <select v-else-if="f.type === 'select'" v-bind="slot" v-model="values[f.key] as string">
            <option v-for="o in f.options" :key="o.value" :value="o.value">{{ o.label }}</option>
          </select>
          <input
            v-else
            v-bind="slot"
            v-model="values[f.key] as string"
            class="input"
            :class="{ mono: f.mono }"
            :inputmode="f.type === 'number' ? 'numeric' : undefined"
            autocomplete="off"
            spellcheck="false"
          />
        </FormField>
        <p v-if="kind === 'ots' && f.key === 'calendars'" class="row">
          <button type="button" class="btn btn-small" @click="usePublic">
            Use the public calendars
          </button>
          <span class="muted">Fills the list; nothing is sent until you save.</span>
        </p>
      </template>

      <section v-if="kind === 'worm'" class="stack" data-testid="lock-mode-note">
        <p
          v-if="mode === 'governance'"
          class="notice tone-warn"
          role="status"
          data-testid="governance-warning"
        >
          <span aria-hidden="true">&#9650;</span><span>{{ GOVERNANCE_TEXT }}</span>
        </p>
        <template v-else>
          <p class="notice tone-info" role="status" data-testid="compliance-note">
            <span aria-hidden="true">&#9432;</span>
            <span
              >{{ COMPLIANCE_TEXT }}
              <template v-if="retentionOk"
                >A lock that starts now would end on
                <strong data-testid="retention-date">{{
                  retentionEnds(retention)
                }}</strong>.</template
              ></span
            >
          </p>
        </template>
        <div v-if="confirmCompliance" class="stack" data-testid="compliance-confirm">
          <p>Type <code>compliance</code> to confirm that this lock cannot be undone.</p>
          <TypeToConfirm v-model="typed" expected="compliance" />
          <span v-if="errorFor('confirm')" class="error" role="alert">{{
            errorFor('confirm')
          }}</span>
        </div>
      </section>

      <section v-if="kind === 'ots'" class="muted" data-testid="ots-note">
        {{ OTS_WAIT_TEXT }}
      </section>

      <details v-if="advanced.length" class="stack" data-testid="more-settings">
        <summary>More settings</summary>
        <div class="stack">
          <template v-for="f in advanced" :key="`${kind}-${f.key}`">
            <label v-if="f.type === 'checkbox'" class="check">
              <input v-model="values[f.key] as boolean" type="checkbox" />
              {{ f.label }}
              <span class="muted">{{ f.hint }}</span>
              <span v-if="errorFor(f.key)" class="error" role="alert">{{ errorFor(f.key) }}</span>
            </label>
            <FormField
              v-else
              v-slot="slot"
              :label="f.label"
              :hint="f.hint"
              :error="errorFor(f.key)"
            >
              <select v-if="f.type === 'select'" v-bind="slot" v-model="values[f.key] as string">
                <option v-for="o in f.options" :key="o.value" :value="o.value">
                  {{ o.label }}
                </option>
              </select>
              <input
                v-else
                v-bind="slot"
                v-model="values[f.key] as string"
                class="input"
                :class="{ mono: f.mono }"
                :inputmode="f.type === 'number' ? 'numeric' : undefined"
                autocomplete="off"
                spellcheck="false"
              />
            </FormField>
          </template>
        </div>
      </details>

      <fieldset v-if="CREDENTIAL_FIELDS[kind].length" class="stack" data-testid="credentials">
        <legend>Credentials</legend>
        <p class="muted">{{ CREDENTIALS_TEXT }}</p>
        <p v-if="editing" class="muted" data-testid="credentials-state">
          {{ target?.credentialsSet ? 'Credentials are stored.' : 'No credentials are stored.' }}
        </p>
        <FormField
          v-for="c in CREDENTIAL_FIELDS[kind]"
          :key="`${kind}-${c.key}`"
          v-slot="slot"
          :label="c.label"
          :hint="c.hint"
          :error="errorFor(`credentials.${c.key}`)"
          :required="c.requiredOnCreate && !editing"
        >
          <input
            v-bind="slot"
            v-model="credentials[c.key] as string"
            class="input mono"
            :type="c.secret ? 'password' : 'text'"
            autocomplete="off"
            spellcheck="false"
            :placeholder="editing && target?.credentialsSet ? 'Stored' : undefined"
            :data-testid="`credential-${c.key}`"
          />
        </FormField>
      </fieldset>

      <label class="check">
        <input v-model="enabled" type="checkbox" data-testid="enabled-box" />
        {{ editing ? 'This target is used' : 'Use this target as soon as it is saved' }}
        <span class="muted">{{
          editing ? '' : 'You can also save it first, test it, and turn it on later.'
        }}</span>
      </label>

      <ProblemNotice v-if="notice" :error="notice" />
      <p v-if="unchanged" class="notice tone-info" role="status" data-testid="anchor-unchanged">
        <span aria-hidden="true">&#9432;</span> Nothing was changed, so nothing was saved.
      </p>
      <p v-if="Object.keys(errors).length" class="error" role="alert" data-testid="form-errors">
        Some fields need your attention:
        <template v-for="(_m, k, i) in errors" :key="k"
          ><template v-if="i > 0">, </template
          ><SafeName :value="String(k)" kind="text" :max="40" /></template
        >.
      </p>
    </form>
    <template #footer>
      <button type="button" class="btn" :disabled="save.isPending.value" @click="emit('close')">
        Cancel
      </button>
      <button
        type="submit"
        form="anchor-target-form"
        class="btn btn-primary"
        :disabled="save.isPending.value"
      >
        {{ editing ? 'Save the target' : 'Add the target' }}
      </button>
    </template>
  </AppDialog>
</template>

<style scoped>
.kind-choice {
  align-items: flex-start;
}
</style>
