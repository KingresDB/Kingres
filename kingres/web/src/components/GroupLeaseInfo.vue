<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The lease of one shard group (contract 0.7.0, `ShardGroup.lease`): whether the primary holds it, whether losing it
// stops the instance (the group has a standby: "enforced") or only raises an alert ("watched"), who holds it and
// for how long. The state is what the server that answered knows, which the text says. A lost enforced lease is said
// in plain words: the primary was stopped on purpose.
import { computed } from 'vue'
import StatusBadge from './StatusBadge.vue'
import SafeName from './SafeName.vue'
import { LEASE_STATE } from '@/lib/status'
import { formatLease } from '@/lib/lease'
import type { GroupLease } from '@/api/types'

const props = defineProps<{ lease: GroupLease | undefined }>()
const state = computed(() => props.lease?.state)
const remaining = computed(() => formatLease(props.lease?.remainingMillis))
</script>

<template>
  <div class="lease stack" data-testid="group-lease" :data-lease-state="state ?? 'absent'">
    <p v-if="!lease" class="muted" data-testid="lease-absent">
      Lease: this server does not report leases (it is an older revision, or it knows nothing about
      this group).
    </p>
    <template v-else>
      <p class="row">
        <StatusBadge :meta="LEASE_STATE[lease.state]" />
        <span
          class="badge"
          :class="lease.enforced ? 'tone-info' : 'tone-neutral'"
          data-testid="lease-enforced"
        >
          <span aria-hidden="true">{{ lease.enforced ? '⚑' : '○' }}</span>
          {{ lease.enforced ? 'Enforced' : 'Watched only' }}
        </span>
      </p>
      <p v-if="lease.state === 'held'" data-testid="lease-text">
        The primary on server <SafeName :value="lease.holder" kind="text" :max="60" /> holds the
        lease<template v-if="remaining">
          ; <span data-testid="lease-remaining">{{ remaining }}</span> left when this page was last
          refreshed (it is renewed every two seconds)</template
        >.
      </p>
      <p v-else-if="lease.state === 'granted'" data-testid="lease-text">
        The server that answered granted the lease to server
        <SafeName :value="lease.holder" kind="text" :max="60" /><template v-if="remaining">
          ; <span data-testid="lease-remaining">{{ remaining }}</span> left when this page was last
          refreshed</template
        >. It does not run this primary itself, so this is what it knows, not a check of the
        primary.
      </p>
      <p v-else-if="lease.state === 'lost' && lease.enforced" data-testid="lease-text">
        <strong>The primary was stopped on purpose.</strong> The server that runs it could not renew
        the lease from a majority of the servers in time, and this group has a standby, so it
        stopped the primary: that way two primaries can never run for one group. The primary starts
        again when the lease is obtained again.
      </p>
      <p v-else-if="lease.state === 'lost'" data-testid="lease-text">
        <strong>The lease was lost, but the primary keeps running.</strong> This group has no
        standby, so stopping its only copy would help nobody; the alert "Primary lost its lease" is
        the only effect.
      </p>
      <p v-else data-testid="lease-text">
        No server holds a lease for this group as far as the server that answered knows.
      </p>
      <p v-if="lease.state !== 'lost'" class="muted" data-testid="lease-enforcement">
        <template v-if="lease.enforced"
          >Enforced: the group has a standby, so a primary that loses the lease is
          stopped.</template
        >
        <template v-else
          >Watched only: the group has no standby, so losing the lease raises an alert and nothing
          is stopped.</template
        >
      </p>
    </template>
  </div>
</template>
