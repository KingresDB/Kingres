// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The one-line summary that the workbench shows above the text of an EXPLAIN (phase 12, section 13 of the query design, and the
// phase 2b item "EXPLAIN path summary"). It is read from the text lines that the sharding extension prints (`Pushed:`,
// `Repartition:`, `Pushdown:`, `Rewritten:`, `Remote groups:`); it is a convenience for the reader and is NEVER used for any
// decision. A text with no line that matches gives no summary at all, and an unknown line is not guessed at.
import type { QueryResult } from '@/api/types'

export interface PlanSummary {
  /** "repartitioned join", "colocated join", "partial aggregate", "pushed aggregate", "pushed query" or "scan". */
  path: string
  /** Why a faster path was not used, from a `Pushdown: not used:` or `Repartition: not used:` line. */
  notUsed: string[]
  /** "count(DISTINCT) -> approximate", from a `Rewritten:` line. */
  rewritten: string[]
  /** "2 of 3" from `Remote groups:` or `Groups:`, when the text says it. */
  groups: string | null
}

const MAX_LINE = 300

/** The plan lines of a result of EXPLAIN (first column `QUERY PLAN`), or null when the result is not one. */
export function planLines(result: Pick<QueryResult, 'columns' | 'rows'>): string[] | null {
  const first = result.columns[0]
  if (!first || first.name !== 'QUERY PLAN') return null
  const lines: string[] = []
  for (const row of result.rows) {
    const cell = row[0]
    // A plan in JSON or YAML format comes as one cell of text with its own lines; the text formats come as one row per line.
    if (typeof cell === 'string') lines.push(...cell.split(/\r?\n/))
  }
  return lines
}

const clip = (s: string): string => (s.length > MAX_LINE ? `${s.slice(0, MAX_LINE)}...` : s)

/** Reads the summary, or returns null when no line is recognized. */
export function summarizePlan(lines: string[]): PlanSummary | null {
  let path: string | null = null
  const notUsed: string[] = []
  const rewritten: string[] = []
  let groups: string | null = null
  const set = (p: string, strong = false) => {
    // The most specific path wins: a repartitioned join is also a join.
    const rank = [
      'scan',
      'pushed query',
      'pushed aggregate',
      'partial aggregate',
      'colocated join',
      'repartitioned join',
    ]
    if (path === null || strong || rank.indexOf(p) > rank.indexOf(path)) path = p
  }
  for (const raw of lines) {
    const line = raw.trim()
    let m = /^Pushed:\s*(.*)$/i.exec(line)
    if (m) {
      const what = m[1] ?? ''
      if (/^join\b/i.test(what))
        set(/repartitioned/i.test(what) ? 'repartitioned join' : 'colocated join')
      else if (/^aggregate\b.*\bpartial\b/i.test(what)) set('partial aggregate')
      else if (/^aggregate\b/i.test(what)) set('pushed aggregate')
      else set('pushed query')
      continue
    }
    m = /^Repartition:\s*(.*)$/i.exec(line)
    if (m) {
      const what = m[1] ?? ''
      const not = /^not used:\s*(.*)$/i.exec(what)
      if (not) notUsed.push(clip(not[1] ?? ''))
      else set('repartitioned join')
      continue
    }
    m = /^Pushdown:\s*not used:\s*(.*)$/i.exec(line)
    if (m) {
      notUsed.push(clip(m[1] ?? ''))
      continue
    }
    m = /^Rewritten:\s*(.*)$/i.exec(line)
    if (m) {
      const text = (m[1] ?? '')
        .replace(/\s*\(p\s*=\s*\d+\)\s*$/i, '')
        .replace(/\bcount\(DISTINCT\s+\w+\)\s*->\s*approximate/i, 'count(DISTINCT) -> approximate')
      rewritten.push(clip(text))
      continue
    }
    m = /^(?:Remote groups|Groups):\s*(\d+)\s+of\s+(\d+)/i.exec(line)
    if (m && groups === null) groups = `${m[1]} of ${m[2]}`
    if (/Custom Scan \(Kingres Distributed Scan\)/i.test(line)) set('scan')
    if (/Custom Scan \(Kingres Fragment Scan\)/i.test(line)) set('pushed query')
  }
  if (path === null && notUsed.length === 0 && rewritten.length === 0) return null
  // A statement with a `not used` line and no pushed path went through the scan.
  return { path: path ?? 'scan', notUsed, rewritten, groups }
}

/** The text of the summary, as one short sentence list: "Path: colocated join. Not used: ... Rewritten: ...". */
export function summaryText(s: PlanSummary): string {
  const parts = [`Path: ${s.path}`]
  for (const n of s.notUsed) parts.push(`Not used: ${n}`)
  for (const r of s.rewritten) parts.push(`Rewritten: ${r}`)
  return parts.join('. ') + '.'
}
