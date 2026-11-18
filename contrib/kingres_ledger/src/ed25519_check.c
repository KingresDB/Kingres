/*-------------------------------------------------------------------------
 *
 * ed25519_check.c
 *	  Which 32 bytes may be registered as an Ed25519 public key (L21-01).
 *
 * Plain Ed25519 verification (RFC 8032, and OpenSSL's EVP_DigestVerify)
 * accepts a signature that nobody made under a public key of small order: for
 * the identity point the signature R = identity, S = 0 verifies for every
 * message, and for the other points of order 2, 4 and 8 a signature verifies
 * for a share of the messages that the caller reaches by choosing the nonce,
 * which is part of the message.  Several encodings give the same point, and a
 * revocation of one key identifier would leave the others alive.  So the
 * registry accepts only the canonical encoding of a point of the curve that
 * does not have small order, and this file is that check.  It is written
 * here, from the definitions, and uses nothing but integer arithmetic: the
 * extension has it with and without OpenSSL.
 *
 * What is checked, with p = 2^255 - 19 and the curve  -x^2 + y^2 = 1 + d x^2 y^2,
 * d = -121665 / 121666 (the twisted Edwards form of Curve25519):
 *
 *	  1. canonical: the 255-bit number y of the key (the low 255 bits, little
 *		 endian) is below p; and x is not zero with the sign bit set (the
 *		 sign bit selects between x and -x, and zero has one value).  Then
 *		 each point has one encoding.
 *	  2. on the curve: x^2 = (y^2 - 1) / (d y^2 + 1) must be a square mod p
 *		 (Euler: x2^((p-1)/2) is 0 or 1).  The denominator is never zero,
 *		 because d is not a square.
 *	  3. not of small order.  The group of the curve has order 8 * l, l an odd
 *		 prime, so the points whose order divides 8 are exactly the points P
 *		 with [8]P = identity, and there are eight of them.  The doubling
 *		 formula of the curve depends on y alone once x^2 is known:
 *			 y(2P) = (y^2 + x^2) / (2 - y^2 + x^2)
 *		 (from y3 = (y1 y2 + x1 x2) / (1 - d x1 x2 y1 y2) with the equation of
 *		 the curve; the denominator 1 - d x^2 y^2 is never zero, d being a
 *		 non-square).  The key has small order exactly when three doublings
 *		 of its y give y = 1: the identity is the only point with y = 1,
 *		 since y = 1 gives x^2 = 0.
 *
 * Why this is complete: a refused key is refused for one of the three reasons
 * above and an accepted key meets all three, and the three reasons are the
 * whole of what "a canonical encoding of a point of the curve that does not
 * have small order" says.  The eight small-order points are covered by (3)
 * whatever their encoding; their non-canonical encodings (y = p, p + 1, p + 2^k
 * aliases, a sign bit on x = 0) are covered by (1).  Points that have a
 * component of small order next to a component of order l are accepted:
 * their holder is a client that has the private scalar of a real key, which is
 * all the registry asks.  The tests of the extension compare this file with the
 * eight encodings that the literature lists, with their aliases and with a
 * value that is not on the curve.
 *
 * The numbers mod p are 32 limbs of 8 bits in an array of int64; every
 * operation ends in fe_carry, which brings the limbs into 0..255.  Speed is
 * not a concern (a registration runs this a few hundred times in all), the
 * representation was chosen to be easy to argue.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/ed25519_check.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "ed25519_check.h"

#define FE_LIMBS 32

typedef struct
{
	int64		v[FE_LIMBS];
} fe;

/* p = 2^255 - 19, little endian. */
static const uint8 fe_p[FE_LIMBS] = {
	0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f
};

/* p - 2, the exponent of the inverse. */
static const uint8 fe_p_minus_2[FE_LIMBS] = {
	0xeb, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f
};

/* (p - 1) / 2 = 2^254 - 10, the exponent of Euler's criterion. */
static const uint8 fe_euler[FE_LIMBS] = {
	0xf6, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x3f
};

/*
 * Bring the limbs of t (non-negative, any size that fits) into 0..255 with the
 * value kept mod p: a carry out of the top limb is a multiple of 2^256, and
 * 2^256 = 2 * 2^255 = 38 mod p.
 */
static void
fe_carry(int64 *t)
{
	int64		c;
	int			i;

	do
	{
		c = 0;
		for (i = 0; i < FE_LIMBS; i++)
		{
			t[i] += c;
			c = t[i] >> 8;
			t[i] &= 255;
		}
		t[0] += 38 * c;
	} while (c != 0);
}

static void
fe_set_small(fe *r, int64 n)
{
	int			i;

	for (i = 0; i < FE_LIMBS; i++)
		r->v[i] = 0;
	r->v[0] = n;
	fe_carry(r->v);
}

static void
fe_add(fe *r, const fe *a, const fe *b)
{
	int			i;

	for (i = 0; i < FE_LIMBS; i++)
		r->v[i] = a->v[i] + b->v[i];
	fe_carry(r->v);
}

/* a - b: 4p is added so that no limb is negative (the limbs of b are at most 255). */
static void
fe_sub(fe *r, const fe *a, const fe *b)
{
	int			i;

	for (i = 0; i < FE_LIMBS; i++)
		r->v[i] = a->v[i] + 4 * (int64) fe_p[i] - b->v[i];
	fe_carry(r->v);
}

static void
fe_mul(fe *r, const fe *a, const fe *b)
{
	int64		t[2 * FE_LIMBS];
	int			i;
	int			j;

	for (i = 0; i < 2 * FE_LIMBS; i++)
		t[i] = 0;
	for (i = 0; i < FE_LIMBS; i++)
		for (j = 0; j < FE_LIMBS; j++)
			t[i + j] += a->v[i] * b->v[j];
	/* the limb 32 + k is worth 2^(256 + 8k) = 38 * 2^(8k) */
	for (i = 0; i < FE_LIMBS; i++)
		r->v[i] = t[i] + 38 * t[i + FE_LIMBS];
	fe_carry(r->v);
}

/* r = a ^ e, e a 256-bit little-endian number. */
static void
fe_pow(fe *r, const fe *a, const uint8 *e)
{
	fe			acc;
	int			bit;

	fe_set_small(&acc, 1);
	for (bit = 8 * FE_LIMBS - 1; bit >= 0; bit--)
	{
		fe_mul(&acc, &acc, &acc);
		if ((e[bit / 8] >> (bit % 8)) & 1)
			fe_mul(&acc, &acc, a);
	}
	*r = acc;
}

static void
fe_inv(fe *r, const fe *a)
{
	fe_pow(r, a, fe_p_minus_2);
}

/*
 * Subtract p from the limbs when they hold at least p: a value of fewer than
 * 2^256 bits is less than 2p + 38, so two rounds make it canonical.  Returns
 * true when the number was at least p before the last round that changed it
 * (only the canonical test of the input uses the return value).
 */
static bool
fe_sub_p_if_ge(fe *a)
{
	int64		s[FE_LIMBS];
	int64		borrow = 0;
	int			i;

	for (i = 0; i < FE_LIMBS; i++)
	{
		s[i] = a->v[i] - fe_p[i] - borrow;
		if (s[i] < 0)
		{
			s[i] += 256;
			borrow = 1;
		}
		else
			borrow = 0;
	}
	if (borrow)
		return false;
	for (i = 0; i < FE_LIMBS; i++)
		a->v[i] = s[i];
	return true;
}

static void
fe_reduce(fe *a)
{
	(void) fe_sub_p_if_ge(a);
	(void) fe_sub_p_if_ge(a);
}

static bool
fe_equal(const fe *a, const fe *b)
{
	fe			x = *a;
	fe			y = *b;
	int			i;

	fe_reduce(&x);
	fe_reduce(&y);
	for (i = 0; i < FE_LIMBS; i++)
		if (x.v[i] != y.v[i])
			return false;
	return true;
}

static bool
fe_is_zero(const fe *a)
{
	fe			z;

	fe_set_small(&z, 0);
	return fe_equal(a, &z);
}

/* x^2 of the point with this y: (y^2 - 1) / (d y^2 + 1); false when the denominator is zero. */
static bool
fe_x_squared(fe *x2, const fe *y, const fe *d)
{
	fe			one;
	fe			y2;
	fe			u;
	fe			v;

	fe_set_small(&one, 1);
	fe_mul(&y2, y, y);
	fe_sub(&u, &y2, &one);
	fe_mul(&v, d, &y2);
	fe_add(&v, &v, &one);
	if (fe_is_zero(&v))
		return false;
	fe_inv(&v, &v);
	fe_mul(x2, &u, &v);
	return true;
}

KlKeyDefect
kl_ed25519_key_defect(const uint8 *key)
{
	fe			y;
	fe			d;
	fe			t;
	fe			one;
	fe			x2;
	fe			euler;
	int			i;
	int			round;
	bool		sign = (key[31] & 0x80) != 0;

	/* 1. canonical: the 255-bit y is below p. */
	for (i = 0; i < FE_LIMBS; i++)
		y.v[i] = key[i];
	y.v[31] &= 0x7f;
	t = y;
	if (fe_sub_p_if_ge(&t))
		return KL_KEY_NOT_CANONICAL;

	/* d = -121665 / 121666 */
	fe_set_small(&t, 121666);
	fe_inv(&t, &t);
	fe_set_small(&one, 121665);
	fe_set_small(&d, 0);
	fe_sub(&d, &d, &one);
	fe_mul(&d, &d, &t);
	fe_set_small(&one, 1);

	/* 2. on the curve: x^2 is a square. */
	if (!fe_x_squared(&x2, &y, &d))
		return KL_KEY_NOT_ON_CURVE;
	fe_pow(&euler, &x2, fe_euler);
	if (!fe_is_zero(&euler) && !fe_equal(&euler, &one))
		return KL_KEY_NOT_ON_CURVE;

	/* 1 (continued). x = 0 has one encoding, the one without the sign bit. */
	if (sign && fe_is_zero(&x2))
		return KL_KEY_NOT_CANONICAL;

	/* 3. three doublings of y reach the identity (y = 1) only for a point of small order. */
	for (round = 0; round < 3; round++)
	{
		fe			y2;
		fe			num;
		fe			den;

		if (!fe_x_squared(&x2, &y, &d))
			return KL_KEY_NOT_ON_CURVE;
		fe_mul(&y2, &y, &y);
		fe_add(&num, &y2, &x2);
		fe_set_small(&t, 2);
		fe_sub(&den, &t, &y2);
		fe_add(&den, &den, &x2);
		if (fe_is_zero(&den))
			return KL_KEY_NOT_ON_CURVE;
		fe_inv(&den, &den);
		fe_mul(&y, &num, &den);
	}
	if (fe_equal(&y, &one))
		return KL_KEY_SMALL_ORDER;
	return KL_KEY_OK;
}

const char *
kl_ed25519_key_defect_text(KlKeyDefect defect)
{
	switch (defect)
	{
		case KL_KEY_NOT_CANONICAL:
			return "it is not the canonical encoding of a point (a coordinate is not below 2^255 - 19, or the sign bit is set on a zero coordinate), and the same point has another encoding";
		case KL_KEY_NOT_ON_CURVE:
			return "it is not the encoding of a point of the curve";
		case KL_KEY_SMALL_ORDER:
			return "the point has small order (one of the eight points of order 1, 2, 4 and 8), and a signature under such a key proves nothing";
		default:
			return "it is acceptable";
	}
}

/*
 * kl_ed25519_key_defect_cached
 *		The same judgment, remembered for the keys that were found acceptable:
 *		ledger_attest asks on every call and the judgment costs about three
 *		milliseconds (ten exponentiations in the field, written for clarity), which
 *		would be most of the cost of an attested write.  The judgment is a function
 *		of the 32 bytes and nothing else, so a remembered answer cannot be out of
 *		date; the last few keys of the session are kept, and a key that is refused
 *		is judged again every time (it ends the call).
 */
#define KL_KEY_CACHE 8

static uint8 kl_key_cache[KL_KEY_CACHE][32];
static int	kl_key_cache_n = 0;
static int	kl_key_cache_next = 0;

KlKeyDefect
kl_ed25519_key_defect_cached(const uint8 *key)
{
	KlKeyDefect defect;
	int			i;

	for (i = 0; i < kl_key_cache_n; i++)
		if (memcmp(kl_key_cache[i], key, 32) == 0)
			return KL_KEY_OK;
	defect = kl_ed25519_key_defect(key);
	if (defect == KL_KEY_OK)
	{
		memcpy(kl_key_cache[kl_key_cache_next], key, 32);
		kl_key_cache_next = (kl_key_cache_next + 1) % KL_KEY_CACHE;
		if (kl_key_cache_n < KL_KEY_CACHE)
			kl_key_cache_n++;
	}
	return defect;
}
