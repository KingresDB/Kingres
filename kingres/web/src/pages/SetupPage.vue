<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// First-run wizard, shown while the server is `uninitialized`:
//   1. setup token  2. create a cluster or join one  3. task progress  4. hand-off to sign-in.
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useQueryClient } from '@tanstack/vue-query'
import FormField from '@/components/FormField.vue'
import PinChoice from '@/components/PinChoice.vue'
import OwnFingerprint from '@/components/OwnFingerprint.vue'
import SafeName from '@/components/SafeName.vue'
import { fingerprintError, normalizeFingerprint } from '@/lib/fingerprint'
import BrandLogo from '@/components/BrandLogo.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import LiveRegions from '@/components/LiveRegions.vue'
import { createAnnouncer } from '@/composables/useAnnounce'
import { vBlockEnterSubmit } from '@/lib/forms'
import { setSetupToken } from '@/api/client'
import { describeProblem } from '@/api/problem'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import {
  absolutePath,
  compact,
  firstError,
  integerBetween,
  minLength,
  nodeAddress,
  required,
  CLUSTER_NAME_RE,
  IDENTIFIER_RE,
} from '@/lib/validators'
import type { Durability, SetupTaskSummary, Task } from '@/api/types'

type Step = 'token' | 'choose' | 'create' | 'join' | 'progress'

const router = useRouter()
const queryClient = useQueryClient()
const announcer = createAnnouncer()
const step = ref<Step>('token')
const token = ref('')
const submitted = ref(false)

const create = reactive({
  clusterName: '',
  adminUser: 'admin',
  adminPassword: '',
  adminPassword2: '',
  dataDirectory: '/var/lib/kingres',
  walDirectory: '',
  logDirectory: '',
  memoryMb: 4096,
  sharding: true,
  ledger: true,
  replicaCount: 1,
  durability: 'sync' as Durability,
})
const join = reactive({
  address: '',
  adminUser: 'admin',
  adminPassword: '',
  dataDirectory: '/var/lib/kingres',
  memoryMb: 4096,
  serverGroup: '',
  expectedFingerprint: '',
})
/** The person chose to skip the check (never pre-selected): sent as `allowUnpinned: true`. */
const joinUnpinned = ref(false)
/** This server's own fingerprint, when the server reports it (loopback caller or valid setup token). */
const ownFingerprint = ref<string | null>(null)
async function loadOwnFingerprint(): Promise<void> {
  try {
    ownFingerprint.value = (await endpoints.getNodeState()).internalFingerprint ?? null
  } catch {
    ownFingerprint.value = null
  }
}
onMounted(() => void loadOwnFingerprint())

/** Last task seen by the wizard; shown if a poll briefly reports no setup task. */
let lastTask: Task | null = null

/** The summary in GET /setup/state, shaped like a Task so the shared progress view can show it. */
function taskFromSummary(summary: SetupTaskSummary): Task {
  return {
    id: summary.id,
    kind: summary.kind,
    state: summary.state,
    progress: {
      percent: summary.progressPercent,
      currentStep: summary.steps.find((s) => s.state === 'running')?.name ?? null,
      steps: summary.steps.map((s) => ({ name: s.name, state: s.state })),
    },
    error: summary.error,
    detail: summary.detail ?? null,
    subject: { type: 'cluster', id: 'cluster' },
    cancellable: false,
    createdAt: summary.createdAt,
    updatedAt: summary.updatedAt,
    createdBy: 'setup',
  }
}

/**
 * Follows setup through GET /setup/state (`setupTask`). GET /tasks/{id} is not used: it needs a
 * session, and there is none until setup has finished and the user has signed in.
 */
async function readSetupTask(): Promise<Task> {
  const node = await endpoints.getNodeState()
  if (!node.setupTask) {
    if (lastTask) return lastTask
    throw new Error('The server reports no setup task.')
  }
  lastTask = taskFromSummary(node.setupTask)
  return lastTask
}
const remember = (accepted: Promise<Task>): Promise<Task> =>
  accepted.then((task) => {
    lastTask = task
    return task
  })

const createAction = useTaskAction(
  () =>
    remember(
      endpoints.initCluster({
        clusterName: create.clusterName,
        admin: { username: create.adminUser, password: create.adminPassword },
        dataPaths: {
          dataDirectory: create.dataDirectory,
          walDirectory: create.walDirectory || null,
          logDirectory: create.logDirectory || null,
        },
        memoryQuota: { totalMegabytes: create.memoryMb },
        features: { sharding: create.sharding, ledger: create.ledger },
        defaultReplicaCount: create.replicaCount,
        defaultDurability: create.durability,
      }),
    ),
  { fetchTask: readSetupTask },
)
const joinAction = useTaskAction(
  () =>
    remember(
      endpoints.joinCluster({
        existingNodeAddress: join.address,
        admin: { username: join.adminUser, password: join.adminPassword },
        dataPaths: { dataDirectory: join.dataDirectory },
        memoryQuota: { totalMegabytes: join.memoryMb },
        serverGroup: join.serverGroup || null,
        ...(joinUnpinned.value
          ? { allowUnpinned: true }
          : { expectedFingerprint: normalizeFingerprint(join.expectedFingerprint) }),
      }),
    ),
  { fetchTask: readSetupTask },
)
const mode = ref<'create' | 'join'>('create')
const action = computed(() => (mode.value === 'create' ? createAction : joinAction))

const createErrors = computed(() =>
  compact({
    clusterName: CLUSTER_NAME_RE.test(create.clusterName)
      ? undefined
      : 'Use letters, digits, "-" and "_", starting with a letter or digit (up to 63 characters).',
    'admin.username': IDENTIFIER_RE.test(create.adminUser)
      ? undefined
      : 'Use lower-case letters, digits and "_".',
    'admin.password': firstError(create.adminPassword, minLength('The password', 12)),
    adminPassword2:
      create.adminPassword === create.adminPassword2 ? undefined : 'The passwords do not match.',
    'dataPaths.dataDirectory': firstError(create.dataDirectory, absolutePath('The data directory')),
    'memoryQuota.totalMegabytes': firstError(create.memoryMb, (v) =>
      v >= 512 ? undefined : 'The memory quota must be at least 512 MB.',
    ),
    defaultReplicaCount: firstError<number | null>(
      create.replicaCount,
      integerBetween('The replica count', 0, 3),
    ),
  }),
)
const joinErrors = computed(() =>
  compact({
    existingNodeAddress: firstError(join.address, nodeAddress()),
    'admin.username': firstError(join.adminUser, required('The user name')),
    'admin.password': firstError(join.adminPassword, required('The password')),
    'dataPaths.dataDirectory': firstError(join.dataDirectory, absolutePath('The data directory')),
    expectedFingerprint: joinUnpinned.value
      ? undefined
      : join.expectedFingerprint.trim() === ''
        ? 'Enter the fingerprint of the existing server, or choose below to trust whatever answers at the address.'
        : fingerprintError(join.expectedFingerprint),
  }),
)

/** Errors the server reported for the current form, keyed like the client-side ones. */
const serverErrors = computed(() =>
  action.value.error.value ? describeProblem(action.value.error.value).fieldErrors : {},
)
const errors = computed<Record<string, string | undefined>>(() => ({
  ...serverErrors.value,
  // The contract names a body property by JSON Pointer; the forms key it without the slash.
  ...(serverErrors.value['/expectedFingerprint']
    ? { expectedFingerprint: serverErrors.value['/expectedFingerprint'] }
    : {}),
  ...(submitted.value ? (mode.value === 'create' ? createErrors.value : joinErrors.value) : {}),
}))

const tokenInvalid = computed(() => {
  const e = action.value.error.value
  return e ? describeProblem(e).code === 'setup_token_invalid' : false
})

const tokenChecking = ref(false)
const tokenError = ref<unknown>(null)

/** Validates the token with the server before the user fills in anything (POST /setup/token-check). */
async function acceptToken(): Promise<void> {
  tokenChecking.value = true
  tokenError.value = null
  setSetupToken(token.value.trim())
  try {
    await endpoints.checkSetupToken()
    step.value = 'choose'
    // With the token the server may now tell this server's own fingerprint.
    void loadOwnFingerprint()
  } catch (error) {
    setSetupToken(null)
    tokenError.value = error
  } finally {
    tokenChecking.value = false
  }
}

function choose(next: 'create' | 'join'): void {
  mode.value = next
  submitted.value = false
  step.value = next
}

async function submit(): Promise<void> {
  submitted.value = true
  const invalid = mode.value === 'create' ? createErrors.value : joinErrors.value
  if (Object.keys(invalid).length > 0) return
  const accepted = await action.value.run()
  if (accepted) step.value = 'progress'
}

function backToForm(): void {
  action.value.reset()
  step.value = mode.value
  // The password of the administrator account is typed again after a failure.
  create.adminPassword = ''
  create.adminPassword2 = ''
  join.adminPassword = ''
}

// The setup token is one-time and held in memory: it must not stay attached to later requests when the
// page is left for any reason (back button, another tab finished the setup, a redirect).
onBeforeUnmount(() => setSetupToken(null))

async function finish(): Promise<void> {
  setSetupToken(null)
  await queryClient.invalidateQueries({ queryKey: keys.nodeState })
  await router.push({ name: 'login' })
}
</script>

<template>
  <main class="wizard">
    <LiveRegions :polite="announcer.polite.value" :assertive="announcer.assertive.value" />
    <div class="card stack-lg">
      <header class="stack" style="gap: var(--s-1)">
        <BrandLogo variant="horizontal" :size="44" />
        <h1>Set up this server</h1>
        <p class="muted">
          This KingresDB server has not been set up yet. Create a new cluster or join an existing
          one.
        </p>
        <ol class="steps" aria-label="Setup steps">
          <li :aria-current="step === 'token' ? 'step' : undefined">1. Setup token</li>
          <li :aria-current="step === 'choose' ? 'step' : undefined">2. Create or join</li>
          <li :aria-current="step === 'create' || step === 'join' ? 'step' : undefined">
            3. Settings
          </li>
          <li :aria-current="step === 'progress' ? 'step' : undefined">4. Setup</li>
        </ol>
      </header>

      <!-- 1. token -->
      <form v-if="step === 'token'" class="stack" @submit.prevent="acceptToken">
        <ProblemNotice v-if="tokenError" :error="tokenError" />
        <FormField
          v-slot="slot"
          label="Setup token"
          hint="Read the one-time token from the root-readable file written when the package was installed."
        >
          <input
            v-bind="slot"
            v-model="token"
            class="input mono"
            autocomplete="off"
            required
            data-autofocus
          />
        </FormField>
        <div>
          <button type="submit" class="btn btn-primary" :disabled="!token.trim() || tokenChecking">
            {{ tokenChecking ? 'Checking...' : 'Continue' }}
          </button>
        </div>
      </form>

      <!-- 2. choose -->
      <div v-else-if="step === 'choose'" class="stack">
        <div class="grid-2">
          <section class="card stack">
            <h2>Create new cluster</h2>
            <p>Start a new cluster with this server as its first member.</p>
            <div>
              <button type="button" class="btn btn-primary" @click="choose('create')">
                Create new cluster
              </button>
            </div>
          </section>
          <section class="card stack">
            <h2>Join existing cluster</h2>
            <p>
              Add this server to a cluster that already runs. Data moves only after you start a
              rebalance.
            </p>
            <div>
              <button type="button" class="btn" @click="choose('join')">
                Join existing cluster
              </button>
            </div>
          </section>
        </div>
        <section class="card stack" aria-labelledby="own-fp-h">
          <h2 id="own-fp-h">Fingerprint of this server</h2>
          <OwnFingerprint
            :fingerprint="ownFingerprint"
            purpose="If an administrator adds this server from the panel of a cluster (Add server), they pin this fingerprint. Give it to them by a way other than the network between the two servers."
            fallback="This server does not report its fingerprint here (it does so only to a request from the machine itself, or with the setup token). Read it in the log of this server, in the line bootstrap certificate of the internal API."
          />
        </section>
        <div>
          <button type="button" class="btn btn-link" @click="step = 'token'">
            Change setup token
          </button>
        </div>
      </div>

      <!-- 3a. create -->
      <form
        v-else-if="step === 'create'"
        v-block-enter-submit
        class="stack"
        novalidate
        @submit.prevent="submit"
      >
        <h2>Create new cluster</h2>
        <ProblemNotice v-if="action.error.value" :error="action.error.value" />
        <p v-if="tokenInvalid">
          <button type="button" class="btn btn-link" @click="step = 'token'">
            Enter the setup token again
          </button>
        </p>
        <fieldset>
          <legend>Cluster</legend>
          <FormField v-slot="slot" label="Cluster name" :error="errors.clusterName" required>
            <input
              v-bind="slot"
              v-model="create.clusterName"
              class="input"
              autocomplete="off"
              data-autofocus
            />
          </FormField>
        </fieldset>
        <fieldset>
          <legend>Administrator account</legend>
          <FormField v-slot="slot" label="User name" :error="errors['admin.username']" required>
            <input v-bind="slot" v-model="create.adminUser" class="input" autocomplete="username" />
          </FormField>
          <FormField
            v-slot="slot"
            label="Password"
            hint="At least 12 characters."
            :error="errors['admin.password']"
            required
          >
            <input
              v-bind="slot"
              v-model="create.adminPassword"
              class="input"
              type="password"
              autocomplete="new-password"
            />
          </FormField>
          <FormField v-slot="slot" label="Repeat password" :error="errors.adminPassword2" required>
            <input
              v-bind="slot"
              v-model="create.adminPassword2"
              class="input"
              type="password"
              autocomplete="new-password"
            />
          </FormField>
        </fieldset>
        <fieldset>
          <legend>Data paths and memory</legend>
          <FormField
            v-slot="slot"
            label="Data directory"
            :error="errors['dataPaths.dataDirectory']"
            required
          >
            <input v-bind="slot" v-model="create.dataDirectory" class="input mono" />
          </FormField>
          <FormField
            v-slot="slot"
            label="WAL directory (optional)"
            hint="Leave empty to keep the WAL inside the data directory."
          >
            <input v-bind="slot" v-model="create.walDirectory" class="input mono" />
          </FormField>
          <FormField v-slot="slot" label="Log directory (optional)">
            <input v-bind="slot" v-model="create.logDirectory" class="input mono" />
          </FormField>
          <FormField
            v-slot="slot"
            label="Memory quota (MB)"
            :error="errors['memoryQuota.totalMegabytes']"
            required
          >
            <input
              v-bind="slot"
              v-model.number="create.memoryMb"
              class="input num"
              type="number"
              min="512"
              step="256"
            />
          </FormField>
        </fieldset>
        <fieldset>
          <legend>Features</legend>
          <label class="check"><input v-model="create.sharding" type="checkbox" /> Sharding</label>
          <label class="check"
            ><input v-model="create.ledger" type="checkbox" /> Tamper-evident ledger</label
          >
          <p class="hint muted">
            Features can be turned on later in Settings but never turned off again.
          </p>
          <FormField
            v-slot="slot"
            label="Default replica count"
            hint="Standby copies of every shard group (0 to 3). Changed later in cluster settings."
            :error="errors.defaultReplicaCount"
          >
            <input
              v-bind="slot"
              v-model.number="create.replicaCount"
              class="input num"
              type="number"
              min="0"
              max="3"
            />
          </FormField>
          <FormField v-slot="slot" label="Default durability">
            <select v-bind="slot" v-model="create.durability">
              <option value="async">Asynchronous</option>
              <option value="sync">Synchronous (quorum)</option>
              <option value="syncApply">Synchronous with apply</option>
            </select>
          </FormField>
        </fieldset>
        <div class="row">
          <button type="submit" class="btn btn-primary" :disabled="action.isStarting.value">
            Create cluster
          </button>
          <button type="button" class="btn" @click="step = 'choose'">Back</button>
        </div>
      </form>

      <!-- 3b. join -->
      <form
        v-else-if="step === 'join'"
        v-block-enter-submit
        class="stack"
        novalidate
        @submit.prevent="submit"
      >
        <h2>Join existing cluster</h2>
        <ProblemNotice v-if="action.error.value" :error="action.error.value" />
        <p v-if="tokenInvalid">
          <button type="button" class="btn btn-link" @click="step = 'token'">
            Enter the setup token again
          </button>
        </p>
        <fieldset>
          <legend>Existing cluster</legend>
          <FormField
            v-slot="slot"
            label="Address of any existing server"
            hint="host:port of its panel port, for example 10.0.0.11:8091."
            :error="errors.existingNodeAddress"
            required
          >
            <input v-bind="slot" v-model="join.address" class="input mono" data-autofocus />
          </FormField>
          <PinChoice
            v-model:fingerprint="join.expectedFingerprint"
            v-model:unpinned="joinUnpinned"
            purpose="join"
            hint="SHA-256 of that server's internal certificate, 64 hex digits; colons are allowed. Read it on that server's This server page."
            :error="errors.expectedFingerprint"
          />
          <FormField
            v-slot="slot"
            label="Administrator user name"
            :error="errors['admin.username']"
            required
          >
            <input v-bind="slot" v-model="join.adminUser" class="input" autocomplete="username" />
          </FormField>
          <FormField
            v-slot="slot"
            label="Administrator password"
            :error="errors['admin.password']"
            required
          >
            <input
              v-bind="slot"
              v-model="join.adminPassword"
              class="input"
              type="password"
              autocomplete="current-password"
            />
          </FormField>
        </fieldset>
        <fieldset>
          <legend>This server</legend>
          <FormField
            v-slot="slot"
            label="Data directory"
            :error="errors['dataPaths.dataDirectory']"
            required
          >
            <input v-bind="slot" v-model="join.dataDirectory" class="input mono" />
          </FormField>
          <FormField
            v-slot="slot"
            label="Memory quota (MB)"
            :error="errors['memoryQuota.totalMegabytes']"
          >
            <input
              v-bind="slot"
              v-model.number="join.memoryMb"
              class="input num"
              type="number"
              min="512"
              step="256"
            />
          </FormField>
          <FormField v-slot="slot" label="Server group (optional)" hint="Rack or zone name.">
            <input v-bind="slot" v-model="join.serverGroup" class="input" />
          </FormField>
        </fieldset>
        <p class="muted">
          After joining, this server shows as "pending rebalance". Data moves only when you start a
          rebalance.
        </p>
        <div class="row">
          <button type="submit" class="btn btn-primary" :disabled="action.isStarting.value">
            Join cluster
          </button>
          <button type="button" class="btn" @click="step = 'choose'">Back</button>
        </div>
      </form>

      <!-- 4. progress -->
      <div v-else class="stack">
        <h2>{{ mode === 'create' ? 'Creating the cluster' : 'Joining the cluster' }}</h2>
        <TaskProgress :task="action.task.value" :poll-error="action.pollError.value" />
        <div v-if="action.state.value === 'done'" class="stack">
          <p class="notice tone-ok" role="status">
            <span aria-hidden="true">&#10003;</span>
            This server is ready. Sign in with the administrator account.
          </p>
          <p v-if="mode === 'join'" class="muted" data-testid="join-fingerprint-note">
            <template v-if="action.task.value?.detail"
              >What the server reports about the first contact:
              <SafeName :value="action.task.value.detail" kind="text" :max="500" multiline
            /></template>
            <template v-else
              >The server did not report whether the certificate of the existing server was checked,
              so this page cannot say. Compare the fingerprint with the one on the This server page
              of the existing server.</template
            >
          </p>
          <div>
            <button type="button" class="btn btn-primary" @click="finish">
              Continue to sign in
            </button>
          </div>
        </div>
        <div v-else-if="action.state.value === 'failed'">
          <button type="button" class="btn" @click="backToForm">Back to the settings</button>
        </div>
      </div>
    </div>
  </main>
</template>

<style scoped>
.wizard {
  min-height: 100vh;
  display: grid;
  place-items: start center;
  padding: var(--s-6) var(--s-4);
}
.wizard .card {
  width: min(760px, 100%);
}
.steps {
  list-style: none;
  display: flex;
  flex-wrap: wrap;
  gap: var(--s-1) var(--s-4);
  margin: var(--s-2) 0 0;
  padding: 0;
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
}
.steps [aria-current='step'] {
  color: var(--c-text);
  font-weight: 700;
}
</style>
