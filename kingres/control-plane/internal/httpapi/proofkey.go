// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// proofKey derives the key a new session proves itself to other servers with
// (package fwdproof). It returns nil when that is not possible: the role has no
// SCRAM-SHA-256 verifier, or the authenticator cannot derive one. Sign-in still
// works then; the session just cannot be forwarded to the leader.
func (s *Server) proofKey(ctx context.Context, username, password string) []byte {
	k, ok := s.deps.Authenticator.(auth.ProofKeyer)
	if !ok {
		return nil
	}
	key, err := k.ProofKey(ctx, username, password)
	if err != nil {
		if errors.Is(err, scram.ErrNotSCRAM) {
			s.log.Warn("the role has no SCRAM-SHA-256 password; its sessions cannot be forwarded to the manager leader")
		} else {
			s.log.Error("deriving the proof key of a session", "error", err)
		}
		return nil
	}
	return key
}
