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
