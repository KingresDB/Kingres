// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Saves text as a file in the browser. The text is written as given: no re-serialization, no
// added newline, no byte order mark (a JSON document that a verifier reads must stay as it came).

/** Starts a download of `text` as `filename` (UTF-8, `application/json`). */
export function saveTextFile(filename: string, text: string, type = 'application/json'): void {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  link.rel = 'noopener'
  document.body.appendChild(link)
  link.click()
  link.remove()
  // The browser has taken the blob when click() returns; release it a moment later.
  window.setTimeout(() => URL.revokeObjectURL(url), 10_000)
}
