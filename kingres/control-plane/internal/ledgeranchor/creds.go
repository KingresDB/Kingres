// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// The credentials of an anchor target (design 6.8, 9.1): a file <ledger-keys-dir>/anchor/<target id>.json, mode 0600,
// in the envelope of the signing key files: the line KINGRES-ANCHOR-1, then one line of JSON, either
//
//	{"credentials":{"accessKeyId":"...","secretAccessKey":"..."}}
//
// in clear (as a signing key is without a passphrase file; the host and the backups protect it), or, with a
// passphrase file, the same object sealed with AES-256-GCM under PBKDF2-HMAC-SHA256 of the passphrase:
//
//	{"kdf":"pbkdf2-hmac-sha256","iterations":600000,"salt":"<b64>","nonce":"<b64>","ciphertext":"<b64>"}
//
// whose authenticated data is the first line, the name of the function, a zero byte, the count and the salt. A
// target may instead name credentialsPath in its configuration, a file that already exists on every holder (the
// operator's own secret mount), in the plain form; it is read at every use and never copied.
const (
	// CredMagic is the first line of a credentials file.
	CredMagic = "KINGRES-ANCHOR-1"
	// MaxCredFile is the most a credentials file may hold.
	MaxCredFile = 16 << 10
	credKDF     = "pbkdf2-hmac-sha256"
	credSalt    = 16
	credNonce   = 12
)

// ErrNoCredentials means the target has credentials set and this server has no file for them (it is not a holder, or
// the key directory was restored without them).
var ErrNoCredentials = errors.New("ledgeranchor: this server has no credentials file for the target")

// ErrHoldersUnreached is the answer of Fetch when no holder gave the file and at least one could not be asked (it did not answer, or it
// answered something that is no answer): the file may exist. It is not ErrNoCredentials, which says that every holder was asked and none
// has the file. The first is a reason to wait and try again, the second a reason to ask the operator for the credentials (P7R-08).
var ErrHoldersUnreached = errors.New("ledgeranchor: no holder gave the credentials file and not every holder could be asked")

// HolderAnswer is what one holder said to the request for a credentials file.
type HolderAnswer int

const (
	// HolderHasFile: it sent the file.
	HolderHasFile HolderAnswer = iota
	// HolderHasNone: it answered, and it has no file for the target.
	HolderHasNone
	// HolderUnreached: it did not answer, or its answer says nothing about the file (an error status, bytes that are no file).
	HolderUnreached
)

// FetchFirst asks the hosts one after the other and returns the first file it gets. When none gives a file it returns
// ErrNoCredentials if every host answered that it has none (also when there is no other host), and ErrHoldersUnreached otherwise.
func FetchFirst(ctx context.Context, hosts []string, ask func(ctx context.Context, host string) ([]byte, HolderAnswer)) ([]byte, error) {
	unreached := false
	for _, h := range hosts {
		if ctx.Err() != nil {
			return nil, ErrHoldersUnreached
		}
		raw, ans := ask(ctx, h)
		switch {
		case ans == HolderHasFile && len(raw) > 0:
			return raw, nil
		case ans == HolderHasNone:
		default:
			unreached = true
		}
	}
	if unreached {
		return nil, ErrHoldersUnreached
	}
	return nil, ErrNoCredentials
}

// ErrPassphraseFile is the error of a passphrase file that cannot be read: a matter of this server (P7G-04). It is an ErrCredentials.
var ErrPassphraseFile = fmt.Errorf("%w (the passphrase file cannot be read)", ErrCredentials)

// ErrInstallWrite is part of the error of Install when the file was good and writing it failed (P7F-02).
var ErrInstallWrite = errors.New("ledgeranchor: the credentials file could not be written on this server")

// ErrCredentials means a credentials file cannot be read or opened. Its text names no content.
var ErrCredentials = errors.New("ledgeranchor: the credentials file cannot be read or opened")

// Creds are the credentials of a target by member name. A value is an anchor.Secret: it prints as a placeholder in
// every verb, in JSON and in text.
type Creds map[string]anchor.Secret

// Format implements fmt.Formatter.
func (Creds) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(anchor.RedactedText)) }

// MarshalJSON implements json.Marshaler.
func (Creds) MarshalJSON() ([]byte, error) { return json.Marshal(anchor.RedactedText) }

// Secrets lists the values, for a Redactor.
func (c Creds) Secrets() []anchor.Secret {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]anchor.Secret, 0, len(keys)+1)
	for _, k := range keys {
		out = append(out, c[k])
	}
	// the HTTP Basic pair is sent as the base64 of "user:password": a server that repeats the header in a text repeats that, which is
	// not a spelling of either member alone
	if u, p := c["username"], c["password"]; u != "" && p != "" {
		out = append(out, anchor.Secret(u.Reveal()+":"+p.Reveal()))
	}
	return out
}

// IDs lists the targets that have a credentials file on this server.
func (s *CredStore) IDs() ([]int64, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, ErrCredentials
	}
	var out []int64
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || len(name) < 6 || name[len(name)-5:] != ".json" {
			continue
		}
		if id, err := strconv.ParseInt(name[:len(name)-5], 10, 64); err == nil && id > 0 && strconv.FormatInt(id, 10) == name[:len(name)-5] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (c Creds) get(name string) anchor.Secret { return c[name] }

// plain is the JSON of the credentials, with the values in clear: used only to write a file.
func (c Creds) plain() ([]byte, error) {
	m := map[string]string{}
	for k, v := range c {
		m[k] = v.Reveal()
	}
	return json.Marshal(m)
}

// The members of each kind.
var credMembers = map[string][]credMember{
	anchor.KindWORM: {
		{"accessKeyId", true, 4, 128, false}, {"secretAccessKey", true, 4, 256, false}, {"sessionToken", false, 4, 4096, false},
	},
	anchor.KindTSA: {
		// at least anchor.MinSecretLength: the redactor ignores a shorter value, so a shorter one could not be removed from a text (P7A-10)
		{"username", false, anchor.MinSecretLength, 256, false}, {"password", false, anchor.MinSecretLength, 256, true},
	},
	anchor.KindOTS: {},
}

type credMember struct {
	name     string
	required bool
	min, max int
	// space allows the space character (a password).
	space bool
}

// ParseCreds validates the credentials object of a request or of a file for a kind and returns it. The error names
// the member and the rule, never a value. nil and null mean no credentials.
func ParseCreds(kind string, raw json.RawMessage) (Creds, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) {
		return nil, nil
	}
	if len(trim) > MaxCredFile {
		return nil, errors.New("the credentials are too large")
	}
	members, ok := credMembers[kind]
	if !ok {
		return nil, errors.New("unknown kind of target")
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trim))
	if err := dec.Decode(&m); err != nil || m == nil || dec.More() {
		return nil, errors.New("the credentials must be a JSON object")
	}
	allowed := map[string]credMember{}
	for _, mm := range members {
		allowed[mm.name] = mm
	}
	if len(m) == 0 {
		return nil, nil // {} removes the stored credentials
	}
	out := Creds{}
	for k, v := range m {
		mm, ok := allowed[k]
		if !ok {
			return nil, fmt.Errorf("%s: unknown credential member %s", kind, safeName(k))
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("credentials.%s must be a string", mm.name)
		}
		if len(s) < mm.min || len(s) > mm.max {
			return nil, fmt.Errorf("credentials.%s must have %d to %d characters", mm.name, mm.min, mm.max)
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c < 0x20 || c > 0x7e || (c == ' ' && !mm.space) {
				return nil, fmt.Errorf("credentials.%s may hold printable ASCII characters only (no white space or control character)", mm.name)
			}
		}
		out[k] = anchor.Secret(s)
	}
	for _, mm := range members {
		if mm.required && out[mm.name] == "" {
			return nil, fmt.Errorf("credentials.%s is required", mm.name)
		}
	}
	if kind == anchor.KindTSA && (out["username"] == "") != (out["password"] == "") {
		return nil, errors.New("credentials.username and credentials.password go together")
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// safeName is a member name that is safe to echo: short and made of letters.
func safeName(s string) string {
	if len(s) > 32 {
		return "(a name that is too long)"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return "(a name with other characters than letters)"
		}
	}
	return strconv.Quote(s)
}

// CredStore is the credentials files of this server.
type CredStore struct {
	dir      string
	passFile string
	// Iterations is the PBKDF2 count that is written (default ledgerkeys.Iterations); MinIterations the least that
	// is read. Tests lower both.
	Iterations    int
	MinIterations int
	// Rand supplies the salt and the nonce (crypto/rand when nil).
	Rand io.Reader
	// writeErr is an error that every write of a file returns (tests).
	writeErr error

	// locks serialize the changes of the file of one target (P7H-04).
	lmu   sync.Mutex
	locks map[int64]*sync.Mutex
}

// lock takes the lock of one target and returns its release.
func (s *CredStore) lock(id int64) func() {
	s.lmu.Lock()
	if s.locks == nil {
		s.locks = map[int64]*sync.Mutex{}
	}
	m := s.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	s.lmu.Unlock()
	m.Lock()
	return m.Unlock
}

// NewCredStore prepares <dir>/anchor with mode 0700; passFile is the passphrase file ("" for files in clear).
func NewCredStore(dir, passFile string) (*CredStore, error) {
	if dir == "" {
		return nil, errors.New("ledgeranchor: the key directory is empty")
	}
	s := &CredStore{dir: filepath.Join(dir, "anchor"), passFile: passFile, Iterations: ledgerkeys.Iterations, MinIterations: ledgerkeys.Iterations}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("ledgeranchor: creating the credentials directory: %w", pathError(err))
	}
	return s, nil
}

// Sealed reports whether this server writes sealed files.
func (s *CredStore) Sealed() bool { return s.passFile != "" }

func (s *CredStore) path(id int64) string {
	return filepath.Join(s.dir, strconv.FormatInt(id, 10)+".json")
}

// pathError is err without the path of the file: the operating system's text names the path, and a path is not
// secret but a credentials directory is not shown either.
func pathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (s *CredStore) passphrase() (ledgerkeys.Passphrase, error) {
	if s.passFile == "" {
		return ledgerkeys.Passphrase{}, nil
	}
	p, err := ledgerkeys.ReadPassphraseFile(s.passFile)
	if err != nil {
		return ledgerkeys.Passphrase{}, ErrPassphraseFile
	}
	return p, nil
}

func credHeader(iterations uint32, salt []byte) []byte {
	h := []byte(CredMagic + "\n" + credKDF)
	h = append(h, 0)
	h = binary.BigEndian.AppendUint32(h, iterations)
	return append(h, salt...)
}

// A credentials file holds, besides the credentials, the rotation generation and the stamp of the rotation (P7G-02): the generation is a counter
// that every change of the credentials raises, the stamp is 16 random bytes that a rotation makes once and every copy keeps. Both are inside
// the file body (inside the sealed text for a sealed file, so they are authenticated), never in the time of the file system: a file restored
// from a backup, or a server whose clock is ahead, does not look newer. A file that an earlier version wrote has neither: it is generation 0
// without a stamp.
//
//	{"credentials":{...},"generation":3,"stamp":"<32 hex>"}
const (
	// MaxCredGeneration bounds the counter that a file may carry.
	MaxCredGeneration = 1 << 40
	credStampLen      = 16
)

// credBody is the JSON object of the body of a file (the plain line, or the text that is sealed).
type credBody struct {
	Credentials json.RawMessage `json:"credentials"`
	Generation  uint64          `json:"generation"`
	Stamp       string          `json:"stamp"`
}

func validStamp(s string) bool {
	if len(s) != 2*credStampLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// NewStamp makes the stamp of a rotation.
func NewStamp() (string, error) {
	b := make([]byte, credStampLen)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", ErrCredentials
	}
	return hex.EncodeToString(b), nil
}

// Encode returns the file for the credentials of the generation gen with the stamp of its rotation.
func (s *CredStore) Encode(c Creds, gen uint64, stamp string) ([]byte, error) {
	if gen > MaxCredGeneration || (gen == 0) != (stamp == "") || stamp != "" && !validStamp(stamp) {
		return nil, ErrCredentials
	}
	members, err := c.plain()
	if err != nil {
		return nil, ErrCredentials
	}
	defer clear(members)
	plain := []byte(`{"credentials":`)
	plain = append(plain, members...)
	plain = append(plain, `,"generation":`+strconv.FormatUint(gen, 10)+`,"stamp":"`+stamp+`"}`...)
	defer clear(plain)
	pass, err := s.passphrase()
	if err != nil {
		return nil, err
	}
	defer pass.Destroy()
	if !pass.IsSet() {
		out := []byte(CredMagic + "\n")
		out = append(out, plain...)
		return append(out, '\n'), nil
	}
	rd := s.Rand
	if rd == nil {
		rd = rand.Reader
	}
	salt, nonce := make([]byte, credSalt), make([]byte, credNonce)
	if _, err := io.ReadFull(rd, salt); err != nil {
		return nil, ErrCredentials
	}
	if _, err := io.ReadFull(rd, nonce); err != nil {
		return nil, ErrCredentials
	}
	it := s.Iterations
	if it <= 0 {
		it = ledgerkeys.Iterations
	}
	aead, err := credAEAD(pass, uint32(it), salt) //nolint:gosec // a small positive count
	if err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plain, credHeader(uint32(it), salt)) //nolint:gosec // as above
	e := base64.StdEncoding.EncodeToString
	return []byte(CredMagic + "\n" + `{"kdf":"` + credKDF + `","iterations":` + strconv.Itoa(it) +
		`,"salt":"` + e(salt) + `","nonce":"` + e(nonce) + `","ciphertext":"` + e(sealed) + `"}` + "\n"), nil
}

func credAEAD(p ledgerkeys.Passphrase, iterations uint32, salt []byte) (cipher.AEAD, error) {
	key, err := p.DeriveKey(salt, int(iterations))
	if err != nil {
		return nil, ErrCredentials
	}
	defer clear(key)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrCredentials
	}
	return cipher.NewGCM(blk)
}

// Stored is what a credentials file holds.
type Stored struct {
	Creds      Creds
	Generation uint64
	// Stamp is the stamp of the rotation, empty for a file of an earlier version (generation 0).
	Stamp string
	// Plain is true when the file was in clear (not sealed).
	Plain bool
}

// Decode reads a file made by Encode (a sealed one needs the passphrase file of this server; a plain one is
// accepted whether or not this server seals).
func (s *CredStore) Decode(data []byte, kind string) (Creds, error) {
	st, err := s.DecodeFile(data, kind)
	if err != nil {
		return nil, err
	}
	return st.Creds, nil
}

// DecodeFile reads the credentials, the generation and the stamp of a file. A file that an earlier version wrote is generation 0 without a stamp.
func (s *CredStore) DecodeFile(data []byte, kind string) (Stored, error) {
	b, err := s.openBody(data)
	if err != nil {
		return Stored{}, err
	}
	defer clear(b.Members)
	c, err := ParseCreds(kind, b.Members)
	if err != nil || c == nil {
		return Stored{}, ErrCredentials
	}
	return Stored{Creds: c, Generation: b.Generation, Stamp: b.Stamp, Plain: b.Plain}, nil
}

// openedBody is the members of a file as JSON, with the generation and the stamp.
type openedBody struct {
	Members    []byte
	Generation uint64
	Stamp      string
	// Plain: the body was read from a file in clear.
	Plain bool
}

// parseBody splits the text of a file body: the new form is an object with the members of credentials, a generation and a stamp, the old form
// is the object of the members.
func parseBody(text []byte, line bool) (openedBody, error) {
	if line {
		var ok bool
		if text, ok = bytes.CutSuffix(text, []byte("\n")); !ok {
			return openedBody{}, ErrCredentials
		}
	}
	if !bytes.HasPrefix(text, []byte(`{"credentials":`)) {
		return openedBody{Members: bytes.Clone(text)}, nil // an earlier version: the members only
	}
	var cb credBody
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cb); err != nil || dec.More() || len(cb.Credentials) == 0 || cb.Generation > MaxCredGeneration || cb.Generation == 0 && cb.Stamp != "" ||
		cb.Generation > 0 && !validStamp(cb.Stamp) {
		return openedBody{}, ErrCredentials
	}
	return openedBody{Members: bytes.Clone(cb.Credentials), Generation: cb.Generation, Stamp: cb.Stamp}, nil
}

// openBody returns the body of a file, opened when it is sealed. The caller clears Members.
func (s *CredStore) openBody(data []byte) (openedBody, error) {
	rest, ok := bytes.CutPrefix(data, []byte(CredMagic+"\n"))
	if !ok || len(data) > MaxCredFile {
		return openedBody{}, ErrCredentials
	}
	if bytes.HasPrefix(rest, []byte(`{"credentials":`)) {
		if !bytes.HasSuffix(rest, []byte("}\n")) {
			return openedBody{}, ErrCredentials
		}
		b, err := parseBody(rest, true)
		b.Plain = true
		return b, err
	}
	var env struct {
		KDF        string `json:"kdf"`
		Iterations int    `json:"iterations"`
		Salt       string `json:"salt"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
	}
	dec := json.NewDecoder(bytes.NewReader(rest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil || env.KDF != credKDF || env.Iterations < s.MinIterations || env.Iterations > ledgerkeys.MaxIterations {
		return openedBody{}, ErrCredentials
	}
	salt, err1 := base64.StdEncoding.Strict().DecodeString(env.Salt)
	nonce, err2 := base64.StdEncoding.Strict().DecodeString(env.Nonce)
	ct, err3 := base64.StdEncoding.Strict().DecodeString(env.Ciphertext)
	if err1 != nil || err2 != nil || err3 != nil || len(salt) != credSalt || len(nonce) != credNonce || len(ct) > MaxCredFile {
		return openedBody{}, ErrCredentials
	}
	pass, err := s.passphrase()
	if err != nil {
		return openedBody{}, err
	}
	defer pass.Destroy()
	if !pass.IsSet() {
		return openedBody{}, ErrCredentials
	}
	aead, err := credAEAD(pass, uint32(env.Iterations), salt) //nolint:gosec // bounded above
	if err != nil {
		return openedBody{}, err
	}
	plain, err := aead.Open(nil, nonce, ct, credHeader(uint32(env.Iterations), salt)) //nolint:gosec // as above
	if err != nil {
		return openedBody{}, ErrCredentials
	}
	defer clear(plain)
	return parseBody(plain, false)
}

// CopyInfo says which credentials a server holds for a target without saying what they are. Generation is the rotation counter of the file
// (0 for a file of an earlier version). Digest is an HMAC-SHA256, keyed with the stamp of the rotation (which is in the file and is never sent
// by this call), over a fixed label, the id, the generation and the members with their values in a fixed order: two copies of one rotation
// give the same digest, and nobody who sees a digest and not the file can test guesses of a secret against it (P7G-05). It is empty for a file
// without a stamp.
type CopyInfo struct {
	ID         int64  `json:"id"`
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest,omitempty"`
}

// digestOf computes the digest of a file's content.
func digestOf(id int64, st Stored) (string, error) {
	if st.Stamp == "" {
		return "", nil
	}
	key, err := hex.DecodeString(st.Stamp)
	if err != nil {
		return "", ErrCredentials
	}
	m := map[string]string{}
	for k, v := range st.Creds {
		m[k] = v.Reveal()
	}
	canon, err := json.Marshal(m)
	if err != nil {
		return "", ErrCredentials
	}
	defer clear(canon)
	h := hmac.New(sha256.New, key)
	h.Write([]byte("kingres-anchor-credentials-digest\x00" + strconv.FormatInt(id, 10) + "\x00" + strconv.FormatUint(st.Generation, 10) + "\x00"))
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sameDigest compares two digests in constant time; an empty digest equals nothing.
func sameDigest(a, b string) bool {
	return a != "" && b != "" && hmac.Equal([]byte(a), []byte(b))
}

// Digest describes the file of a target on this server.
func (s *CredStore) Digest(id int64) (CopyInfo, error) {
	data, err := readPrivate(s.path(id), MaxCredFile)
	if errors.Is(err, os.ErrNotExist) {
		return CopyInfo{}, ErrNoCredentials
	}
	if err != nil {
		return CopyInfo{}, err
	}
	defer clear(data)
	b, err := s.openBody(data)
	if err != nil {
		return CopyInfo{}, err
	}
	defer clear(b.Members)
	var m map[string]string
	if err := json.Unmarshal(b.Members, &m); err != nil || len(m) == 0 {
		return CopyInfo{}, ErrCredentials
	}
	cr := Creds{}
	for k, v := range m {
		cr[k] = anchor.Secret(v)
	}
	gen, stamp := b.Generation, b.Stamp
	if s.Sealed() && b.Plain && gen > 0 {
		// a file in clear above generation 0 on a server that seals is not one that a rotation of this cluster made (P7H-01): it takes no part
		// in the ordering
		gen, stamp = 0, ""
	}
	d, err := digestOf(id, Stored{Creds: cr, Generation: gen, Stamp: stamp})
	if err != nil {
		return CopyInfo{}, err
	}
	return CopyInfo{ID: id, Generation: gen, Digest: d}, nil
}

// Put writes the file of a target atomically, mode 0600, replacing one that exists: a rotation. The generation is one above the generation of the
// file that is there and one above floor (the highest generation that a holder has: a leader that missed a rotation must not give a lower
// number than the others hold), and the rotation has a stamp of its own. It returns the bytes written so that the leader can hand them to the
// other holders (never a passphrase; the callee seals them again).
func (s *CredStore) Put(id int64, c Creds) ([]byte, error) { return s.Rotate(id, c, 0) }

// Rotate is Put with a floor for the generation.
func (s *CredStore) Rotate(id int64, c Creds, floor uint64) ([]byte, error) {
	defer s.lock(id)()
	gen := floor
	if cur, err := s.Digest(id); err == nil && cur.Generation > gen {
		gen = cur.Generation
	}
	if gen >= MaxCredGeneration {
		return nil, ErrGenerationExhausted
	}
	gen++
	stamp, err := NewStamp()
	if err != nil {
		return nil, err
	}
	return s.write(id, c, gen, stamp)
}

func (s *CredStore) write(id int64, c Creds, gen uint64, stamp string) ([]byte, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	data, err := s.Encode(c, gen, stamp)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(s.path(id), data); err != nil {
		clear(data)
		return nil, err
	}
	return data, nil
}

// ErrOlderGeneration means that a file was refused because the file this server has is of a higher generation, or of the same generation from
// another rotation: an older or a rival file is never written over a newer one (P7G-02).
var ErrOlderGeneration = errors.New("ledgeranchor: this server holds a newer credentials file for the target, or a different one of the same generation")

// ErrPlainFile means that a file in clear above generation 0 was refused by a server that seals its files: the generation of a plain file is not
// authenticated, and a plain file would win over every sealed one (P7H-01). It is an ErrCredentials.
var ErrPlainFile = fmt.Errorf("%w (a file in clear above generation 0 is not accepted by a server that seals its files)", ErrCredentials)

// MaxGenerationJump is how far above the generation of the file that is here an incoming generation may be: a rotation counts one at a time, so
// a jump is a damaged or foreign file (P7H-01). A holder that reports more is not believed, and its file is neither taken nor replaced.
const MaxGenerationJump = 64

// ErrGenerationJump means that a file was refused because its generation is more than MaxGenerationJump above the one that is here.
var ErrGenerationJump = fmt.Errorf("%w (its generation is far above the one of this server)", ErrCredentials)

// ErrGenerationExhausted means that the credentials cannot be changed because the generation of the file is at its bound (a damaged file):
// remove the file of the target on this server (<key directory>/anchor/<id>.json) and enter the credentials again, which writes a new one.
var ErrGenerationExhausted = errors.New("ledgeranchor: the generation of the credentials file is at its bound: remove the file of the target on this server and enter the credentials again")

// MaxLeaderJump is how far above the file that is here, or above what the leader saw at the holders when there is no file, a file may be that the
// leader hands over (P7I-03): the leader is the authenticated source of the cluster's credentials over the internal API, so a holder that missed
// more than MaxGenerationJump changes is brought up to date without the operator removing anything. A file above that is a damaged one.
const MaxLeaderJump = 1 << 20

// InstallSource says where a file comes from, which decides what Install believes of it.
type InstallSource struct {
	// Leader: the file was sent by the leader over the authenticated internal API (credentials/install). It is accepted in clear by a server that
	// seals (it is sealed here, keeping its generation and stamp: P7I-02), and it may be MaxLeaderJump above the reference.
	Leader bool
	// Known is the reference of the bound when this server has no file for the target: the highest generation that the sender saw at the holders
	// (0 when it saw none). A file that was fetched from a holder and the first file of a holder that was given one are bounded against it
	// (P7I-01).
	Known uint64
}

// Install is InstallFrom for a file that is not from the leader (a file fetched from a holder), with no reference for a first file.
func (s *CredStore) Install(id int64, kind string, data []byte) error {
	return s.InstallFrom(id, kind, data, InstallSource{})
}

// InstallFrom writes a file received from another server, with the generation and the stamp it carries: it must decode here (so a holder with
// another passphrase says so), and it is written again under this server's own sealing. A file of a lower generation than the one that is here,
// or of the same generation and another rotation, is refused with ErrOlderGeneration; a generation far above the reference is refused with
// ErrGenerationJump; a file in clear above generation 0 on a server that seals is refused with ErrPlainFile unless the leader sent it.
//
// A file that does not decode here is ErrCredentials. A failure of this server (a full or unwritable disk, a passphrase file that cannot be read)
// is ErrInstallWrite as well, so that a caller can tell a bad file from a bad server (P7F-02, P7G-04).
func (s *CredStore) InstallFrom(id int64, kind string, data []byte, src InstallSource) error {
	st, err := s.DecodeFile(data, kind)
	if err != nil {
		if errors.Is(err, ErrPassphraseFile) {
			return fmt.Errorf("%w: %w", ErrInstallWrite, err)
		}
		return err
	}
	if s.Sealed() && st.Plain && st.Generation > 0 && !src.Leader {
		return ErrPlainFile
	}
	limit := uint64(MaxGenerationJump)
	if src.Leader {
		limit = MaxLeaderJump
	}
	defer s.lock(id)()
	cur, cerr := s.Digest(id)
	if cerr == nil {
		if st.Generation < cur.Generation {
			return ErrOlderGeneration
		}
		if st.Generation > cur.Generation+limit {
			return ErrGenerationJump
		}
		if st.Generation == cur.Generation {
			d, derr := digestOf(id, st)
			if derr != nil {
				return derr
			}
			// two files of generation 0 (earlier versions) cannot be told apart: the later one is written, as before
			if (d != "" || cur.Digest != "") && !sameDigest(d, cur.Digest) {
				return ErrOlderGeneration
			}
		}
	} else if st.Generation > src.Known+limit {
		// no file here: the first file is bounded against what the sender saw at the holders, not believed at any number (P7I-01)
		return ErrGenerationJump
	}
	if _, err = s.write(id, st.Creds, st.Generation, st.Stamp); err != nil {
		return fmt.Errorf("%w: %w", ErrInstallWrite, err)
	}
	return nil
}

// SealPlainFiles writes again, under this server's sealing, the files in clear above generation 0 that it holds (P7I-02): a server that has just got
// a passphrase file holds its own files in clear, and Digest does not count a file in clear on a server that seals (a planted file must not win the
// ordering), so without this it would take itself for behind. The generation and the stamp are kept, the file is this server's own. A file above
// MaxLeaderJump is left alone (damaged, and still not counted). It returns the number of files sealed.
func (s *CredStore) SealPlainFiles() (int, error) {
	if !s.Sealed() {
		return 0, nil
	}
	ids, err := s.IDs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		unlock := s.lock(id)
		data, rerr := readPrivate(s.path(id), MaxCredFile)
		if rerr != nil {
			unlock()
			continue
		}
		b, oerr := s.openBody(data)
		clear(data)
		if oerr != nil || !b.Plain || b.Generation == 0 || b.Generation > MaxLeaderJump {
			clear(b.Members)
			unlock()
			continue
		}
		var m map[string]string
		if json.Unmarshal(b.Members, &m) != nil {
			clear(b.Members)
			unlock()
			continue
		}
		cr := Creds{}
		for k, v := range m {
			cr[k] = anchor.Secret(v)
		}
		clear(b.Members)
		if _, werr := s.write(id, cr, b.Generation, b.Stamp); werr != nil {
			unlock()
			return n, werr
		}
		n++
		unlock()
	}
	return n, nil
}

// Raw returns the bytes of the file of a target, for the leader that distributes it (ErrNoCredentials when there
// is none). The caller clears the result.
func (s *CredStore) Raw(id int64) ([]byte, error) {
	data, err := readPrivate(s.path(id), MaxCredFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredentials
	}
	return data, err
}

// Get reads the credentials of a target.
func (s *CredStore) Get(id int64, kind string) (Creds, error) {
	data, err := s.Raw(id)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	return s.Decode(data, kind)
}

// Has reports whether a file for the target exists.
func (s *CredStore) Has(id int64) bool {
	st, err := os.Lstat(s.path(id))
	return err == nil && st.Mode().IsRegular()
}

// Remove deletes the file of a target (no error when there is none).
func (s *CredStore) Remove(id int64) error {
	err := os.Remove(s.path(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrCredentials
	}
	return nil
}

// ReadCredentialsFile reads the operator's own file named by credentialsPath: a regular file, not a link, at most
// 4 KiB, with no access for group or others on systems that have modes, holding a JSON object of the members of the
// kind.
func ReadCredentialsFile(path, kind string) (Creds, error) {
	data, err := readPrivate(path, 4096)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	c, err := ParseCreds(kind, data)
	if err != nil || c == nil {
		return nil, errCredContent
	}
	return c, nil
}

// The three reasons a credentials file named by credentialsPath is refused, told apart so that the operator is told which (P7A-12). Each
// is an ErrCredentials; none names a path or a content.
var (
	errCredMode    = fmt.Errorf("%w (the file may be read by group or others)", ErrCredentials)
	errCredContent = fmt.Errorf("%w (the content is not a JSON object with the members of the kind)", ErrCredentials)
)

// credFileProblem says in words why ReadCredentialsFile refused: no path, no content.
func credFileProblem(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "the file does not exist on this server"
	case errors.Is(err, errCredMode):
		return "the file can be read by group or others (give it the mode 0600)"
	case errors.Is(err, errCredContent):
		return "the content is not a JSON object with the members of the kind"
	}
	return "the file is not a regular file of at most 4 KiB that this server can read"
}

// readPrivate reads a regular file that only its owner can read (on a system with modes), without following a
// link, and refuses more than limit bytes. Errors name no path and no content.
func readPrivate(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, ErrCredentials
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, ErrCredentials
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, errCredMode
	}
	f, err := os.Open(path) // #nosec G304 -- the path is configured by the operator or built from a number
	if err != nil {
		return nil, ErrCredentials
	}
	defer func() { _ = f.Close() }()
	if st2, err := f.Stat(); err != nil || !os.SameFile(st, st2) {
		return nil, ErrCredentials
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		clear(data)
		return nil, ErrCredentials
	}
	return data, nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cred-*")
	if err != nil {
		return ErrCredentials
	}
	name := tmp.Name()
	fail := func() error { _ = tmp.Close(); _ = os.Remove(name); return ErrCredentials }
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			return fail()
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail()
	}
	if err := tmp.Sync(); err != nil {
		return fail()
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return ErrCredentials
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return ErrCredentials
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil { // #nosec G304 -- the credentials directory
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}
