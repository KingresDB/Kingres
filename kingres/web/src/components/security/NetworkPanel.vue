<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Network access rules (the pg_hba.conf rules, applied on every server). The list is replaced as
// a whole with optimistic concurrency (`revision`). Replacing it changes every server at once and
// can lock everybody out, so it is only ever done from a confirmation that shows what changes.
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import TableWrap from '../TableWrap.vue'
import TaskActionDialog from '../TaskActionDialog.vue'
import SafeName from '../SafeName.vue'
import HiddenPreview from '../HiddenPreview.vue'
import ProblemNotice from '../ProblemNotice.vue'
import { ApiError } from '@/api/problem'
import { endpoints } from '@/api/endpoints'
import { keys, useRoles } from '@/api/queries'
import { vBlockEnterSubmit } from '@/lib/forms'
import {
  allowsAnyAdministrator,
  describeRule,
  diffRules,
  evaluateAccess,
  formatList,
  isUnchanged,
  parseList,
} from '@/lib/network-rules'
import { useSessionStore } from '@/stores/session'
import type { NetworkAccessRule } from '@/api/types'

const session = useSessionStore()
const access = useQuery({ queryKey: keys.networkAccess, queryFn: endpoints.getNetworkAccess })
const roles = useRoles()

interface EditableRule {
  type: NetworkAccessRule['type']
  databases: string
  users: string
  address: string
  method: NetworkAccessRule['method']
  comment: string
  /** The rule as the server has it; fields that were not edited are sent back untouched. */
  original: NetworkAccessRule | null
}
const rules = ref<EditableRule[]>([])
const tlsRequired = ref(false)
const dirty = ref(false)
const reviewing = ref<{
  revision: number
  tlsRequired: boolean
  rules: NetworkAccessRule[]
} | null>(null)

function load(): void {
  const d = access.data.value
  if (!d) return
  tlsRequired.value = d.tlsRequired
  rules.value = d.rules.map((r) => ({
    type: r.type,
    databases: formatList(r.databases),
    users: formatList(r.users),
    address: r.address,
    method: r.method,
    comment: r.comment ?? '',
    original: r,
  }))
  dirty.value = false
}
// Take over server data until the user starts editing; later refreshes must not overwrite edits.
watch(
  () => access.data.value,
  () => {
    if (!dirty.value) load()
  },
  { immediate: true },
)

/** The rule list the form describes. A field that was not edited keeps the server's exact value. */
function toRule(r: EditableRule): NetworkAccessRule {
  const o = r.original
  return {
    type: r.type,
    databases: o && r.databases === formatList(o.databases) ? o.databases : parseList(r.databases),
    users: o && r.users === formatList(o.users) ? o.users : parseList(r.users),
    address: r.address,
    method: r.method,
    comment: o && r.comment === (o.comment ?? '') ? (o.comment ?? null) : r.comment || null,
  }
}
const edited = computed(() => rules.value.map(toRule))

function addRule(): void {
  dirty.value = true
  rules.value.push({
    type: tlsRequired.value ? 'hostssl' : 'host',
    databases: 'all',
    users: 'all',
    address: '',
    method: 'scram-sha-256',
    comment: '',
    original: null,
  })
}
function removeRule(i: number): void {
  dirty.value = true
  rules.value.splice(i, 1)
}
function discard(): void {
  void access.refetch().then(() => {
    dirty.value = false
    load()
  })
}

// --- the confirmation ------------------------------------------------------------------------

/** The address the server saw this session come from (contract 0.6.0, `Session.clientAddress`). */
const sessionAddress = computed(() => session.session?.clientAddress ?? null)
// An older server does not report it: the address of the last panel action in the audit log is the fallback.
const ownAddress = useQuery({
  queryKey: computed(() => [...keys.audit, 'own-address', session.username]),
  queryFn: () => endpoints.listAuditEvents({ actor: session.username ?? '', limit: 20 }),
  enabled: computed(() => !!session.username && sessionAddress.value === null),
})
const knownAddress = computed(
  () =>
    sessionAddress.value ??
    ownAddress.data.value?.items.find((e) => e.source === 'panel' && e.actorAddress)
      ?.actorAddress ??
    null,
)
const administrators = computed(() => {
  const names = (roles.data.value?.items ?? [])
    .filter((r) => r.clusterRoles.includes('kingres_admin'))
    .map((r) => r.name)
  return session.username && !names.includes(session.username)
    ? [...names, session.username]
    : names
})

const review = computed(() => {
  const before = access.data.value?.rules ?? []
  const after = reviewing.value?.rules ?? []
  const diff = diffRules(before, after)
  const tlsChanged =
    (access.data.value?.tlsRequired ?? false) !== (reviewing.value?.tlsRequired ?? false)
  const address = knownAddress.value
  const user = session.username ?? ''
  const afterAccess = address ? evaluateAccess(after, address, user) : null
  const beforeAccess = address ? evaluateAccess(before, address, user) : null
  const lostAddress =
    afterAccess !== null && afterAccess.result !== 'allowed' && afterAccess.result !== 'unknown'
  return {
    diff,
    tlsChanged,
    unchanged: isUnchanged(diff) && !tlsChanged,
    empty: after.length === 0,
    noAdministrator: !allowsAnyAdministrator(after, administrators.value),
    address,
    lostAddress,
    wasAllowed: beforeAccess?.result === 'allowed',
    cannotEvaluate: afterAccess?.result === 'unknown',
    rejectedBy: afterAccess?.result === 'rejected' ? afterAccess.rule + 1 : null,
  }
})
// The server decides about the lock-out (its dry run). The panel's own address check is a hint: it
// asks for the typed confirmation only while the server has not said the rules are acceptable.
const serverAccepts = computed(() => check.value.state === 'valid')
const risky = computed(
  () =>
    review.value.empty ||
    review.value.noAdministrator ||
    (review.value.lostAddress && !serverAccepts.value),
)
const confirmDisabled = computed(
  () => check.value.state === 'checking' || check.value.state === 'refused',
)
const plural = (n: number, verb: string) => `${n} rule${n === 1 ? '' : 's'} ${verb}`

// --- the server's dry run (contract 0.6.0) ---------------------------------------------------

type Check =
  | { state: 'idle' }
  | { state: 'checking' }
  | { state: 'valid'; revision: number }
  | { state: 'refused'; error: ApiError }
  | { state: 'unavailable'; error: unknown }
const check = ref<Check>({ state: 'idle' })
let checkToken = 0

async function runCheck(r: {
  revision: number
  tlsRequired: boolean
  rules: NetworkAccessRule[]
}): Promise<void> {
  const mine = ++checkToken
  check.value = { state: 'checking' }
  try {
    const answer = await endpoints.checkNetworkAccess(r)
    if (mine === checkToken) check.value = { state: 'valid', revision: answer.revision }
  } catch (error) {
    if (mine !== checkToken) return
    // 400, 409 and 422 are the server refusing exactly this change; anything else (503, no answer) is
    // not an answer about the rules, so it must not block, and the real change is checked again anyway.
    check.value =
      error instanceof ApiError && [400, 409, 422].includes(error.status)
        ? { state: 'refused', error }
        : { state: 'unavailable', error }
  }
}
onBeforeUnmount(() => {
  checkToken += 1
})

function openReview(): void {
  const d = access.data.value
  if (!d) return
  reviewing.value = { revision: d.revision, tlsRequired: tlsRequired.value, rules: edited.value }
  void runCheck(reviewing.value)
}
function closeReview(): void {
  checkToken += 1
  check.value = { state: 'idle' }
  reviewing.value = null
}
const start = () => {
  const r = reviewing.value
  if (!r) throw new Error('Nothing to apply.')
  return endpoints.replaceNetworkAccess(r)
}
</script>

<template>
  <QueryBoundary
    :loading="access.isPending.value"
    :error="access.error.value"
    @retry="access.refetch()"
  >
    <form
      v-if="access.data.value"
      v-block-enter-submit
      class="stack"
      @submit.prevent
      @input="dirty = true"
    >
      <p class="muted">
        Revision {{ access.data.value.revision }}. Rules are checked from top to bottom; the first
        match wins. Nothing is applied until you review the change and confirm it.
      </p>
      <label class="check"
        ><input v-model="tlsRequired" type="checkbox" /> Require TLS (plain <code>host</code> rules
        are rejected)</label
      >

      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Network access rules
          </caption>
          <thead>
            <tr>
              <th scope="col">Type</th>
              <th scope="col">Databases</th>
              <th scope="col">Users</th>
              <th scope="col">Address</th>
              <th scope="col">Method</th>
              <th scope="col">Comment</th>
              <th scope="col"><span class="sr-only">Remove</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(r, i) in rules" :key="i">
              <td>
                <select v-model="r.type" :aria-label="`Rule ${i + 1} type`">
                  <option value="host">host</option>
                  <option value="hostssl">hostssl</option>
                  <option value="hostnossl">hostnossl</option>
                </select>
              </td>
              <td>
                <input
                  v-model="r.databases"
                  class="input mono"
                  :aria-label="`Rule ${i + 1} databases`"
                />
                <HiddenPreview :value="r.databases" />
              </td>
              <td>
                <input v-model="r.users" class="input mono" :aria-label="`Rule ${i + 1} users`" />
                <HiddenPreview :value="r.users" />
              </td>
              <td>
                <input
                  v-model="r.address"
                  class="input mono"
                  :aria-label="`Rule ${i + 1} address`"
                  placeholder="10.0.0.0/24"
                />
              </td>
              <td>
                <select v-model="r.method" :aria-label="`Rule ${i + 1} method`">
                  <option value="scram-sha-256">scram-sha-256</option>
                  <option value="cert">cert (client certificate)</option>
                  <option value="reject">reject</option>
                </select>
              </td>
              <td>
                <input v-model="r.comment" class="input" :aria-label="`Rule ${i + 1} comment`" />
                <HiddenPreview :value="r.comment" />
              </td>
              <td>
                <button
                  type="button"
                  class="btn btn-small"
                  :aria-label="`Remove rule ${i + 1}`"
                  @click="removeRule(i)"
                >
                  Remove
                </button>
              </td>
            </tr>
            <tr v-if="rules.length === 0">
              <td colspan="7" class="muted">No rules. Nobody can connect over the network.</td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <p class="hint muted">
        Several names in a field are separated by commas. Put a name that contains a comma, a space
        or a quote in double quotes.
      </p>

      <div class="row">
        <button type="button" class="btn" @click="addRule">Add rule</button>
        <GateButton
          op="replaceNetworkAccess"
          variant="primary"
          :disabled="!dirty"
          @click="openReview"
          >Review and apply</GateButton
        >
        <button v-if="dirty" type="button" class="btn" @click="discard">Discard changes</button>
      </div>

      <section aria-labelledby="apply-h">
        <h2 id="apply-h">Applied revision per server</h2>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Applied revision per server
            </caption>
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col" class="num">Revision</th>
                <th scope="col">Result</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in access.data.value.applyStatus" :key="s.serverId">
                <th scope="row">Server <SafeName :value="s.serverId" kind="text" :max="60" /></th>
                <td class="num">{{ s.appliedRevision }}</td>
                <td>
                  <span v-if="s.error" class="badge wrap tone-danger"
                    ><span aria-hidden="true">&#10006;</span>
                    <SafeName :value="s.error" kind="text" :max="300"
                  /></span>
                  <span
                    v-else-if="s.appliedRevision === access.data.value.revision"
                    class="badge tone-ok"
                    ><span aria-hidden="true">&#10003;</span> Applied</span
                  >
                  <span v-else class="badge tone-info"
                    ><span aria-hidden="true">&#9680;</span> Pending</span
                  >
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>
    </form>
  </QueryBoundary>

  <TaskActionDialog
    v-if="reviewing"
    title="Apply the network access rules on every server?"
    confirm-label="Apply on all servers"
    danger
    wide
    :type-to-confirm="risky ? 'lock out' : undefined"
    :confirm-disabled="confirmDisabled"
    :start="start"
    :invalidate="[keys.networkAccess, keys.audit]"
    @close="closeReview"
    @done="dirty = false"
  >
    <p>
      The rules replace the current ones (revision {{ reviewing.revision }}) on
      <strong>every server</strong>, and the first matching rule decides who may connect.
    </p>

    <div data-testid="server-check">
      <p v-if="check.state === 'checking'" class="notice tone-info" role="status">
        <span aria-hidden="true">&#9680;</span>
        <span>Asking the server whether it accepts these rules. Nothing is changed by this.</span>
      </p>
      <p v-else-if="check.state === 'valid'" class="notice tone-ok" role="status">
        <span aria-hidden="true">&#10003;</span>
        <span
          >The server checked these rules against revision {{ check.revision }} and accepts them,
          including the check that they do not lock out your own address.
          <strong>Nothing is saved yet</strong>: the rules apply only when you confirm below.</span
        >
      </p>
      <template v-else-if="check.state === 'refused'">
        <ProblemNotice :error="check.error" />
        <p class="notice tone-danger" role="alert">
          <span aria-hidden="true">&#10006;</span>
          <span
            >The server would refuse this change, so it cannot be applied.<template
              v-if="check.error.status === 409"
            >
              The rules were changed by someone else after you opened this page: close this window,
              use "Discard changes" to load the current rules, and make your change again.</template
            ><template v-else>
              Close this window, change the rules and review again.</template
            ></span
          >
        </p>
      </template>
      <p v-else-if="check.state === 'unavailable'" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span
          >The server could not check these rules just now, so this window cannot tell you whether
          it accepts them. You can still apply them: the server checks them again and refuses a
          change that locks out your address.</span
        >
      </p>
    </div>
    <div v-if="review.empty" class="notice tone-danger" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span>The new list is empty. Nobody will be able to connect over the network.</span>
    </div>
    <div v-if="review.noAdministrator" class="notice tone-danger" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span>
        The new list has no rule that allows administrators to connect, so nobody could repair the
        rules afterwards.
      </span>
    </div>
    <div
      v-if="review.lostAddress && review.address"
      class="notice"
      :class="serverAccepts ? 'tone-warn' : 'tone-danger'"
      :role="serverAccepts ? 'status' : 'alert'"
      data-testid="address-hint"
    >
      <span aria-hidden="true">{{ serverAccepts ? '\u25B2' : '\u2716' }}</span>
      <span v-if="serverAccepts"
        ><strong>Hint only (the server accepts the rules):</strong>&nbsp;</span
      >
      <span v-if="review.wasAllowed">
        The new rules no longer allow the address you connect from (<code
          ><SafeName :value="review.address" kind="text" :max="60" /></code
        >)<template v-if="review.rejectedBy"> : rule {{ review.rejectedBy }} rejects it</template
        ><template v-else>: no rule matches it</template>.
      </span>
      <span v-else>
        The new rules do not allow the address you connect from (<code
          ><SafeName :value="review.address" kind="text" :max="60" /></code
        >), and neither did the old ones.
      </span>
    </div>
    <div v-if="review.cannotEvaluate" class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span>
        A rule names a group of roles (<code>+name</code>) or a file (<code>@name</code>). The panel
        cannot tell whether it lets you in.
      </span>
    </div>
    <div v-if="!review.address" class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span>
        The panel cannot tell which address you connect from: the server does not report it, and the
        audit log has no earlier panel action of yours with an address. Check the new rules against
        your own address before you apply them.
      </span>
    </div>

    <p v-if="review.unchanged" class="muted">Nothing differs from the rules on the servers.</p>
    <div v-else class="stack" data-testid="rule-diff">
      <ul class="plain-list">
        <li v-if="review.tlsChanged">
          Require TLS: <strong>{{ access.data.value?.tlsRequired ? 'on' : 'off' }}</strong> to
          <strong>{{ reviewing.tlsRequired ? 'on' : 'off' }}</strong>
        </li>
        <li v-if="review.diff.removed.length">
          <strong>{{ plural(review.diff.removed.length, 'removed') }}</strong>
          <ul>
            <li v-for="(r, i) in review.diff.removed" :key="`r${i}`">
              <SafeName :value="describeRule(r)" kind="text" :max="300" />
            </li>
          </ul>
        </li>
        <li v-if="review.diff.added.length">
          <strong>{{ plural(review.diff.added.length, 'added') }}</strong>
          <ul>
            <li v-for="(r, i) in review.diff.added" :key="`a${i}`">
              <SafeName :value="describeRule(r)" kind="text" :max="300" />
            </li>
          </ul>
        </li>
        <li v-if="review.diff.changed.length">
          <strong>{{ plural(review.diff.changed.length, 'changed') }}</strong>
          <ul>
            <li v-for="(c, i) in review.diff.changed" :key="`c${i}`">
              <SafeName :value="describeRule(c.before)" kind="text" :max="300" /><br />
              <span aria-hidden="true">&rarr;</span>
              <span class="sr-only">changed to</span>
              <SafeName :value="describeRule(c.after)" kind="text" :max="300" />
            </li>
          </ul>
        </li>
        <li v-if="review.diff.reordered">
          The rules are the same but in a different order. The first match wins, so the order
          decides who may connect.
        </li>
      </ul>
    </div>
    <p class="hint muted">
      The panel's own quick check (a hint; the server's check above decides) compares your address
      and role name with each rule from the top. It does not look at the database, at TLS, or at
      role groups, and the panel does not know whether these rules apply to the connection the panel
      itself uses. A proxy in front of the server can make the address in the audit log differ from
      yours.
    </p>
  </TaskActionDialog>
</template>

<style scoped>
.plain-list,
.plain-list ul {
  margin: 0;
  padding-left: var(--s-4);
}
</style>
