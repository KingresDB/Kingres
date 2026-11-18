/*-------------------------------------------------------------------------
 *
 * ed25519_check.h
 *	  The check of an Ed25519 public key that the registry makes (L21-01).
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/ed25519_check.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KL_ED25519_CHECK_H
#define KL_ED25519_CHECK_H

#include "c.h"

/*
 * Why a public key is refused (KL_KEY_OK when it is not).
 */
typedef enum KlKeyDefect
{
	KL_KEY_OK = 0,
	KL_KEY_NOT_CANONICAL,		/* y is not below 2^255 - 19, or x is zero with the sign bit set */
	KL_KEY_NOT_ON_CURVE,		/* no point of the curve has this y */
	KL_KEY_SMALL_ORDER			/* the point has an order that divides 8 */
} KlKeyDefect;

/*
 * Judge a 32-byte Ed25519 public key.  A key is acceptable when it is the
 * canonical encoding (RFC 8032, 5.1.2) of a point of the curve, and the point
 * is not one of the eight points of order 1, 2, 4 or 8.  Every acceptable key
 * has one encoding, so one point has one key identifier.  No OpenSSL is needed.
 */
extern KlKeyDefect kl_ed25519_key_defect(const uint8 *key);

/* The same, remembering the acceptable keys of the session (for ledger_attest). */
extern KlKeyDefect kl_ed25519_key_defect_cached(const uint8 *key);

/* A sentence for the defect, for the message of the error. */
extern const char *kl_ed25519_key_defect_text(KlKeyDefect defect);

#endif							/* KL_ED25519_CHECK_H */
