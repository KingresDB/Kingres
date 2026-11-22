<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The answer to a change of a table that was refused. 422 `table_not_eligible` names the lines of the report of the sharding
// extension that stop the table (`Problem.blockers`); they are listed under the general words. Every other problem is the
// general notice.
import { computed } from 'vue'
import ProblemNotice from '../ProblemNotice.vue'
import TableBlockerList from './TableBlockerList.vue'
import { ApiError } from '@/api/problem'

const props = defineProps<{ error: unknown }>()
const blockers = computed(() =>
  props.error instanceof ApiError && props.error.code === 'table_not_eligible'
    ? (props.error.problem.blockers ?? [])
    : [],
)
</script>

<template>
  <div class="stack" data-testid="table-change-refusal">
    <ProblemNotice :error="error" />
    <TableBlockerList :lines="blockers" tone="blocker" heading="What stops this table" />
  </div>
</template>
