<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The state of an upload: the progress bar (read out every ten percent), the bytes the server holds, pause, resume and
// cancel (which asks first when much was sent), the sentence for every way it can stop, and the prompt for the password
// when the server says the session ended (the upload then goes on from the server's offset). The sentences are fixed; what
// the server said is shown through the notice of the problem.
import { computed, ref, watch } from 'vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { useAnnounce } from '@/composables/useAnnounce'
import type { UploadFailure, UploadSnapshot } from '@/lib/uploader'
import { formatBytes } from '@/lib/format'
import { MIB } from '@/lib/transfer'

const props = defineProps<{
  snapshot: UploadSnapshot
  percent: number
  /** True while the sign-in of the prompt is being checked. */
  signingIn: boolean
  /** The refusal of the sign-in, if there was one. */
  signInError: unknown
}>()
const emit = defineEmits<{
  pause: []
  resume: []
  cancel: []
  retry: []
  signIn: [password: string]
}>()

const announce = useAnnounce()
const password = ref('')
/** Cancelling deletes what was sent: it asks first when more than a few megabytes are on the server. */
const confirming = ref(false)
const CONFIRM_ABOVE = 4 * MIB

const FAILURE: Record<UploadFailure, string> = {
  empty: 'The file is empty.',
  too_large:
    'The file is above the limit of one import for this table or above the largest file the cluster stages. Split it into parts and import them one after the other. Nothing was sent.',
  size_differs:
    'The upload the browser remembered is for a file of another size, so it is not continued. Start the upload again.',
  lost: 'The server no longer has this upload (it expired, was deleted, or its file is gone). Start the upload again.',
  refused:
    'The server refused the upload: data transfer is turned off, your role is too low, or the connection is not encrypted.',
  quota:
    'The staging area of the cluster has no room for this file. Delete exports or uploads you no longer need, or try again later.',
  checksum:
    'The file arrived changed: its SHA-256 differs from the one declared, and the server deleted the upload. Send the file again.',
  rejected: 'The server refused a part of the file. Start the upload again.',
  network:
    'The connection broke and did not come back. What the server holds is kept; resume the upload when the connection is back.',
  busy: 'The server says that another request is still writing this upload and did not let go in time. What the server holds is kept; resume the upload in a moment.',
  other: 'The upload stopped. What the server holds is kept; try to resume it.',
}
const failure = computed(() => (props.snapshot.failure ? FAILURE[props.snapshot.failure] : null))
const canResume = computed(
  () =>
    props.snapshot.phase === 'paused' ||
    (props.snapshot.phase === 'failed' &&
      (props.snapshot.failure === 'network' ||
        props.snapshot.failure === 'busy' ||
        props.snapshot.failure === 'other') &&
      props.snapshot.uploadId !== null),
)
const showProblem = computed(
  () =>
    props.snapshot.problem !== null &&
    props.snapshot.failure !== null &&
    props.snapshot.failure !== 'network' &&
    props.snapshot.failure !== 'busy' &&
    props.snapshot.failure !== 'size_differs',
)

watch(
  () => Math.floor(props.percent / 10),
  (tenth) => {
    if (props.snapshot.phase === 'sending' && tenth > 0) announce(`Upload ${tenth * 10} percent.`)
  },
)

function submit(): void {
  if (password.value === '') return
  emit('signIn', password.value)
  password.value = ''
}

function askCancel(): void {
  if (props.snapshot.offset > CONFIRM_ABOVE) confirming.value = true
  else emit('cancel')
}
function confirmCancel(): void {
  confirming.value = false
  emit('cancel')
}
</script>

<template>
  <div class="stack" data-testid="upload-status" :data-phase="snapshot.phase">
    <div
      class="progress"
      role="progressbar"
      aria-label="Upload progress"
      aria-valuemin="0"
      aria-valuemax="100"
      :aria-valuenow="percent"
    >
      <span :style="{ width: `${percent}%` }" />
    </div>
    <p class="muted num" data-testid="upload-bytes">
      {{ formatBytes(snapshot.offset) }} of {{ formatBytes(snapshot.size) }} are on the server
      <template v-if="snapshot.resumes > 0">
        ({{ snapshot.resumes }}
        {{ snapshot.resumes === 1 ? 'break' : 'breaks' }} survived)</template
      >
    </p>
    <p v-if="snapshot.continued" class="muted" data-testid="upload-continued">
      An earlier upload of this file was found and is continued from where it stopped.
    </p>
    <p v-if="snapshot.phase === 'starting'" class="muted" role="status">Starting the upload ...</p>
    <p v-if="snapshot.phase === 'sending'" class="muted" role="status">
      Sending the file in parts of 64 MiB, one after the other. Keep this page open; if it closes,
      the upload can be continued by choosing the same file again.
    </p>
    <p v-if="snapshot.phase === 'paused'" class="muted" role="status" data-testid="upload-paused">
      Paused. The server keeps what it has.
    </p>
    <p
      v-if="snapshot.phase === 'complete'"
      class="notice tone-ok"
      role="status"
      data-testid="upload-complete"
    >
      <span aria-hidden="true">&#10003;</span>
      <span
        >The whole file is on the server.
        <template v-if="snapshot.sha256"
          >SHA-256 <code class="mono wrap">{{ snapshot.sha256 }}</code></template
        ></span
      >
    </p>
    <form
      v-if="snapshot.phase === 'password'"
      class="stack"
      data-testid="upload-password"
      @submit.prevent="submit"
    >
      <p class="notice tone-warn" role="alert">
        <span aria-hidden="true">&#9650;</span>
        <span
          >Your session ended in the middle of the upload. Enter your password to go on from where
          the server is ({{ formatBytes(snapshot.offset) }} of {{ formatBytes(snapshot.size) }}).
          Cancelling now cannot delete the upload on the server, because that needs a session: it
          stays listed in the Transfers panel until you delete it there.</span
        >
      </p>
      <label for="upload-password">Password</label>
      <input
        id="upload-password"
        v-model="password"
        class="input"
        type="password"
        autocomplete="current-password"
        data-autofocus
      />
      <ProblemNotice v-if="signInError" :error="signInError" />
      <div class="row">
        <button type="submit" class="btn btn-primary" :disabled="signingIn || password === ''">
          Sign in and continue
        </button>
        <button type="button" class="btn" @click="emit('cancel')">Cancel the upload</button>
      </div>
    </form>
    <div v-if="failure" class="stack" data-testid="upload-failed">
      <p class="notice tone-danger" role="alert">
        <span aria-hidden="true">&#10006;</span> <span>{{ failure }}</span>
      </p>
      <ProblemNotice v-if="showProblem" :error="snapshot.problem" />
    </div>
    <div v-if="confirming" class="stack" data-testid="cancel-confirm" role="alert">
      <p class="notice tone-warn">
        <span aria-hidden="true">&#9650;</span>
        <span
          >{{ formatBytes(snapshot.offset) }} are on the server. Cancelling deletes them, and the
          file has to be sent again from the start.</span
        >
      </p>
      <div class="row">
        <button
          type="button"
          class="btn btn-danger"
          data-testid="cancel-yes"
          data-autofocus
          @click="confirmCancel"
        >
          Delete the upload
        </button>
        <button type="button" class="btn" @click="confirming = false">Keep the upload</button>
      </div>
    </div>
    <div v-if="snapshot.phase !== 'password' && !confirming" class="row">
      <button
        v-if="snapshot.phase === 'sending' || snapshot.phase === 'starting'"
        type="button"
        class="btn"
        data-testid="upload-pause"
        @click="emit('pause')"
      >
        Pause
      </button>
      <button
        v-if="canResume"
        type="button"
        class="btn btn-primary"
        data-testid="upload-resume"
        @click="emit('resume')"
      >
        Resume the upload
      </button>
      <button
        v-if="snapshot.phase === 'failed' && !canResume"
        type="button"
        class="btn"
        data-testid="upload-retry"
        @click="emit('retry')"
      >
        Back to the options
      </button>
      <button
        v-if="snapshot.phase !== 'complete' && snapshot.phase !== 'cancelled'"
        type="button"
        class="btn"
        data-testid="upload-cancel"
        @click="askCancel"
      >
        Cancel the upload
      </button>
    </div>
  </div>
</template>
