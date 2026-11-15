// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Set to "off" to disable the mock API in development and use the Vite proxy instead. */
  readonly VITE_MOCKS?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
