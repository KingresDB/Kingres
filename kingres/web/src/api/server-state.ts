// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// One place that ends the use of server data. Everything in the client that holds API data
// (the query cache, requests in flight, task followers, stores) registers a function here, and
// `resetServerState` runs all of them. Every way a session can end calls it (sign-out, expiry on
// any request, a different user in the session, another tab), so none of them can forget a part.
const resets = new Set<() => void>()

/** Registers a function that drops server data. Returns a function that unregisters it. */
export function registerServerStateReset(reset: () => void): () => void {
  resets.add(reset)
  return () => resets.delete(reset)
}

/** Drops every piece of server data the client holds and cancels everything still running. */
export function resetServerState(): void {
  for (const reset of [...resets]) {
    try {
      reset()
    } catch (error) {
      // One failing reset must not leave the others undone.
      console.error(error)
    }
  }
}

let sessionLostHandler: (() => void) | null = null

/** The application sets this once; it sends the user to the sign-in page. */
export function setSessionLostHandler(handler: (() => void) | null): void {
  sessionLostHandler = handler
}

/** Called by the client for any response that says the session is gone. */
export function reportSessionLost(): void {
  sessionLostHandler?.()
}
