// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// ProofKeyer derives the key a session uses to prove to another server that
// it was opened with the role's password (package fwdproof). Postgres
// implements it; an Authenticator that does not has sessions that cannot be
// forwarded.
type ProofKeyer interface {
	// ProofKey returns the SCRAM ClientKey of password under the role's
	// stored verifier. It is called once, at sign-in, after the password was
	// accepted; scram.ErrNotSCRAM (wrapped) means the role has no SCRAM-SHA-256
	// verifier.
	ProofKey(ctx context.Context, username, password string) ([]byte, error)
}

const verifierSQL = `SELECT rolpassword FROM pg_catalog.pg_authid WHERE rolname = $1 AND rolcanlogin`

// SetVerifierSource makes the authenticator read stored verifiers through f
// instead of through its own connection (the managed instance's service
// connection is the one that works right after a join). f returns
// scram.ErrNotSCRAM for a role without a usable verifier.
func (p *Postgres) SetVerifierSource(f func(ctx context.Context, username string) (string, error)) {
	p.mu.Lock()
	p.verifierSource = f
	p.mu.Unlock()
}

// verifier reads the stored password verifier of a role. A role that does not
// exist, cannot log in or has no password yields scram.ErrNotSCRAM: for the
// caller they are all "no proof possible".
func (p *Postgres) verifier(ctx context.Context, username string) (scram.Parsed, error) {
	p.mu.RLock()
	source := p.verifierSource
	p.mu.RUnlock()
	if source != nil {
		stored, err := source(ctx, username)
		if err != nil {
			if errors.Is(err, scram.ErrNotSCRAM) {
				return scram.Parsed{}, err
			}
			return scram.Parsed{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return scram.Parse(stored)
	}
	cfg := p.config()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = "kingresd-session"
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		return scram.Parsed{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var stored *string
	switch err := conn.QueryRow(cctx, verifierSQL, username).Scan(&stored); {
	case errors.Is(err, pgx.ErrNoRows):
		return scram.Parsed{}, scram.ErrNotSCRAM
	case err != nil:
		return scram.Parsed{}, fmt.Errorf("%w: reading the verifier: %w", ErrUnavailable, err)
	case stored == nil:
		return scram.Parsed{}, scram.ErrNotSCRAM
	}
	return scram.Parse(*stored)
}

// StoredKey implements fwdproof.KeySource.
func (p *Postgres) StoredKey(ctx context.Context, username string) ([]byte, error) {
	v, err := p.verifier(ctx, username)
	return v.StoredKey, err
}

// ProofKey implements ProofKeyer.
func (p *Postgres) ProofKey(ctx context.Context, username, password string) ([]byte, error) {
	v, err := p.verifier(ctx, username)
	if err != nil {
		return nil, err
	}
	return v.ClientKey(password)
}

// AttachProofKey stores the proof key in the session of a credential. The key
// lives in memory with the session, like the session itself.
func (s *Store) AttachProofKey(credential string, key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[digest(credential)]; ok {
		sess.ProofKey = append([]byte(nil), key...)
	}
}
