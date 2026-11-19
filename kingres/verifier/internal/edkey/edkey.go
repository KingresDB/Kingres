// SPDX-License-Identifier: AGPL-3.0-only

// Package edkey says which 32 bytes may be used as an Ed25519 public key.
//
// Plain Ed25519 verification (RFC 8032, crypto/ed25519) accepts a signature
// that nobody made under a public key of small order: for the identity point the
// signature R = identity, S = 0 verifies for every message, and for the other
// points of order 2, 4 and 8 a signature verifies for a share of the messages.
// Several encodings also give one point, so one key would have several
// identifiers and a revocation of one of them would leave the others alive.
// Correction 155 of ledger-phase7.md therefore asks the verifier to apply the same
// three tests as the extension to every public key it reads: the canonical
// encoding of a point of the curve that does not have small order.
//
// This is written from the definitions, with integer arithmetic only, as the
// extension's own check (contrib/kingres_ledger/src/ed25519_check.c) is, and it
// shares no code with it. With p = 2^255 - 19 and the curve
// -x^2 + y^2 = 1 + d x^2 y^2, d = -121665 / 121666:
//
//  1. canonical: the 255-bit y of the key (the low 255 bits, little endian) is
//     below p, and x is not zero with the sign bit set.
//  2. on the curve: x^2 = (y^2 - 1) / (d y^2 + 1) is a square (Euler: the power
//     (p-1)/2 is 0 or 1). The denominator is never zero, d being a non-square.
//  3. not of small order: the points of order dividing 8 are exactly those with
//     [8]P = identity. The doubling formula depends on y alone once x^2 is known,
//     y(2P) = (y^2 + x^2) / (2 - y^2 + x^2), so three doublings of y reach y = 1
//     (the identity is the only point with y = 1) exactly for such a point.
package edkey

import (
	"errors"
	"fmt"
	"math/big"
)

// Size is the length of a public key.
const Size = 32

// The three reasons a key is refused. Check wraps one of them.
var (
	ErrNotCanonical = errors.New("it is not the canonical encoding of a point (a coordinate is not below 2^255 - 19, or the sign bit is set on a zero coordinate), and the same point has another encoding")
	ErrNotOnCurve   = errors.New("it is not the encoding of a point of the curve")
	ErrSmallOrder   = errors.New("the point has small order (one of the eight points of order 1, 2, 4 and 8), and a signature under such a key proves nothing")
)

var (
	one    = big.NewInt(1)
	two    = big.NewInt(2)
	prime  = new(big.Int).Sub(new(big.Int).Lsh(one, 255), big.NewInt(19))
	pMinus = new(big.Int).Sub(prime, two)                      // exponent of the inverse
	euler  = new(big.Int).Rsh(new(big.Int).Sub(prime, one), 1) // (p-1)/2
	curveD = func() *big.Int {
		num := new(big.Int).Neg(big.NewInt(121665))
		den := new(big.Int).ModInverse(big.NewInt(121666), prime)
		return num.Mul(num, den).Mod(num, prime)
	}()
)

func mod(x *big.Int) *big.Int { return x.Mod(x, prime) }

func inv(a *big.Int) *big.Int { return new(big.Int).Exp(a, pMinus, prime) }

// xSquared is (y^2 - 1) / (d y^2 + 1); false when the denominator is zero.
func xSquared(y *big.Int) (*big.Int, bool) {
	y2 := mod(new(big.Int).Mul(y, y))
	u := mod(new(big.Int).Sub(y2, one))
	v := mod(new(big.Int).Add(mod(new(big.Int).Mul(curveD, y2)), one))
	if v.Sign() == 0 {
		return nil, false
	}
	return mod(u.Mul(u, inv(v))), true
}

// Check returns nil when key may be used as a public key, and otherwise an
// error that wraps ErrNotCanonical, ErrNotOnCurve or ErrSmallOrder and says
// which test failed. A key of the wrong length is refused too.
func Check(key []byte) error {
	if len(key) != Size {
		return fmt.Errorf("a public key has %d bytes, expected %d", len(key), Size)
	}
	sign := key[31]&0x80 != 0
	// little endian to big endian, without the sign bit
	be := make([]byte, Size)
	for i := range be {
		be[Size-1-i] = key[i]
	}
	be[0] &= 0x7f
	y := new(big.Int).SetBytes(be)
	if y.Cmp(prime) >= 0 {
		return ErrNotCanonical
	}
	x2, ok := xSquared(y)
	if !ok {
		return ErrNotOnCurve
	}
	if e := new(big.Int).Exp(x2, euler, prime); e.Sign() != 0 && e.Cmp(one) != 0 {
		return ErrNotOnCurve
	}
	if sign && x2.Sign() == 0 {
		return ErrNotCanonical
	}
	for round := 0; round < 3; round++ {
		x2, ok = xSquared(y)
		if !ok {
			return ErrNotOnCurve
		}
		y2 := mod(new(big.Int).Mul(y, y))
		num := mod(new(big.Int).Add(y2, x2))
		den := mod(new(big.Int).Add(mod(new(big.Int).Sub(two, y2)), x2))
		if den.Sign() == 0 {
			return ErrNotOnCurve
		}
		y = mod(num.Mul(num, inv(den)))
	}
	if y.Cmp(one) == 0 {
		return ErrSmallOrder
	}
	return nil
}
