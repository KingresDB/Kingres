// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Theme preference. "system" follows the operating system; light/dark override it. The design
// tokens in styles/tokens.css react to the `data-theme` attribute set here.
import { ref, watch } from 'vue'
import { defineStore } from 'pinia'

export type ThemeMode = 'system' | 'light' | 'dark'
const STORAGE_KEY = 'kingres.theme'

function readStored(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // Storage can be blocked (private window); the default is fine.
  }
  return 'system'
}

export const useThemeStore = defineStore('theme', () => {
  const mode = ref<ThemeMode>(readStored())

  function apply(): void {
    const root = document.documentElement
    if (mode.value === 'system') root.removeAttribute('data-theme')
    else root.setAttribute('data-theme', mode.value)
  }

  watch(
    mode,
    (value) => {
      apply()
      try {
        localStorage.setItem(STORAGE_KEY, value)
      } catch {
        // Ignore: the preference simply is not remembered.
      }
    },
    { immediate: true },
  )

  /** Cycles system -> light -> dark -> system. */
  function cycle(): void {
    mode.value = mode.value === 'system' ? 'light' : mode.value === 'light' ? 'dark' : 'system'
  }

  return { mode, cycle }
})
