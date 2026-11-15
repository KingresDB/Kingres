// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Liveness from heartbeats (contract 0.7.0): `Server.status` is `suspect` or `dead` when the answering server has
// heard nothing from the server for the suspect or the failure timeout, and `lastHeartbeatAt` says when it did.
import type { Server } from '@/api/types'
import { formatAgo, formatDateTime } from './format'

/** True for a server that the answering server hears (a status that is not about silence may still be not live). */
export const isLive = (s: Pick<Server, 'status'>): boolean => s.status === 'live'

/**
 * How long ago the last heartbeat was: "7s ago (14 Nov 2026, 13:05:09 UTC)", or null when no heartbeat is on record.
 * Kept short for a table cell; the exact time is in \`text\` too.
 */
export function heartbeatAge(
  s: Pick<Server, 'lastHeartbeatAt'>,
  now: number = Date.now(),
): { ago: string; text: string } | null {
  if (!s.lastHeartbeatAt) return null
  const ago = formatAgo(s.lastHeartbeatAt, now)
  return { ago, text: `${ago} (${formatDateTime(s.lastHeartbeatAt)})` }
}
