<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Why a conversion or the runs of a policy are refused for a table whose owner is not a superuser (contract 0.18.0, `tableOwner`
// and `ownerTrusted`, the blocker `owner_not_trusted`, the cluster setting `analytics.allowNonSuperuserOwners`): the code of the
// owner of a table (an index expression, a check, a default) runs as the service role during a conversion or a run, and a policy
// is repeated by the scheduler with nobody watching. The notice names the owner and links to the setting for a role that may read it.
import { RouterLink } from 'vue-router'
import SafeName from '../SafeName.vue'
import { useSessionStore } from '@/stores/session'

defineProps<{ owner?: string | null }>()
const session = useSessionStore()
</script>

<template>
  <div class="notice tone-warn" role="alert" data-testid="owner-not-trusted">
    <span aria-hidden="true">&#9650;</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong>The owner of this table is not trusted</strong>
      <span>
        <template v-if="owner"
          >The table is owned by <code><SafeName :value="owner" kind="name" :max="60" /></code>, who
          is not a superuser.
        </template>
        A conversion and every run of a policy run the code of the owner of a table (an index
        expression, a check, a default) as the service role, a superuser, and a policy is repeated
        by the scheduler without a person watching. So they are refused for this table, and a
        scheduled run raises one alert and is not tried again.
      </span>
      <span>
        If you trust this owner, turn on "Run the code of table owners who are not superusers" in
        the
        <RouterLink v-if="session.can('getClusterSettings')" :to="{ name: 'settings' }"
          >cluster settings</RouterLink
        ><template v-else
          >cluster settings (a kingres_admin or kingres_ops role reads them)</template
        >.
      </span>
    </div>
  </div>
</template>
