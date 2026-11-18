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
// lines, and refuses every checkpoint of that name while its trust file holds
// both keys among the usable ones. Test it with errors.Is; errors.As gives the
// *KeyIDCollision with the fingerprints.
var ErrKeyIDCollision = errors.New("ledgerkeys: two different keys have the same key id under one name")

// KeyIDCollision is the error of a key id collision. B is empty when only a
// signature line of the other key is known (a note that was read, not made
// here).
type KeyIDCollision struct {
	Name string
	ID   uint32
	A, B string // fingerprints of the two keys
}

// Error says what happened and what the operator does.
func (e *KeyIDCollision) Error() string {
	other := e.B
	if other == "" {
		other = "(not known: only a signature line of it is in the note, which does not verify under the first key)"
	}
	return fmt.Sprintf("%s: under the name %q the keys %s and %s both have the key id %08x. "+
		"A signed note holds one line for a name and key id, so these two keys cannot sign the same checkpoint of this log, "+
		"and the verifier refuses the checkpoints of this name while its trust file lists both as usable. "+
		"A key that has not signed anything yet (state next) must be dropped and another generated; "+
		"check a new key with CheckKeyIDs for the origins of the logs before it is activated",
		ErrKeyIDCollision.Error(), clip(e.Name), shortFP(e.A), shortFP(other), e.ID)
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
// The same key twice is not a collision. A caller that publishes under many
// names checks the keys that may sign at once (an overlap) and the retired keys
// the trust file still lists, once per name, before it activates a key.
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
