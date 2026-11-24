// SPDX-License-Identifier: AGPL-3.0-only

package pki

// Escrow of the key of the certificate authority (ha-phase5-part2.md, section 3.4).
//
// The manager leader holds ca.key. So that a new leader can use the key without the old one, the leader seals a copy to
// each server that may become leader and sends it over the internal API. A copy is a blob (EscrowBlob) that only the
// private key of its recipient opens: the key agreement is X25519 between a fresh key of the leader and the recipient's
// escrow key, the content key is HKDF-SHA256 of the shared secret, the cipher is AES-256-GCM, and everything that
// names the blob (recipient, key id, CA fingerprint, generation, time) is the additional data, so a blob cannot be
// moved to another server or relabelled. The escrow key of a server is its own file below the pki directory
// (escrow/recipient-<id>.key, mode 0600); it never leaves the server and is not the server's TLS key.
//
// A blob is adopted only when the key inside belongs to the CA certificate the server already holds (ca.crt), so a
// blob that somebody else made for this server (the public key is public) cannot put another CA in place.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Names below the pki directory.
const (
	// EscrowDir is the directory of the escrow (mode 0700).
	EscrowDir = "escrow"
	// EscrowBlobFile is the sealed copy of the CA key a recipient holds.
	EscrowBlobFile = "ca-key.sealed"
	// EscrowStateFile is the leader's bookkeeping (EscrowState).
	EscrowStateFile = "state.json"

	recipientKeyPrefix = "recipient-"
	recipientKeySuffix = ".key"
	escrowVersion      = 2
	escrowInfo         = "kingres ca escrow v1"
)

// Errors of the escrow. The messages never carry key material.
var (
	// ErrNoEscrow means this server holds no sealed copy.
	ErrNoEscrow = errors.New("pki: no sealed copy of the CA key on this server")
	// ErrEscrowRefused means a blob was refused: it is not for this server, is damaged, is older than the copy held,
	// or does not belong to this cluster's CA.
	ErrEscrowRefused = errors.New("pki: the sealed copy of the CA key was refused")
)

// EscrowBlob is the sealed copy of the CA key for one recipient.
type EscrowBlob struct {
	Version       int       `json:"version"`
	ServerID      string    `json:"serverId"`
	KeyID         string    `json:"keyId"`
	CAFingerprint string    `json:"caFingerprint"`
	Generation    int64     `json:"generation"`
	Epoch         int64     `json:"epoch"`
	Seq           int64     `json:"seq"`
	SealedAt      time.Time `json:"sealedAt"`
	Ephemeral     []byte    `json:"ephemeral"`
	Ciphertext    []byte    `json:"ciphertext"` // nonce, then the sealed content
}

// escrowPlain is what the blob carries: the CA key and the revocation list of the CA.
type escrowPlain struct {
	CAKey   string          `json:"caKey"`
	Revoked json.RawMessage `json:"revoked"`
}

type revokedDoc struct {
	Serials map[string]time.Time `json:"serials"`
	CRL     int64                `json:"crlNumber"`
}

// Position orders the copies of a recipient: the epoch of the leader that sealed it (it rises at every change of leader), the
// generation (it rises at every switching on and every rotation) and a counter of the seals of that leader. The wall clock
// orders nothing, so a leader whose clock is behind is not refused, and a former leader is (its epoch is lower).
type Position struct{ Epoch, Generation, Seq int64 }

// Less says whether p comes before q.
func (p Position) Less(q Position) bool {
	if p.Epoch != q.Epoch {
		return p.Epoch < q.Epoch
	}
	if p.Generation != q.Generation {
		return p.Generation < q.Generation
	}
	return p.Seq < q.Seq
}

// Position is the place of the blob in the order of the copies.
func (b EscrowBlob) Position() Position { return Position{b.Epoch, b.Generation, b.Seq} }

// aad is the authenticated header of a blob: every field is length-prefixed, so no value can shift into another.
func (b EscrowBlob) aad() []byte {
	if b.Version == 1 {
		// The copies of the first version (before the review fixes): fields joined with a bar, no epoch and no seq. They are still opened, so a
		// failover between an upgrade and the first resend of the new leader finds a copy that works.
		return []byte(fmt.Sprintf("%s|%d|%s|%s|%s|%d|%d", escrowInfo, b.Version, b.ServerID, b.KeyID, b.CAFingerprint, b.Generation, b.SealedAt.UnixNano()))
	}
	var out []byte
	put := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		out = append(append(out, n[:]...), s...)
	}
	put(escrowInfo)
	for _, v := range []int64{int64(b.Version), b.Generation, b.Epoch, b.Seq, b.SealedAt.UnixNano()} {
		put(strconv.FormatInt(v, 10))
	}
	put(b.ServerID)
	put(b.KeyID)
	put(b.CAFingerprint)
	return out
}

// KeyIDOf is the identifier of a recipient's public key: the first 8 bytes of its SHA-256, in hex.
func KeyIDOf(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

func escrowDir(pkiDir string) string { return filepath.Join(pkiDir, EscrowDir) }

// recipientKeyFile is the content of an escrow key file.
type recipientKeyFile struct {
	KeyID   string    `json:"keyId"`
	Created time.Time `json:"created"`
	Private []byte    `json:"private"`
}

func ensureEscrowDir(pkiDir string) (string, error) {
	d := escrowDir(pkiDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

func newRecipientKey(pkiDir string, now time.Time) (keyID string, pub []byte, err error) {
	d, err := ensureEscrowDir(pkiDir)
	if err != nil {
		return "", nil, err
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	pub = priv.PublicKey().Bytes()
	keyID = KeyIDOf(pub)
	b, err := json.Marshal(recipientKeyFile{KeyID: keyID, Created: now.UTC(), Private: priv.Bytes()})
	if err != nil {
		return "", nil, err
	}
	if err := WriteFile(filepath.Join(d, recipientKeyPrefix+keyID+recipientKeySuffix), b, 0o600); err != nil {
		return "", nil, err
	}
	return keyID, pub, nil
}

func readRecipientKeys(pkiDir string) (map[string]recipientKeyFile, error) {
	ents, err := os.ReadDir(escrowDir(pkiDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]recipientKeyFile{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, recipientKeyPrefix) || !strings.HasSuffix(n, recipientKeySuffix) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(escrowDir(pkiDir), n)); err != nil {
			return nil, err
		} else if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("pki: the escrow key file %s has mode %v; it must not be readable by others (0600)", n, fi.Mode().Perm())
		}
		b, err := os.ReadFile(filepath.Join(escrowDir(pkiDir), n)) //nolint:gosec // below the pki directory
		if err != nil {
			return nil, err
		}
		var k recipientKeyFile
		if err := json.Unmarshal(b, &k); err != nil || k.KeyID == "" {
			continue // an unreadable file is no key
		}
		out[k.KeyID] = k
	}
	return out, nil
}

// RecipientOffer returns the escrow public key this server offers to the leader: the newest key it has, made when it has
// none or when rotate is set (a rotation keeps the older keys until a blob for the new one has been stored).
func RecipientOffer(pkiDir string, rotate bool, now time.Time) (keyID string, pub []byte, err error) {
	keys, err := readRecipientKeys(pkiDir)
	if err != nil {
		return "", nil, err
	}
	if !rotate {
		var newest *recipientKeyFile
		for _, k := range keys {
			if newest == nil || k.Created.After(newest.Created) {
				kk := k
				newest = &kk
			}
		}
		if newest != nil {
			priv, err := ecdh.X25519().NewPrivateKey(newest.Private)
			if err == nil {
				return newest.KeyID, priv.PublicKey().Bytes(), nil
			}
		}
	}
	return newRecipientKey(pkiDir, now)
}

// plainOf is the sealed content of a CA: its key and its revocation list.
func (ca *CA) escrowPlain() (plain []byte, err error) {
	kb, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		return nil, err
	}
	ca.mu.Lock()
	rb, err := json.Marshal(revokedDoc{Serials: ca.revoked, CRL: ca.crlNum})
	ca.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return json.Marshal(escrowPlain{CAKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})), Revoked: rb})
}

// EscrowContentHash identifies the content a blob would carry now (it changes when the revocation list does), so that the
// leader sends again only when something changed. It is not part of any blob.
func (ca *CA) EscrowContentHash() (string, error) {
	p, err := ca.escrowPlain()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:]), nil
}

// CAFingerprint is the SHA-256 of the CA certificate.
func (ca *CA) CAFingerprint() string { return Fingerprint(ca.cert.Raw) }

func deriveKey(shared, ephemeral, recipient []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, append(append([]byte{}, ephemeral...), recipient...), escrowInfo, 32)
}

// SealCA seals the CA key and revocation list of ca to the recipient server serverID, whose escrow public key is pub.
func SealCA(ca *CA, serverID, keyID string, pub []byte, pos Position, now time.Time) (EscrowBlob, error) {
	if keyID != KeyIDOf(pub) {
		return EscrowBlob{}, errors.New("pki: the key id does not belong to the public key")
	}
	if _, err := ecdh.X25519().NewPublicKey(pub); err != nil {
		return EscrowBlob{}, fmt.Errorf("pki: the recipient's escrow key: %w", err)
	}
	plain, err := ca.escrowPlain()
	if err != nil {
		return EscrowBlob{}, err
	}
	return sealPlain(plain, ca.CAFingerprint(), serverID, keyID, pub, pos, now)
}

// sealPlain seals a content to a recipient (SealCA seals the CA; the tests seal damaged contents).
func sealPlain(plain []byte, fingerprint, serverID, keyID string, pub []byte, pos Position, now time.Time) (EscrowBlob, error) {
	rpub, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return EscrowBlob{}, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return EscrowBlob{}, err
	}
	shared, err := eph.ECDH(rpub)
	if err != nil {
		return EscrowBlob{}, err
	}
	defer clear(shared)
	key, err := deriveKey(shared, eph.PublicKey().Bytes(), pub)
	if err != nil {
		return EscrowBlob{}, err
	}
	b := EscrowBlob{Version: escrowVersion, ServerID: serverID, KeyID: keyID, CAFingerprint: fingerprint,
		Generation: pos.Generation, Epoch: pos.Epoch, Seq: pos.Seq, SealedAt: now.UTC().Truncate(time.Microsecond), Ephemeral: eph.PublicKey().Bytes()}
	gcm, err := newGCM(key)
	if err != nil {
		return EscrowBlob{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return EscrowBlob{}, err
	}
	b.Ciphertext = append(nonce, gcm.Seal(nil, nonce, plain, b.aad())...)
	clear(plain)
	clear(key)
	return b, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// open decrypts a blob with the recipient's private key.
func (b EscrowBlob) open(serverID string, k recipientKeyFile) (escrowPlain, error) {
	var out escrowPlain
	if (b.Version != escrowVersion && b.Version != 1) || b.ServerID != serverID || b.KeyID != k.KeyID {
		return out, ErrEscrowRefused
	}
	priv, err := ecdh.X25519().NewPrivateKey(k.Private)
	if err != nil {
		return out, ErrEscrowRefused
	}
	epub, err := ecdh.X25519().NewPublicKey(b.Ephemeral)
	if err != nil {
		return out, ErrEscrowRefused
	}
	shared, err := priv.ECDH(epub)
	if err != nil {
		return out, ErrEscrowRefused
	}
	defer clear(shared)
	key, err := deriveKey(shared, b.Ephemeral, priv.PublicKey().Bytes())
	if err != nil {
		return out, ErrEscrowRefused
	}
	gcm, err := newGCM(key)
	if err != nil || len(b.Ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return out, ErrEscrowRefused
	}
	plain, err := gcm.Open(nil, b.Ciphertext[:gcm.NonceSize()], b.Ciphertext[gcm.NonceSize():], b.aad())
	if err != nil {
		return out, ErrEscrowRefused
	}
	if err := json.Unmarshal(plain, &out); err != nil {
		return out, ErrEscrowRefused
	}
	return out, nil
}

// keyMatchesCert reports whether the EC private key in PEM is the key of the CA certificate whose PEM is certPEM.
func keyMatchesCert(keyPEM string, cert *x509.Certificate) (*ecdsa.PrivateKey, bool) {
	kb, _ := pem.Decode([]byte(keyPEM))
	if kb == nil {
		return nil, false
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return key, ok && pub.Equal(&key.PublicKey)
}

func readCACert(pkiDir string) (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(pkiDir, CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return nil, err
	}
	return ParseCertPEM(b)
}

// readBlob reads the blob this server holds.
func readBlob(pkiDir string) (EscrowBlob, error) {
	var b EscrowBlob
	raw, err := os.ReadFile(filepath.Join(escrowDir(pkiDir), EscrowBlobFile)) //nolint:gosec // fixed name below the pki directory
	if errors.Is(err, os.ErrNotExist) {
		return b, ErrNoEscrow
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("%w: unreadable", ErrEscrowRefused)
	}
	return b, nil
}

// EscrowHeld describes the blob a server holds, without opening it.
type EscrowHeld struct {
	KeyID    string
	Position Position
	SealedAt time.Time
}

// HeldEscrow reports the blob of this server (ErrNoEscrow when there is none).
func HeldEscrow(pkiDir string) (EscrowHeld, error) {
	b, err := readBlob(pkiDir)
	if err != nil {
		return EscrowHeld{}, err
	}
	return EscrowHeld{KeyID: b.KeyID, Position: b.Position(), SealedAt: b.SealedAt}, nil
}

// StoreEscrow checks a blob the leader sent and keeps it. The blob must be for this server and for one of its escrow
// keys, must open, must hold the key of the CA certificate this server holds, and must not be older than the one held;
// otherwise nothing changes and the error is ErrEscrowRefused. The escrow keys that the blob does not use are removed.
func StoreEscrow(pkiDir, serverID string, b EscrowBlob) error {
	keys, err := readRecipientKeys(pkiDir)
	if err != nil {
		return err
	}
	k, ok := keys[b.KeyID]
	if !ok {
		return fmt.Errorf("%w: no such escrow key on this server", ErrEscrowRefused)
	}
	plain, err := b.open(serverID, k)
	if err != nil {
		return err
	}
	cert, err := readCACert(pkiDir)
	if err != nil {
		return err
	}
	if _, ok := keyMatchesCert(plain.CAKey, cert); !ok || b.CAFingerprint != Fingerprint(cert.Raw) {
		return fmt.Errorf("%w: it is not the key of this cluster's CA", ErrEscrowRefused)
	}
	var rv revokedDoc
	if len(plain.Revoked) > 0 {
		if err := json.Unmarshal(plain.Revoked, &rv); err != nil {
			return fmt.Errorf("%w: the revocation list in it is not valid", ErrEscrowRefused)
		}
	}
	// A held copy that cannot be read counts as the oldest possible position: it must not make every blob acceptable by
	// accident, and it must not block a good one.
	if held, err := readBlob(pkiDir); err == nil && b.Position().Less(held.Position()) {
		return fmt.Errorf("%w: older than the copy held", ErrEscrowRefused)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := WriteFile(filepath.Join(escrowDir(pkiDir), EscrowBlobFile), raw, 0o600); err != nil {
		return err
	}
	for id := range keys {
		if id != b.KeyID {
			_ = os.Remove(filepath.Join(escrowDir(pkiDir), recipientKeyPrefix+id+recipientKeySuffix))
		}
	}
	return nil
}

// DropEscrow removes the sealed copy and every escrow key of this server (escrow switched off, or the server left the
// set of recipients). A missing copy is not an error.
func DropEscrow(pkiDir string) error {
	d := escrowDir(pkiDir)
	ents, err := os.ReadDir(d)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var first error
	for _, e := range ents {
		if e.Name() == EscrowStateFile {
			continue
		}
		if err := os.Remove(filepath.Join(d, e.Name())); err != nil && first == nil && !errors.Is(err, os.ErrNotExist) {
			first = err
		}
	}
	return first
}

// AdoptResult says what AdoptEscrow did.
type AdoptResult int

const (
	// AdoptHeld: this server already holds ca.key; nothing was done.
	AdoptHeld AdoptResult = iota
	// AdoptNone: no ca.key and no sealed copy.
	AdoptNone
	// Adopted: ca.key and revoked.json were written from the sealed copy.
	Adopted
)

// AdoptEscrow makes this server the holder of the CA from its sealed copy: it opens the copy with its escrow key, checks
// that the key is the one of ca.crt, writes ca.key (mode 0600, never over an existing one) and merges the revocation list
// of the copy with the list the previous leader last distributed (revoked-serials.json), so that no revocation is lost.
// A copy that does not open or does not belong to the CA is an error and nothing is written.
func AdoptEscrow(pkiDir, serverID string, now time.Time) (AdoptResult, error) {
	if valid, err := caKeyUsable(pkiDir); err != nil {
		return AdoptNone, err
	} else if valid {
		return AdoptHeld, nil
	}
	// A ca.key that is there but cannot be used (a write that a power failure cut short) is not "key held": it is put aside
	// and the copy is adopted instead; without a copy the server answers that it has no key and the alert is raised.
	if _, err := os.Stat(filepath.Join(pkiDir, CAKeyFile)); err == nil {
		aside := filepath.Join(pkiDir, fmt.Sprintf("%s.unusable-%d", CAKeyFile, now.Unix()))
		if err := os.Rename(filepath.Join(pkiDir, CAKeyFile), aside); err != nil {
			return AdoptNone, err
		}
	}
	b, err := readBlob(pkiDir)
	if errors.Is(err, ErrNoEscrow) {
		return AdoptNone, nil
	}
	if err != nil {
		return AdoptNone, err
	}
	plain, key, err := openHeld(pkiDir, serverID, b)
	if err != nil {
		return AdoptNone, err
	}
	var rev revokedDoc
	if len(plain.Revoked) > 0 {
		if err := json.Unmarshal(plain.Revoked, &rev); err != nil {
			return AdoptNone, fmt.Errorf("%w: the revocation list", ErrEscrowRefused)
		}
	}
	if rev.Serials == nil {
		rev.Serials = map[string]time.Time{}
	}
	// The list of the copy, the list the previous leader last distributed and the one in the revocation list file
	// that is published: no revocation is lost, and the number of the lists never goes back.
	local, _ := LoadRevokedList(pkiDir)
	if crl, err := os.ReadFile(filepath.Join(pkiDir, CRLFile)); err == nil { //nolint:gosec // fixed name below the pki directory
		if n, s, err := CRLContent(crl); err == nil {
			for x := range s {
				local = append(local, x)
			}
			if n > rev.CRL {
				rev.CRL = n
			}
		}
	}
	for _, s := range local {
		if _, ok := rev.Serials[s]; !ok {
			rev.Serials[s] = now.UTC()
		}
	}
	rb, err := json.Marshal(rev)
	if err != nil {
		return AdoptNone, err
	}
	// revoked.json first, then ca.key, each atomically and durably (temp file, fsync, rename, directory fsync): a crash in
	// between leaves no key, and the next adoption starts again.
	if err := WriteFile(filepath.Join(pkiDir, RevokedFile), rb, 0o600); err != nil {
		return AdoptNone, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return AdoptNone, err
	}
	defer clear(kb)
	if err := WriteFile(filepath.Join(pkiDir, CAKeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return AdoptNone, err
	}
	return Adopted, nil
}

// caKeyUsable reports whether pki/ca.key exists and is the key of ca.crt. A missing file is (false, nil).
func caKeyUsable(pkiDir string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(pkiDir, CAKeyFile)) //nolint:gosec // fixed name below the pki directory
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cert, err := readCACert(pkiDir)
	if err != nil {
		return false, err
	}
	_, ok := keyMatchesCert(string(raw), cert)
	return ok, nil
}

// openHeld opens a blob with the escrow key of this server and checks that it holds the key of ca.crt.
func openHeld(pkiDir, serverID string, b EscrowBlob) (escrowPlain, *ecdsa.PrivateKey, error) {
	keys, err := readRecipientKeys(pkiDir)
	if err != nil {
		return escrowPlain{}, nil, err
	}
	k, ok := keys[b.KeyID]
	if !ok {
		return escrowPlain{}, nil, fmt.Errorf("%w: the escrow key of the copy is not on this server", ErrEscrowRefused)
	}
	plain, err := b.open(serverID, k)
	if err != nil {
		return plain, nil, err
	}
	cert, err := readCACert(pkiDir)
	if err != nil {
		return plain, nil, err
	}
	key, ok := keyMatchesCert(plain.CAKey, cert)
	if !ok || b.CAFingerprint != Fingerprint(cert.Raw) {
		return plain, nil, fmt.Errorf("%w: it is not the key of this cluster's CA", ErrEscrowRefused)
	}
	return plain, key, nil
}

// HeldRevoked opens the copy this server holds and returns the serials of the revocation list in it (nil, ErrNoEscrow
// when there is no copy). A returning leader merges them into its own list before it distributes anything.
func HeldRevoked(pkiDir, serverID string) ([]string, error) {
	b, err := readBlob(pkiDir)
	if err != nil {
		return nil, err
	}
	plain, _, err := openHeld(pkiDir, serverID, b)
	if err != nil {
		return nil, err
	}
	var rev revokedDoc
	if len(plain.Revoked) > 0 {
		if err := json.Unmarshal(plain.Revoked, &rev); err != nil {
			return nil, fmt.Errorf("%w: the revocation list", ErrEscrowRefused)
		}
	}
	out := make([]string, 0, len(rev.Serials))
	for s := range rev.Serials {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// EscrowRecipientState is what the leader knows of one recipient.
type EscrowRecipientState struct {
	KeyID       string    `json:"keyId"`
	Generation  int64     `json:"generation"`
	SealedAt    time.Time `json:"sealedAt"`
	ContentHash string    `json:"contentHash"`
	LastError   string    `json:"lastError,omitempty"`
	LastRefused bool      `json:"lastRefused,omitempty"`
	Epoch       int64     `json:"epoch"`
	// DropPending: the server is not (or no longer) a recipient and its copy has not been deleted yet; the record stays and the
	// drop is tried every round. Misses counts the rounds in a row in which a member was not a candidate.
	DropPending bool      `json:"dropPending,omitempty"`
	Misses      int       `json:"misses,omitempty"`
	LastTry     time.Time `json:"lastTry"`
}

// EscrowState is the leader's bookkeeping of the escrow (escrow/state.json). Whether escrow is on follows the CA key: a
// new leader that adopted a copy is told by Enabled that it goes on.
type EscrowState struct {
	Enabled    bool  `json:"enabled"`
	Generation int64 `json:"generation"`
	// Epoch rises at every change of leader and Seq at every seal of one leader (Position); Swept says that every member was told
	// to drop its copy after the escrow was switched off.
	Epoch      int64                           `json:"epoch"`
	Seq        int64                           `json:"seq"`
	Swept      bool                            `json:"swept,omitempty"`
	Recipients map[string]EscrowRecipientState `json:"recipients"`
}

// LoadEscrowState reads the bookkeeping; a missing file is the state "off".
func LoadEscrowState(pkiDir string) (EscrowState, error) {
	st := EscrowState{Recipients: map[string]EscrowRecipientState{}}
	b, err := os.ReadFile(filepath.Join(escrowDir(pkiDir), EscrowStateFile)) //nolint:gosec // fixed name below the pki directory
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("pki: %s: %w", EscrowStateFile, err)
	}
	if st.Recipients == nil {
		st.Recipients = map[string]EscrowRecipientState{}
	}
	return st, nil
}

// SaveEscrowState writes the bookkeeping.
func SaveEscrowState(pkiDir string, st EscrowState) error {
	if _, err := ensureEscrowDir(pkiDir); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return WriteFile(filepath.Join(escrowDir(pkiDir), EscrowStateFile), b, 0o600)
}

// SortedRecipients returns the recipient ids of a state, sorted.
func (s EscrowState) SortedRecipients() []string {
	out := make([]string, 0, len(s.Recipients))
	for id := range s.Recipients {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// EncodeKey and DecodeKey carry an escrow public key in JSON.
func EncodeKey(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// DecodeKey is the inverse of EncodeKey.
func DecodeKey(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
