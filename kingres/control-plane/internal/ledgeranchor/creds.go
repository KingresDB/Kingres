// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"

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
	out := make([]anchor.Secret, 0, len(keys))
	for _, k := range keys {
		out = append(out, c[k])
	}
	return out
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
		{"username", false, 1, 256, false}, {"password", false, 1, 256, true},
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
		return ledgerkeys.Passphrase{}, ErrCredentials
	}
	return p, nil
}

func credHeader(iterations uint32, salt []byte) []byte {
	h := []byte(CredMagic + "\n" + credKDF)
	h = append(h, 0)
	h = binary.BigEndian.AppendUint32(h, iterations)
	return append(h, salt...)
}

// Encode returns the file for the credentials.
func (s *CredStore) Encode(c Creds) ([]byte, error) {
	plain, err := c.plain()
	if err != nil {
		return nil, ErrCredentials
	}
	defer clear(plain)
	pass, err := s.passphrase()
	if err != nil {
		return nil, err
	}
	defer pass.Destroy()
	if !pass.IsSet() {
		out := []byte(CredMagic + "\n" + `{"credentials":`)
		out = append(out, plain...)
		return append(out, '}', '\n'), nil
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

// Decode reads a file made by Encode (a sealed one needs the passphrase file of this server; a plain one is
// accepted whether or not this server seals).
func (s *CredStore) Decode(data []byte, kind string) (Creds, error) {
	rest, ok := bytes.CutPrefix(data, []byte(CredMagic+"\n"))
	if !ok || len(data) > MaxCredFile {
		return nil, ErrCredentials
	}
	if plain, ok := bytes.CutPrefix(rest, []byte(`{"credentials":`)); ok {
		body, ok := bytes.CutSuffix(plain, []byte("}\n"))
		if !ok {
			return nil, ErrCredentials
		}
		c, err := ParseCreds(kind, body)
		if err != nil || c == nil {
			return nil, ErrCredentials
		}
		return c, nil
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
		return nil, ErrCredentials
	}
	salt, err1 := base64.StdEncoding.Strict().DecodeString(env.Salt)
	nonce, err2 := base64.StdEncoding.Strict().DecodeString(env.Nonce)
	ct, err3 := base64.StdEncoding.Strict().DecodeString(env.Ciphertext)
	if err1 != nil || err2 != nil || err3 != nil || len(salt) != credSalt || len(nonce) != credNonce || len(ct) > MaxCredFile {
		return nil, ErrCredentials
	}
	pass, err := s.passphrase()
	if err != nil {
		return nil, err
	}
	defer pass.Destroy()
	if !pass.IsSet() {
		return nil, ErrCredentials
	}
	aead, err := credAEAD(pass, uint32(env.Iterations), salt) //nolint:gosec // bounded above
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, credHeader(uint32(env.Iterations), salt)) //nolint:gosec // as above
	if err != nil {
		return nil, ErrCredentials
	}
	defer clear(plain)
	c, err := ParseCreds(kind, plain)
	if err != nil || c == nil {
		return nil, ErrCredentials
	}
	return c, nil
}

// Put writes the file of a target atomically, mode 0600, replacing one that exists. It returns the bytes written so
// that the leader can hand them to the other holders (never a passphrase; the callee seals them again).
func (s *CredStore) Put(id int64, c Creds) ([]byte, error) {
	data, err := s.Encode(c)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(s.path(id), data); err != nil {
		clear(data)
		return nil, err
	}
	return data, nil
}

// Install writes a file received from the leader: it must decode here (so a holder with another passphrase says
// so), and it is written again under this server's own sealing.
func (s *CredStore) Install(id int64, kind string, data []byte) error {
	c, err := s.Decode(data, kind)
	if err != nil {
		return err
	}
	_, err = s.Put(id, c)
	return err
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
		return nil, ErrCredentials
	}
	return c, nil
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
		return nil, fmt.Errorf("%w (the file may be read by group or others)", ErrCredentials)
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
