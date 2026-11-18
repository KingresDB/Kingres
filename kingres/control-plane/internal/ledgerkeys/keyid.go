// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
)

// ErrKeyIDCollision marks two different keys that have the same 32 bit key id
// under one key name (for a log, its origin). A signed note can carry only one
// line for a name and key id, and the verifier refuses a note with two such
// lines. It also refuses every checkpoint of that name while its trust file
// lists both keys among the usable ones (a fresh checkpoint), and every note
// it already holds (a state file, --old) while the trust file lists both keys
// at all, whatever their standing. Test it with errors.Is; errors.As gives the
// *KeyIDCollision with the fingerprints.
var ErrKeyIDCollision = errors.New("ledgerkeys: two different keys have the same key id under one name")

// KeyIDCollision is the error of a key id collision. B is empty when only a
// signature line of the other key is known (a note that was read, not made
// here).
type KeyIDCollision struct {
	Name string
	ID   uint32
	A, B string // fingerprints of the two keys
	// Fresh and Held say which paths of the verifier refuse, when CheckSetKeyIDs
	// found the collision in a set (they are false for a collision found by
	// CheckKeyIDs, VerifyNote or a note, which know no standing). Held is true
	// for every collision of two listed keys: the verifier reads every key of the
	// trust file when it checks a note it holds. Fresh is true when both keys
	// are also usable for a fresh checkpoint (neither is revoked, and neither is
	// a next key left out of the trust file).
	Fresh, Held bool
	// Also lists the other colliding pairs CheckSetKeyIDs or AddChecked found, in
	// the order of the set (empty for one pair). The pair in the error itself is
	// the first one that affects fresh checkpoints (Fresh) when there is one, so
	// that a pair that matters for fresh checkpoints is never hidden behind one
	// that matters only for held notes (K3-02).
	Also []KeyIDCollision
}

// Error says what happened and what the operator does.
func (e *KeyIDCollision) Error() string {
	other := e.B
	if other == "" {
		other = "(not known: only a signature line of it is in the note, which does not verify under the first key)"
	}
	return fmt.Sprintf("%s: under the name %q the keys %s and %s both have the key id %08x. "+
		"A signed note holds one line for a name and key id, so these two keys cannot sign the same checkpoint of this log, "+
		"and the verifier refuses the checkpoints of this name while its trust file lists both as usable, and every note of this name that it holds while its trust file lists both at all (a revoked key too). "+
		"A key that has not signed anything yet (state next) must be dropped and another generated; "+
		"check a new key with Set.AddChecked or CheckSetKeyIDs for the origins of the logs before it is added",
		ErrKeyIDCollision.Error(), clip(e.Name), shortFP(e.A), shortFP(other), e.ID) + e.alsoText()
}

func (e *KeyIDCollision) alsoText() string {
	if len(e.Also) == 0 {
		return ""
	}
	s := fmt.Sprintf(" (%d other colliding pair(s) under this name:", len(e.Also))
	for _, o := range e.Also {
		s += fmt.Sprintf(" %s and %s;", shortFP(o.A), shortFP(o.B))
	}
	return s + ")"
}

// Is reports whether target is ErrKeyIDCollision.
func (e *KeyIDCollision) Is(target error) bool { return target == ErrKeyIDCollision }

func shortFP(fp string) string {
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

// CheckKeyIDs returns a *KeyIDCollision (which is ErrKeyIDCollision) when two
// different keys of pubs have the same key id under name, and nil otherwise.
// The same key twice is not a collision. It knows nothing about the standing
// of a key: a caller that holds a Set uses CheckSetKeyIDs, which applies the
// verifier's rule to the keys the trust file would list.
func CheckKeyIDs(name string, pubs ...ed25519.PublicKey) error {
	seen := map[uint32]ed25519.PublicKey{}
	for _, p := range pubs {
		if len(p) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: a public key has %d bytes, expected %d", ErrInvalidArgument, len(p), ed25519.PublicKeySize)
		}
		id := KeyID(name, p)
		if q, ok := seen[id]; ok && !bytes.Equal(p, q) {
			return &KeyIDCollision{Name: name, ID: id, A: Fingerprint(q), B: Fingerprint(p)}
		}
		seen[id] = p
	}
	return nil
}

// CheckSetKeyIDs applies the verifier's rule about key ids to the keys that
// the trust file written from set with opt would list, under the key name
// origin (the origin of a log), and returns a *KeyIDCollision when two
// different keys have the same key id, nil otherwise. The rule has two paths:
//
//   - a fresh checkpoint is checked against the usable keys of the trust file
//     (a revoked key is left out, and so is a next key unless IncludeNext
//     lists it): two of them with one id make the verifier refuse every
//     checkpoint of the name (the collision has Fresh set);
//   - a note the auditor already holds (a state file, --old, an old checkpoint)
//     is checked against every key of the file, whatever its standing, so one
//     revoked key with the id of another listed key makes the verifier refuse
//     that log for every auditor who holds a note of it (Held, set for every
//     collision this function reports).
//
// A key is never removed from the trust file, so no later transition cures a
// collision with a revoked or retired key. Call it with the set as it will be
// after the change, with IncludeNext false for what the next trust file lists
// and true for what it lists once the next keys are pinned (the stricter
// reading; Set.AddChecked uses it), once per log origin. The same key twice is
// not a collision.
//
// It reports EVERY colliding pair, not only the first: the returned
// *KeyIDCollision is the first pair that affects fresh checkpoints (Fresh)
// when there is one, else the first pair, and Also lists the others, so a pair
// that stops fresh checkpoints is never hidden behind one that only affects
// notes already held (K3-02, correction 107).
func CheckSetKeyIDs(set Set, opt TrustOptions, origin string) error {
	all, err := setCollisions(set, opt, origin)
	if err != nil {
		return err
	}
	return collisionError(all)
}

// setCollisions finds every colliding pair of the keys the trust file would
// list, in the order of the set (the pair of the keys i < j is reported in the
// order i, j).
func setCollisions(set Set, opt TrustOptions, origin string) ([]KeyIDCollision, error) {
	if !ValidKeyName(origin) {
		return nil, fmt.Errorf("%w: %q is not a valid key name", ErrInvalidArgument, clip(origin))
	}
	var listed []Key
	for _, k := range set {
		if len(k.Public) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: a public key has %d bytes, expected %d", ErrInvalidArgument, len(k.Public), ed25519.PublicKeySize)
		}
		if k.State == StateNext && !opt.IncludeNext {
			continue
		}
		listed = append(listed, k)
	}
	var out []KeyIDCollision
	for i, a := range listed {
		for _, b := range listed[i+1:] {
			if bytes.Equal(a.Public, b.Public) || KeyID(origin, a.Public) != KeyID(origin, b.Public) {
				continue
			}
			out = append(out, KeyIDCollision{Name: origin, ID: KeyID(origin, a.Public), A: Fingerprint(a.Public), B: Fingerprint(b.Public),
				Fresh: a.State != StateRevoked && b.State != StateRevoked, Held: true})
		}
	}
	return out, nil
}

// collisionError turns the pairs found into one error: the first Fresh pair, or
// the first pair, with the others in Also. nil for no pair.
func collisionError(all []KeyIDCollision) error {
	if len(all) == 0 {
		return nil
	}
	main := 0
	for i, c := range all {
		if c.Fresh {
			main = i
			break
		}
	}
	e := all[main]
	for i, c := range all {
		if i != main {
			e.Also = append(e.Also, c)
		}
	}
	return &e
}
