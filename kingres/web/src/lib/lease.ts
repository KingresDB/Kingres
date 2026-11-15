// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Words about the lease of a shard group (contract 0.7.0).

/** "7.2 s" for a lease with time left, "expired" at zero, and an empty text when the server stated no time. */
export function formatLease(remainingMillis: number | null | undefined): string {
  if (remainingMillis === null || remainingMillis === undefined) return ''
  if (!Number.isFinite(remainingMillis)) return ''
  if (remainingMillis <= 0) return 'no time'
  if (remainingMillis < 1000) return `${Math.round(remainingMillis)} ms`
  return `${(remainingMillis / 1000).toFixed(1)} s`
}
