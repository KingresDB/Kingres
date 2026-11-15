// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Number, size and time formatting. Output is stable (fixed locale) so tests do not depend on the
// machine.

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const

/** 1536 -> "1.5 KiB". */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined) return '-'
  let value = Math.abs(bytes)
  let unit = 0
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  const text = unit === 0 ? String(Math.round(value)) : value.toFixed(value >= 100 ? 0 : 1)
  return `${bytes < 0 ? '-' : ''}${text} ${UNITS[unit]}`
}

/** 12345 -> "12,345". */
export function formatNumber(n: number | null | undefined, fractionDigits = 0): string {
  if (n === null || n === undefined) return '-'
  return n.toLocaleString('en-US', {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  })
}

/** 93784 -> "1d 2h". Shows at most two units. */
export function formatDuration(totalSeconds: number | null | undefined): string {
  if (totalSeconds === null || totalSeconds === undefined) return '-'
  const s = Math.max(0, Math.floor(totalSeconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m ${s % 60}s`
  return `${s}s`
}

/** Replication lag, shown in the most useful unit. */
export function formatLag(seconds: number | null | undefined, bytes?: number | null): string {
  if (seconds === null || seconds === undefined) {
    return bytes === null || bytes === undefined ? '-' : formatBytes(bytes)
  }
  if (seconds < 1) return `${Math.round(seconds * 1000)} ms`
  return seconds < 120 ? `${seconds.toFixed(1)} s` : formatDuration(seconds)
}

const dateTime = new Intl.DateTimeFormat('en-GB', {
  dateStyle: 'medium',
  timeStyle: 'medium',
  timeZone: 'UTC',
})

/** "14 Nov 2026, 13:05:09 UTC". All API timestamps are UTC. */
export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : `${dateTime.format(d)} UTC`
}

/** "3 min ago". */
export function formatAgo(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return '-'
  const diff = Math.round((now - new Date(iso).getTime()) / 1000)
  if (Number.isNaN(diff)) return iso
  if (diff < 5) return 'just now'
  if (diff < 90) return `${diff}s ago`
  if (diff < 5400) return `${Math.round(diff / 60)} min ago`
  if (diff < 129600) return `${Math.round(diff / 3600)} h ago`
  return `${Math.round(diff / 86400)} d ago`
}

/** Shortens a 64-character hash for tables: "ab12cd34…ef56". Full value goes in a title. */
export function shortHash(hash: string | null | undefined): string {
  if (!hash) return '-'
  return hash.length > 16 ? `${hash.slice(0, 8)}…${hash.slice(-6)}` : hash
}

export function percent(n: number | null | undefined): string {
  return n === null || n === undefined ? '-' : `${n.toFixed(n < 10 ? 1 : 0)}%`
}
