// SPDX-License-Identifier: PostgreSQL

// Package scram computes PostgreSQL SCRAM-SHA-256 password verifiers. A role
// is given its password as a verifier, so the clear text never appears in a
// statement that PostgreSQL could write to its log (log_statement) or to
// pg_stat_activity.
package scram

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/text/secure/precis"
)

// Iterations is PostgreSQL's default scram_iterations.
const Iterations = 4096

// MaxPasswordBytes bounds the accepted password length.
const MaxPasswordBytes = 1024

// Verifier returns the verifier string for password with a fresh random salt.
func Verifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("scram: random salt: %w", err)
	}
	return VerifierWithSalt(password, salt, Iterations)
}

// VerifierWithSalt is Verifier with a given salt and iteration count.
func VerifierWithSalt(password string, salt []byte, iter int) (string, error) {
	if password == "" || len(password) > MaxPasswordBytes || strings.ContainsRune(password, 0) {
		return "", fmt.Errorf("scram: the password must be 1 to %d bytes without NUL", MaxPasswordBytes)
	}
	// PostgreSQL applies SASLprep and falls back to the raw string when the
	// password is not valid for it.
	pw := password
	if p, err := precis.OpaqueString.String(password); err == nil {
		pw = p
	}
	salted, err := pbkdf2.Key(sha256.New, pw, salt, iter, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("scram: deriving key: %w", err)
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := mac(salted, "Server Key")
	enc := base64.StdEncoding
	return "SCRAM-SHA-256$" + strconv.Itoa(iter) + ":" + enc.EncodeToString(salt) + "$" +
		enc.EncodeToString(stored[:]) + ":" + enc.EncodeToString(serverKey), nil
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// Parsed is a verifier taken apart: what PostgreSQL stores in pg_authid.
type Parsed struct {
	Iterations int
	Salt       []byte
	// StoredKey is SHA-256(ClientKey): enough to check a client's proof, not to make one.
	StoredKey []byte
	ServerKey []byte
}

// ErrNotSCRAM means the stored password is not a SCRAM-SHA-256 verifier (an
// MD5 hash, or no password at all).
var ErrNotSCRAM = fmt.Errorf("scram: not a SCRAM-SHA-256 verifier")

// Parse takes a verifier "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>" apart.
func Parse(verifier string) (Parsed, error) {
	rest, ok := strings.CutPrefix(verifier, "SCRAM-SHA-256$")
	if !ok {
		return Parsed{}, ErrNotSCRAM
	}
	head, keys, ok := strings.Cut(rest, "$")
	if !ok {
		return Parsed{}, ErrNotSCRAM
	}
	iter, salt, ok := strings.Cut(head, ":")
	stored, server, ok2 := strings.Cut(keys, ":")
	if !ok || !ok2 {
		return Parsed{}, ErrNotSCRAM
	}
	var p Parsed
	var err error
	if p.Iterations, err = strconv.Atoi(iter); err != nil || p.Iterations < 1 || p.Iterations > 1<<24 {
		return Parsed{}, ErrNotSCRAM
	}
	enc := base64.StdEncoding
	if p.Salt, err = enc.DecodeString(salt); err != nil || len(p.Salt) == 0 {
		return Parsed{}, ErrNotSCRAM
	}
	if p.StoredKey, err = enc.DecodeString(stored); err != nil || len(p.StoredKey) != sha256.Size {
		return Parsed{}, ErrNotSCRAM
	}
	if p.ServerKey, err = enc.DecodeString(server); err != nil || len(p.ServerKey) != sha256.Size {
		return Parsed{}, ErrNotSCRAM
	}
	return p, nil
}

// ClientKey derives the SCRAM ClientKey of password under the salt and
// iteration count of the verifier. It is what a client proves it knows; only
// somebody who has seen the password (or this key) can compute it.
func (p Parsed) ClientKey(password string) ([]byte, error) {
	if password == "" || len(password) > MaxPasswordBytes || strings.ContainsRune(password, 0) {
		return nil, fmt.Errorf("scram: the password must be 1 to %d bytes without NUL", MaxPasswordBytes)
	}
	pw := password
	if s, err := precis.OpaqueString.String(password); err == nil {
		pw = s
	}
	salted, err := pbkdf2.Key(sha256.New, pw, p.Salt, p.Iterations, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("scram: deriving key: %w", err)
	}
	return mac(salted, "Client Key"), nil
}

// Stored returns SHA-256(clientKey), the value a verifier keeps.
func Stored(clientKey []byte) []byte {
	s := sha256.Sum256(clientKey)
	return s[:]
}
