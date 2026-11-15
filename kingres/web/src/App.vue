<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { ref } from 'vue'
import { RouterView, useRouter } from 'vue-router'
import SplashScreen from '@/components/SplashScreen.vue'
import LeftClusterNotice from '@/components/this-server/LeftClusterNotice.vue'

// The first navigation checks the setup state and the session; show the splash meanwhile.
const router = useRouter()
const ready = ref(false)
void router.isReady().then(() => {
  ready.value = true
})
</script>

<template>
  <SplashScreen v-if="!ready" />
  <template v-else>
    <LeftClusterNotice />
    <RouterView />
  </template>
</template>
