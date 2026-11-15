// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Certificate fingerprints (SHA-256, hex) and certificate expiry, as the server pages show them.

const FINGERPRINT_RE = /^[0-9a-f]{64}$/

/** Removes colons and white space and lower-cases: the form the server compares. */
export function normalizeFingerprint(text: string): string {
  return text.replace(/[:\s]/g, '').toLowerCase()
}

/** A message for a fingerprint that cannot be right, or undefined (an empty value is allowed). */
export function fingerprintError(text: string): string | undefined {
  if (text.trim() === '') return undefined
  return FINGERPRINT_RE.test(normalizeFingerprint(text))
    ? undefined
    : 'A SHA-256 fingerprint has 64 hexadecimal digits (0-9, a-f); colons between byte pairs are allowed.'
}

/** The 64 digits in groups of eight, so that two fingerprints can be compared by eye. */
export function groupFingerprint(hex: string): string {
  return hex.replace(/(.{8})(?=.)/g, '$1 ')
}

export type CertificateState = 'unknown' | 'ok' | 'expiring' | 'expired'

/** Days before the end of a certificate from which the panel flags it. */
export const CERTIFICATE_WARN_DAYS = 30

export function certificateState(
  expiresAt: string | null | undefined,
  now: number = Date.now(),
): CertificateState {
  if (!expiresAt) return 'unknown'
  const end = Date.parse(expiresAt)
  if (Number.isNaN(end)) return 'unknown'
  if (end <= now) return 'expired'
  return end - now <= CERTIFICATE_WARN_DAYS * 86_400_000 ? 'expiring' : 'ok'
}
