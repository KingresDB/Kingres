<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Signing tab of the Ledger page (contract 0.11.0, design 3.2 to 3.4, 8.5, 9.2): the cluster identifier, where the
// private key lives, the keys with their state and dates, the servers that hold them, the signed events, the trust file
// and what an operator must keep outside the cluster. Only public information is shown: a private key, a seed or a
// passphrase is never asked for, shown, stored or logged. An operation that the server does not build yet answers
// `unsupported_operation` and is shown as what it is.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useMutation } from '@tanstack/vue-query'
import QueryBoundary from '@/components/QueryBoundary.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import GateButton from '@/components/GateButton.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import NullableValue from '@/components/NullableValue.vue'
import CreateKeyDialog from '@/components/ledger/CreateKeyDialog.vue'
import RetireKeyDialog from '@/components/ledger/RetireKeyDialog.vue'
import RevokeKeyDialog from '@/components/ledger/RevokeKeyDialog.vue'
import { endpoints } from '@/api/endpoints'
import { useLedgerSettings, useLedgerSigning } from '@/api/queries'
import { saveTextFile } from '@/lib/download'
import { formatDateTime, formatNumber } from '@/lib/format'
import { isAddressableSegment } from '@/lib/path-segment'
import { useSessionStore } from '@/stores/session'
import {
  activeKeys,
  BACKUP_TEXT,
  eventTypeLabel,
  fingerprintGroups,
  KEEP_OUTSIDE,
  KEY_STATE,
  retireBlockedReason,
  revokeBlockedReason,
  type SigningKey,
} from '@/lib/ledger-signing'

const session = useSessionStore()
const signing = useLedgerSigning()
const settings = useLedgerSettings()
const data = computed(() => signing.data.value)
const keysList = computed(() => data.value?.keys ?? [])

const dialog = ref<
  | { kind: 'create'; first: boolean }
  | { kind: 'retire'; key: SigningKey }
  | { kind: 'revoke'; key: SigningKey }
  | null
>(null)

const hasKeys = computed(() => keysList.value.length > 0)
const noActive = computed(() => hasKeys.value && activeKeys(keysList.value).length === 0)
const rotateBlocked = computed(() =>
  noActive.value
    ? 'No key is active, so there is nothing to rotate. Replacing a lost key is not available yet.'
    : '',
)
const prefixNow = computed(() => settings.data.value?.publication.originPrefix ?? '')
/** The settings are needed to propose the origin prefix of the first key; without them the key is not offered. */
const firstBlocked = computed(() =>
  settings.data.value === undefined
    ? 'The settings of the publication could not be read, so the origin prefix cannot be confirmed.'
    : '',
)

// --- the trust file: a download the server provides, saved as sent -----------------------------------------------
const trust = useMutation({
  mutationFn: () => endpoints.getLedgerTrustFile(),
  onSuccess: (text) => saveTextFile('trust.json', text),
})

// --- copying a public key --------------------------------------------------------------------------------------
const copied = ref<string | null>(null)
async function copyPublicKey(key: SigningKey): Promise<void> {
  try {
    await navigator.clipboard.writeText(key.publicKey)
    copied.value = key.fingerprint
  } catch {
    copied.value = null
  }
}
const canServer = computed(() => session.can('getServer'))
</script>

<template>
  <div class="stack-lg">
    <QueryBoundary
      :loading="signing.isPending.value"
      :error="signing.error.value"
      @retry="signing.refetch()"
    >
      <template v-if="data">
        <section class="card stack" aria-labelledby="sign-state-h">
          <div class="row-between">
            <h2 id="sign-state-h">Signing keys</h2>
            <div class="row">
              <GateButton
                v-if="!hasKeys"
                op="createLedgerSigningKey"
                variant="primary"
                :blocked-reason="firstBlocked"
                @click="dialog = { kind: 'create', first: true }"
                >Create the first key</GateButton
              >
              <GateButton
                v-else
                op="createLedgerSigningKey"
                :blocked-reason="rotateBlocked"
                @click="dialog = { kind: 'create', first: false }"
                >Rotate the key</GateButton
              >
            </div>
          </div>
          <p class="muted">
            The ledger signs the checkpoints of its logs with one Ed25519 key for the whole cluster.
            The private key lives on the servers that may become the manager leader and never leaves
            them; this page shows public information only.
          </p>

          <p
            v-if="data.signerAlarm"
            class="notice tone-danger"
            role="alert"
            data-testid="signer-alarm"
          >
            <span aria-hidden="true">&#10006;</span>
            <span
              ><strong>This server does not sign.</strong>
              <SafeName :value="data.signerAlarm" kind="text" :max="300" multiline
            /></span>
          </p>
          <p
            v-if="data.discontinuity"
            class="notice tone-warn"
            role="status"
            data-testid="discontinuity"
          >
            <span aria-hidden="true">&#9650;</span>
            <span
              >The key ring was replaced after every key was lost. Old checkpoints stay valid under
              the old key for auditors who still pin it, but every auditor must replace their trust
              file with a new one, obtained out of band.</span
            >
          </p>
          <p v-if="!hasKeys" class="notice tone-info" role="status" data-testid="no-key">
            <span aria-hidden="true">&#9432;</span>
            <span
              >No signing key exists yet. Until the first key is created nothing is signed or
              published.</span
            >
          </p>
          <p v-if="noActive" class="notice tone-danger" role="alert" data-testid="no-active-key">
            <span aria-hidden="true">&#10006;</span>
            <span
              >No key is active, so nothing can be signed. Replacing a lost key is not available
              yet: restore the key directory from the backup.</span
            >
          </p>
          <p
            v-if="data.backupAdvised"
            class="notice tone-warn"
            role="status"
            data-testid="backup-advised"
          >
            <span aria-hidden="true">&#9650;</span>
            <span>{{ BACKUP_TEXT }}</span>
          </p>
          <p
            v-if="hasKeys && !data.passphraseProtected"
            class="notice tone-warn"
            role="status"
            data-testid="not-protected"
          >
            <span aria-hidden="true">&#9650;</span>
            <span
              >The key files of this server are not protected by a passphrase: they are stored as
              they are, as the key of the cluster certificate authority is. Anyone who can read the
              key directory can sign as this cluster. Start <code>kingresd</code> with a passphrase
              file to seal them.</span
            >
          </p>

          <dl class="facts">
            <div>
              <dt>Cluster identifier</dt>
              <dd class="mono wrap" data-testid="cluster-id">
                <SafeName v-if="data.clusterId" :value="data.clusterId" kind="text" :max="100" />
                <span v-else class="muted">Created with the first key</span>
              </dd>
            </div>
            <div>
              <dt>Where the private key lives</dt>
              <dd data-testid="signing-mode">
                <template v-if="data.mode === 'file'"
                  >A key file on the holders (below)<template v-if="data.passphraseProtected"
                    >, sealed with a passphrase</template
                  ></template
                >
                <template v-else-if="data.mode === 'command'"
                  >An external signer (command)</template
                >
                <SafeName v-else :value="String(data.mode)" kind="text" :max="40" />
              </dd>
            </div>
            <div>
              <dt>Origin prefix</dt>
              <dd class="mono wrap" data-testid="signing-prefix">
                <SafeName v-if="prefixNow" :value="prefixNow" kind="text" :max="160" />
                <span v-else class="muted">{{
                  settings.isPending.value ? 'loading' : 'not read'
                }}</span>
                <span v-if="settings.data.value?.publication.originPrefixLocked" class="muted">
                  (locked)</span
                >
              </dd>
            </div>
          </dl>
        </section>

        <section class="card stack" aria-labelledby="keys-h">
          <h2 id="keys-h">Keys</h2>
          <QueryBoundary
            :loading="false"
            :error="null"
            :empty="keysList.length === 0"
            empty-title="No keys yet"
            empty-hint="Create the first key to start signing."
          >
            <TableWrap>
              <table class="table">
                <caption class="sr-only">
                  Signing keys
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Fingerprint (SHA-256 of the public key)</th>
                    <th scope="col">State</th>
                    <th scope="col">Created</th>
                    <th scope="col">Active since</th>
                    <th scope="col">Ended</th>
                    <th scope="col"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="k in keysList" :key="k.fingerprint" :data-testid="`key-${k.state}`">
                    <th scope="row">
                      <code class="mono wrap" data-testid="key-fingerprint"
                        ><SafeName :value="fingerprintGroups(k.fingerprint)" kind="text" :max="100"
                      /></code>
                    </th>
                    <td><StatusBadge :meta="KEY_STATE[k.state]" /></td>
                    <td>{{ formatDateTime(k.createdAt) }}</td>
                    <td>
                      <NullableValue :value="k.activatedAt" :format="formatDateTime" />
                    </td>
                    <td>
                      <template v-if="k.revokedAt"
                        >Revoked {{ formatDateTime(k.revokedAt)
                        }}<template v-if="k.compromisedSince"
                          >, compromised since {{ formatDateTime(k.compromisedSince) }}</template
                        >.</template
                      >
                      <template v-else-if="k.retiredAt"
                        >Retired {{ formatDateTime(k.retiredAt)
                        }}<template v-if="k.maxTreeSize !== null && k.maxTreeSize !== undefined"
                          >, may sign up to size {{ formatNumber(k.maxTreeSize) }}</template
                        >.</template
                      >
                      <span v-else class="muted">In use</span>
                    </td>
                    <td>
                      <div class="row">
                        <button
                          type="button"
                          class="btn btn-small"
                          :aria-label="`Copy public key of ${fingerprintGroups(k.fingerprint)}`"
                          @click="copyPublicKey(k)"
                        >
                          Copy public key
                        </button>
                        <GateButton
                          v-if="k.state === 'active'"
                          op="retireLedgerSigningKey"
                          small
                          :blocked-reason="retireBlockedReason(keysList, k)"
                          @click="dialog = { kind: 'retire', key: k }"
                          >Retire now</GateButton
                        >
                        <GateButton
                          v-if="k.state !== 'revoked'"
                          op="revokeLedgerSigningKey"
                          small
                          variant="danger"
                          :blocked-reason="revokeBlockedReason(keysList, k)"
                          @click="dialog = { kind: 'revoke', key: k }"
                          >Revoke</GateButton
                        >
                        <span v-if="copied === k.fingerprint" role="status">Copied.</span>
                      </div>
                    </td>
                  </tr>
                </tbody>
              </table>
            </TableWrap>
          </QueryBoundary>
        </section>

        <section class="card stack" aria-labelledby="holders-h">
          <h2 id="holders-h">Servers that hold the key</h2>
          <p class="muted">
            The manager leader and the servers that may become it. A server that is not listed never
            has the file. A server stays listed until its file was removed.
          </p>
          <p v-if="data.holders.length === 0" class="muted" data-testid="no-holders">
            No server holds a key.
          </p>
          <ul v-else class="plain" data-testid="holders">
            <li v-for="h in data.holders" :key="h">
              <RouterLink
                v-if="canServer && isAddressableSegment(h)"
                :to="{ name: 'server', params: { id: h } }"
                ><SafeName :value="h" kind="text" :max="60"
              /></RouterLink>
              <SafeName v-else :value="h" kind="text" :max="60" />
            </li>
          </ul>
        </section>

        <section class="card stack" aria-labelledby="trust-h">
          <div class="row-between">
            <h2 id="trust-h">Trust file</h2>
            <GateButton
              op="getLedgerTrustFile"
              :disabled="trust.isPending.value"
              :blocked-reason="hasKeys ? '' : 'No key exists, so there is no trust file.'"
              @click="trust.mutate()"
              >Download trust file</GateButton
            >
          </div>
          <p>
            The trust file holds the public signing keys and the origin prefix. It contains no
            secret. It is not a source of first trust: compare the fingerprint above with one that
            you obtained out of band (read aloud or on paper) before an auditor relies on it.
          </p>
          <ProblemNotice v-if="trust.error.value" :error="trust.error.value" />
          <p v-if="trust.isSuccess.value" class="muted" role="status" data-testid="trust-saved">
            The trust file was saved as trust.json.
          </p>
          <h3>What to keep outside the cluster</h3>
          <p class="muted">
            Verification means something only against evidence that the administrators of this
            cluster cannot change.
          </p>
          <ol data-testid="keep-outside">
            <li v-for="t in KEEP_OUTSIDE" :key="t">{{ t }}</li>
          </ol>
        </section>

        <section class="card stack" aria-labelledby="events-h">
          <h2 id="events-h">Signed events</h2>
          <QueryBoundary
            :loading="false"
            :error="null"
            :empty="data.events.length === 0"
            empty-title="No events yet"
            empty-hint="Every key that is added, retired or revoked is recorded as a signed event."
          >
            <TableWrap>
              <table class="table">
                <caption class="sr-only">
                  Signed events of the key history
                </caption>
                <thead>
                  <tr>
                    <th scope="col" class="num">#</th>
                    <th scope="col">Event</th>
                    <th scope="col">Time</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="e in data.events" :key="e.seq">
                    <th scope="row" class="num">{{ e.seq }}</th>
                    <td data-testid="event-type">
                      <template v-if="eventTypeLabel(e.type)"
                        >{{ eventTypeLabel(e.type) }}
                        <span class="muted mono"
                          >(<SafeName :value="e.type" kind="text" :max="40" />)</span
                        ></template
                      >
                      <SafeName v-else :value="e.type" kind="text" :max="60" />
                    </td>
                    <td>{{ formatDateTime(e.time) }}</td>
                  </tr>
                </tbody>
              </table>
            </TableWrap>
          </QueryBoundary>
        </section>
      </template>
    </QueryBoundary>

    <CreateKeyDialog
      v-if="dialog?.kind === 'create'"
      :first="dialog.first"
      :mode="data?.mode === 'command' ? 'command' : 'file'"
      :origin-prefix="prefixNow"
      @close="dialog = null"
    />
    <RetireKeyDialog
      v-if="dialog?.kind === 'retire'"
      :key-row="dialog.key"
      @close="dialog = null"
    />
    <RevokeKeyDialog
      v-if="dialog?.kind === 'revoke'"
      :key-row="dialog.key"
      @close="dialog = null"
    />
  </div>
</template>

<style scoped>
.facts {
  display: grid;
  gap: var(--s-3);
  grid-template-columns: repeat(auto-fit, minmax(16em, 1fr));
  margin: 0;
}
.facts dt {
  font-weight: 600;
}
.facts dd {
  margin: 0;
}
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-1);
}
</style>
